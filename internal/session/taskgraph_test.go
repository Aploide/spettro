package session

import (
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestNormalizeTaskStatus(t *testing.T) {
	cases := map[string]string{
		"":            TaskStatusPending,
		"pending":     TaskStatusPending,
		"todo":        TaskStatusPending,
		"IN_PROGRESS": TaskStatusInProgress,
		"in-progress": TaskStatusInProgress,
		"done":        TaskStatusCompleted,
		"completed":   TaskStatusCompleted,
		"blocked":     TaskStatusBlocked,
		"canceled":    TaskStatusCancelled,
	}
	for in, want := range cases {
		got, err := NormalizeTaskStatus(in)
		if err != nil || got != want {
			t.Fatalf("NormalizeTaskStatus(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := NormalizeTaskStatus("bogus"); err == nil {
		t.Fatalf("expected error for invalid status")
	}
}

func TestValidateTaskGraph(t *testing.T) {
	ok := []Todo{
		{ID: "a", Content: "a"},
		{ID: "b", Content: "b", Dependencies: []string{"a"}},
		{ID: "c", Content: "c", Dependencies: []string{"a", "b"}},
	}
	if err := ValidateTaskGraph(ok); err != nil {
		t.Fatalf("valid graph rejected: %v", err)
	}
	if err := ValidateTaskGraph([]Todo{{ID: "a"}, {ID: "a"}}); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("expected duplicate id error, got %v", err)
	}
	if err := ValidateTaskGraph([]Todo{{ID: "a", Dependencies: []string{"a"}}}); err == nil || !strings.Contains(err.Error(), "itself") {
		t.Fatalf("expected self-dependency error, got %v", err)
	}
	if err := ValidateTaskGraph([]Todo{{ID: "a", Dependencies: []string{"ghost"}}}); err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Fatalf("expected unknown dep error, got %v", err)
	}
	cyc := []Todo{
		{ID: "a", Dependencies: []string{"c"}},
		{ID: "b", Dependencies: []string{"a"}},
		{ID: "c", Dependencies: []string{"b"}},
	}
	if err := ValidateTaskGraph(cyc); err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("expected cycle error, got %v", err)
	}
}

func TestBlockedAndReadyDerivation(t *testing.T) {
	todos := []Todo{
		{ID: "a", Status: TaskStatusCompleted},
		{ID: "b", Status: TaskStatusPending, Dependencies: []string{"a"}},
		{ID: "c", Status: TaskStatusPending, Dependencies: []string{"b"}},
		{ID: "d", Status: TaskStatusPending, Dependencies: []string{"x"}}, // unknown dep gates too
		{ID: "e", Status: TaskStatusCancelled, Dependencies: []string{"c"}},
	}
	blocked := BlockedIDs(todos)
	if _, ok := blocked["c"]; !ok {
		t.Fatalf("c should be blocked (b incomplete)")
	}
	if _, ok := blocked["d"]; !ok {
		t.Fatalf("d should be blocked (unknown dep)")
	}
	if _, ok := blocked["b"]; ok {
		t.Fatalf("b should not be blocked (a completed)")
	}
	if _, ok := blocked["e"]; ok {
		t.Fatalf("cancelled task should not be reported blocked")
	}
	ready := ReadyTasks(todos)
	if len(ready) != 1 || ready[0].ID != "b" {
		t.Fatalf("expected only b ready, got %#v", ready)
	}
	if deps := IncompleteDeps(todos[2], todos); !reflect.DeepEqual(deps, []string{"b"}) {
		t.Fatalf("unexpected incomplete deps for c: %#v", deps)
	}
}

func TestTopoOrder(t *testing.T) {
	todos := []Todo{
		{ID: "c", Dependencies: []string{"a", "b"}},
		{ID: "b", Dependencies: []string{"a"}},
		{ID: "a"},
	}
	got := TopoOrder(todos)
	if !reflect.DeepEqual(got, []string{"a", "b", "c"}) {
		t.Fatalf("unexpected topo order: %#v", got)
	}
	// A cycle falls back to stored order rather than dropping tasks.
	cyc := []Todo{{ID: "a", Dependencies: []string{"b"}}, {ID: "b", Dependencies: []string{"a"}}}
	if got := TopoOrder(cyc); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatalf("unexpected cycle fallback order: %#v", got)
	}
}

