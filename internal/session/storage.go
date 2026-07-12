package session

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

func SessionsDir(globalDir string) string {
	return filepath.Join(globalDir, "sessions")
}

func SessionDir(globalDir, id string) string {
	return filepath.Join(SessionsDir(globalDir), id)
}

func Save(globalDir string, state State) error {
	if state.Metadata.ID == "" {
		return fmt.Errorf("session id is required")
	}
	dir := SessionDir(globalDir, state.Metadata.ID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if state.Metadata.StartedAt.IsZero() {
		state.Metadata.StartedAt = time.Now()
	}
	state.Metadata.UpdatedAt = time.Now()
	// Persist the preview so List can render the resume picker from metadata
	// alone, without loading every session's messages.
	if p := firstUserPreview(state.Messages); p != "" {
		state.Metadata.Preview = p
	}
	if err := writeJSON(filepath.Join(dir, metadataFilename), state.Metadata); err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(dir, messagesFilename), state.Messages); err != nil {
		return err
	}
	// The task files are owned exclusively by UpsertTodo/SaveTodos, which
	// write them as tasks change mid-run. Save must never touch them: callers
	// pass an in-memory snapshot (TUI m.todos, ACP transcript state) that can
	// be stale or empty while tools are writing concurrently, and rewriting
	// the files from that snapshot silently discards newer tasks.
	// The agents event log is append-only and owned by AppendEvent. Only rewrite
	// it when the caller actually supplies events; otherwise a routine Save
	// (which carries no events) would truncate the log written during the run.
	if len(state.Events) > 0 {
		return rewriteEvents(filepath.Join(dir, agentsFilename), state.Events)
	}
	return nil
}

// LoadMetadata reads only a session's metadata file. It is much cheaper than
// Load for listing sessions, which only needs the summary fields.
func LoadMetadata(globalDir, sessionID string) (Metadata, error) {
	var meta Metadata
	if err := readJSON(filepath.Join(SessionDir(globalDir, sessionID), metadataFilename), &meta); err != nil {
		return Metadata{}, err
	}
	return meta, nil
}

