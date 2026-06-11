package session

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
	tasks := state.Tasks
	if len(tasks) == 0 {
		tasks = state.Todos
	}
	if err := writeJSON(filepath.Join(dir, tasksFilename), tasks); err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(dir, todosFilename), tasks); err != nil {
		return err
	}
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

func UpsertTodo(globalDir, sessionID string, t Todo) (Todo, error) {
	if sessionID == "" {
		return Todo{}, fmt.Errorf("session id is required")
	}
	t.ID = strings.TrimSpace(t.ID)
	if t.ID == "" {
		return Todo{}, fmt.Errorf("todo id is required")
	}
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
	todos, err := LoadTodos(globalDir, sessionID)
	if err != nil {
		return Todo{}, err
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
	if err := SaveTodos(globalDir, sessionID, todos); err != nil {
		return Todo{}, err
	}
	return t, nil
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

func writeJSON(path string, value any) error {
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0o644)
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