func TestSaveWithoutTasksDoesNotWipeTaskStore(t *testing.T) {
	globalDir := t.TempDir()
	sid := "sess-1"
	if _, err := UpsertTodo(globalDir, sid, Todo{ID: "a", Content: "first"}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	// A transcript-only snapshot (like the ACP bridge persists after every
	// turn) must not clobber tasks written during the run.
	err := Save(globalDir, State{
		Metadata: Metadata{ID: sid, ProjectPath: "/p"},
		Messages: []Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	todos, err := LoadTodos(globalDir, sid)
	if err != nil {
		t.Fatalf("load todos: %v", err)
	}
	if len(todos) != 1 || todos[0].ID != "a" {
		t.Fatalf("task store wiped by transcript-only save: %#v", todos)
	}
}

func TestConcurrentUpsertTodoKeepsAllTasks(t *testing.T) {
	globalDir := t.TempDir()
	sid := "sess-1"
	const n = 16
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := UpsertTodo(globalDir, sid, Todo{ID: fmt.Sprintf("t%02d", i), Content: "task"})
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent upsert: %v", err)
		}
	}
	todos, err := LoadTodos(globalDir, sid)
	if err != nil {
		t.Fatalf("load todos: %v", err)
	}
	if len(todos) != n {
		t.Fatalf("lost tasks under concurrency: got %d, want %d", len(todos), n)
	}
}

func TestUpsertTodoMintsUniqueIDs(t *testing.T) {
	globalDir := t.TempDir()
	sid := "sess-1"
	// Rapid creates without explicit IDs must never collide (the old
	// wall-clock IDs did within one millisecond).
	for i := 0; i < 5; i++ {
		if _, err := UpsertTodo(globalDir, sid, Todo{Content: fmt.Sprintf("task %d", i)}); err != nil {
			t.Fatalf("upsert %d: %v", i, err)
		}
	}
	todos, err := LoadTodos(globalDir, sid)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(todos) != 5 {
		t.Fatalf("expected 5 tasks, got %d: %#v", len(todos), todos)
	}
	seen := map[string]struct{}{}
	for _, td := range todos {
		if _, dup := seen[td.ID]; dup {
			t.Fatalf("duplicate minted id %q", td.ID)
		}
		seen[td.ID] = struct{}{}
	}
	// Minting skips IDs already taken explicitly.
	if _, err := UpsertTodo(globalDir, sid, Todo{ID: "task-6", Content: "explicit"}); err != nil {
		t.Fatalf("explicit upsert: %v", err)
	}
	minted, err := UpsertTodo(globalDir, sid, Todo{Content: "after explicit"})
	if err != nil {
		t.Fatalf("minted upsert: %v", err)
	}
	if minted.ID != "task-7" {
		t.Fatalf("expected task-7, got %q", minted.ID)
	}
}

func TestConcurrentReadersNeverSeeTornTaskFile(t *testing.T) {
	globalDir := t.TempDir()
	sid := "sess-1"
	if _, err := UpsertTodo(globalDir, sid, Todo{ID: "seed", Content: "seed"}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-done:
				return
			default:
			}
			batch := []Todo{{ID: "seed", Content: fmt.Sprintf("rewrite %d with some padding to make the file larger", i), Status: TaskStatusPending, UpdatedAt: time.Now()}}
			if err := SaveTodos(globalDir, sid, batch); err != nil {
				t.Errorf("save: %v", err)
				return
			}
		}
	}()
	// Readers hammer the file while it is rewritten; with non-atomic writes
	// they used to observe truncated JSON ("unexpected end of JSON input").
	for i := 0; i < 500; i++ {
		if _, err := LoadTodos(globalDir, sid); err != nil {
			close(done)
			wg.Wait()
			t.Fatalf("torn read after %d iterations: %v", i, err)
		}
	}
	close(done)
	wg.Wait()
}

func TestDeleteTodoStripsDependencyReferences(t *testing.T) {
	globalDir := t.TempDir()
	sid := "sess-1"
	if _, err := UpsertTodo(globalDir, sid, Todo{ID: "a", Content: "a", Status: TaskStatusCompleted}); err != nil {
		t.Fatalf("seed a: %v", err)
	}
	if _, err := UpsertTodo(globalDir, sid, Todo{ID: "b", Content: "b", Dependencies: []string{"a"}}); err != nil {
		t.Fatalf("seed b: %v", err)
	}
	found, err := DeleteTodo(globalDir, sid, "a")
	if err != nil || !found {
		t.Fatalf("delete a: found=%v err=%v", found, err)
	}
	todos, err := LoadTodos(globalDir, sid)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(todos) != 1 || todos[0].ID != "b" {
		t.Fatalf("unexpected remaining tasks: %#v", todos)
	}
	if len(todos[0].Dependencies) != 0 {
		t.Fatalf("dangling dependency left behind: %#v", todos[0].Dependencies)
	}
	// The survivor's graph must still validate (no dangling references).
	if err := ValidateTaskGraph(todos); err != nil {
		t.Fatalf("graph invalid after delete: %v", err)
	}
	if found, err := DeleteTodo(globalDir, sid, "ghost"); err != nil || found {
		t.Fatalf("expected not-found for ghost, got found=%v err=%v", found, err)
	}
}

func TestClearCompletedTodos(t *testing.T) {
	globalDir := t.TempDir()
	sid := "sess-1"
	seed := []Todo{
		{ID: "a", Content: "a", Status: TaskStatusCompleted},
		{ID: "b", Content: "b", Status: TaskStatusCancelled},
		{ID: "c", Content: "c", Status: TaskStatusPending, Dependencies: []string{"a", "b"}},
		{ID: "d", Content: "d", Status: TaskStatusInProgress},
	}
	if err := SaveTodos(globalDir, sid, seed); err != nil {
		t.Fatalf("seed: %v", err)
	}
	n, err := ClearCompletedTodos(globalDir, sid)
	if err != nil {
		t.Fatalf("clear: %v", err)
	}
	if n != 2 {
		t.Fatalf("expected 2 removed, got %d", n)
	}
	todos, err := LoadTodos(globalDir, sid)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(todos) != 2 || todos[0].ID != "c" || todos[1].ID != "d" {
		t.Fatalf("unexpected remaining tasks: %#v", todos)
	}
	if len(todos[0].Dependencies) != 0 {
		t.Fatalf("satisfied dependencies should be stripped: %#v", todos[0].Dependencies)
	}
	if err := ValidateTaskGraph(todos); err != nil {
		t.Fatalf("graph invalid after clear: %v", err)
	}
}