func AppendEvent(globalDir, sessionID string, event AgentEvent) error {
	if sessionID == "" {
		return nil
	}
	dir := SessionDir(globalDir, sessionID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if event.At.IsZero() {
		event.At = time.Now()
	}
	f, err := os.OpenFile(filepath.Join(dir, agentsFilename), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	raw, err := json.Marshal(event)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(raw, '\n')); err != nil {
		return err
	}
	return nil
}

func Load(globalDir, sessionID string) (State, error) {
	dir := SessionDir(globalDir, sessionID)
	metaPath := filepath.Join(dir, metadataFilename)
	msgPath := filepath.Join(dir, messagesFilename)
	todoPath := filepath.Join(dir, todosFilename)
	taskPath := filepath.Join(dir, tasksFilename)
	agentPath := filepath.Join(dir, agentsFilename)

	var meta Metadata
	if err := readJSON(metaPath, &meta); err != nil {
		return State{}, err
	}
	var messages []Message
	if err := readJSON(msgPath, &messages); err != nil {
		return State{}, err
	}
	var todos []Todo
	if err := readJSONIfExists(todoPath, &todos); err != nil {
		return State{}, err
	}
	var tasks []Todo
	if err := readJSONIfExists(taskPath, &tasks); err != nil {
		return State{}, err
	}
	if len(tasks) == 0 {
		tasks = todos
	}
	events, err := readEvents(agentPath)
	if err != nil {
		return State{}, err
	}
	return State{Metadata: meta, Messages: messages, Todos: tasks, Tasks: tasks, Events: events}, nil
}

func LoadTodos(globalDir, sessionID string) ([]Todo, error) {
	if sessionID == "" {
		return nil, nil
	}
	var todos []Todo
	taskPath := filepath.Join(SessionDir(globalDir, sessionID), tasksFilename)
	err := readJSONIfExists(taskPath, &todos)
	if err != nil {
		return nil, err
	}
	if len(todos) == 0 {
		err = readJSONIfExists(filepath.Join(SessionDir(globalDir, sessionID), todosFilename), &todos)
	}
	return todos, err
}

func SaveTodos(globalDir, sessionID string, todos []Todo) error {
	todoMu.Lock()
	defer todoMu.Unlock()
	return saveTodosLocked(globalDir, sessionID, todos)
}

// saveTodosLocked writes the task files; the caller must hold todoMu.
func saveTodosLocked(globalDir, sessionID string, todos []Todo) error {
	if sessionID == "" {
		return fmt.Errorf("session id is required")
	}
	dir := SessionDir(globalDir, sessionID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(dir, tasksFilename), todos); err != nil {
		return err
	}
	return writeJSON(filepath.Join(dir, todosFilename), todos)
}

// todoMu serializes read-modify-write cycles on the task store so parallel
// task tool calls in one agent step do not overwrite each other's tasks.
var todoMu sync.Mutex

func UpsertTodo(globalDir, sessionID string, t Todo) (Todo, error) {
	if sessionID == "" {
		return Todo{}, fmt.Errorf("session id is required")
	}
	t.ID = strings.TrimSpace(t.ID)
	t.Content = strings.TrimSpace(t.Content)
	if t.Content == "" {
		return Todo{}, fmt.Errorf("todo content is required")
	}
	if strings.TrimSpace(t.Status) == "" {
		t.Status = "pending"
	}
	t.Priority = strings.TrimSpace(t.Priority)
	if t.Priority == "" {
		t.Priority = "normal"
	}
	t.Dependencies = compactDependencies(t.Dependencies)
	now := time.Now()
	t.UpdatedAt = now
	// Load, validate, and save under one lock: parallel tool calls otherwise
	// each read the same base list and the last writer silently drops the
	// others' tasks.
	todoMu.Lock()
	defer todoMu.Unlock()
	todos, err := LoadTodos(globalDir, sessionID)
	if err != nil {
		return Todo{}, err
	}
	// An empty ID means "new task": mint one here, against the stored list and
	// under the lock. Callers used to derive IDs from the wall clock, and two
	// creates in the same millisecond collided — the second silently replaced
	// the first instead of appending.
	if t.ID == "" {
		t.ID = nextTaskID(todos)
	}
	replaced := false
	for i := range todos {
		if todos[i].ID == t.ID {
			if t.CreatedAt.IsZero() {
				t.CreatedAt = todos[i].CreatedAt
			}
			todos[i] = t
			replaced = true
			break
		}
	}
	if !replaced {
		if t.CreatedAt.IsZero() {
			t.CreatedAt = now
		}
		todos = append(todos, t)
	}
	// Graph invariants are enforced here, on the merged list, so every writer
	// (task tools, /tasks command) sees the same rules atomically with the
	// state it validated against.
	if err := ValidateTaskGraph(todos); err != nil {
		return Todo{}, err
	}
	if t.Status == TaskStatusInProgress || t.Status == TaskStatusCompleted {
		if gating := IncompleteDeps(t, todos); len(gating) > 0 {
			return Todo{}, fmt.Errorf("task %q cannot be %s: unmet dependencies: %s", t.ID, t.Status, strings.Join(gating, ", "))
		}
	}
	if err := saveTodosLocked(globalDir, sessionID, todos); err != nil {
		return Todo{}, err
	}
	return t, nil
}

// DeleteTodo removes a task by ID and strips it from every other task's
// dependency list (a deleted task no longer gates anything). It reports
// whether the task existed.
func DeleteTodo(globalDir, sessionID, id string) (bool, error) {
	if sessionID == "" {
		return false, fmt.Errorf("session id is required")
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return false, fmt.Errorf("todo id is required")
	}
	todoMu.Lock()
	defer todoMu.Unlock()
	todos, err := LoadTodos(globalDir, sessionID)
	if err != nil {
		return false, err
	}
	out := make([]Todo, 0, len(todos))
	found := false
	for _, t := range todos {
		if t.ID == id {
			found = true
			continue
		}
		deps := t.Dependencies[:0:0]
		for _, dep := range t.Dependencies {
			if dep != id {
				deps = append(deps, dep)
			}
		}
		t.Dependencies = deps
		out = append(out, t)
	}
	if !found {
		return false, nil
	}
	return true, saveTodosLocked(globalDir, sessionID, out)
}

// ClearCompletedTodos removes every completed and cancelled task, keeping the
// active part of the graph. Dependencies on removed tasks are stripped (they
// were already satisfied). Returns the number of tasks removed.
func ClearCompletedTodos(globalDir, sessionID string) (int, error) {
	if sessionID == "" {
		return 0, fmt.Errorf("session id is required")
	}
	todoMu.Lock()
	defer todoMu.Unlock()
	todos, err := LoadTodos(globalDir, sessionID)
	if err != nil {
		return 0, err
	}
	removed := map[string]struct{}{}
	kept := make([]Todo, 0, len(todos))
	for _, t := range todos {
		if taskDone(t.Status) {
			removed[t.ID] = struct{}{}
			continue
		}
		kept = append(kept, t)
	}
	if len(removed) == 0 {
		return 0, nil
	}
	for i := range kept {
		deps := kept[i].Dependencies[:0:0]
		for _, dep := range kept[i].Dependencies {
			if _, gone := removed[dep]; !gone {
				deps = append(deps, dep)
			}
		}
		kept[i].Dependencies = deps
	}
	return len(removed), saveTodosLocked(globalDir, sessionID, kept)
}

// nextTaskID returns the smallest unused "task-N" identifier.
func nextTaskID(todos []Todo) string {
	used := make(map[string]struct{}, len(todos))
	for _, t := range todos {
		used[t.ID] = struct{}{}
	}
	for n := 1; ; n++ {
		id := fmt.Sprintf("task-%d", n)
		if _, ok := used[id]; !ok {
			return id
		}
	}
}

func GetTodo(globalDir, sessionID, id string) (Todo, bool, error) {
	todos, err := LoadTodos(globalDir, sessionID)
	if err != nil {
		return Todo{}, false, err
	}
	for _, t := range todos {
		if t.ID == id {
			return t, true, nil
		}
	}
	return Todo{}, false, nil
}

func compactDependencies(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, 0, len(in))
	seen := map[string]struct{}{}
	for _, dep := range in {
		dep = strings.TrimSpace(dep)
		if dep == "" {
			continue
		}
		if _, ok := seen[dep]; ok {
			continue
		}
		seen[dep] = struct{}{}
		out = append(out, dep)
	}
	return out
}

func readEvents(path string) ([]AgentEvent, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return nil, nil
	}
	out := make([]AgentEvent, 0, len(lines))
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var ev AgentEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			return nil, err
		}
		out = append(out, ev)
	}
	return out, nil
}

func rewriteEvents(path string, events []AgentEvent) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	for _, event := range events {
		raw, err := json.Marshal(event)
		if err != nil {
			return err
		}
		if _, err := f.Write(append(raw, '\n')); err != nil {
			return err
		}
	}
	return nil
}

// writeJSON writes atomically (temp file + rename): session files are read
// concurrently (task tools, TUI sync, ACP) while turns write them, and a
// plain WriteFile lets readers observe a torn, half-written file ("unexpected
// end of JSON input").
func writeJSON(path string, value any) error {
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func readJSON(path string, target any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, target)
}

func readJSONIfExists(path string, target any) error {
	if err := readJSON(path, target); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	return nil
}
