package agent

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

func TestExtractDuckDuckGoResults(t *testing.T) {
	html := `
<div class="results">
  <a class="result__a" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fexample.com%2Fpost">Example Post</a>
  <a class="result__a" href="https://duckduckgo.com/l/?uddg=https%3A%2F%2Fgolang.org%2Fdoc">Go Docs</a>
  <a class="result__a" href="/l/?uddg=https%3A%2F%2Fexample.com%2Fpost">Duplicate Example</a>
</div>`
	rows := extractDuckDuckGoResults(html, 10)
	if len(rows) != 2 {
		t.Fatalf("expected 2 unique rows, got %d: %#v", len(rows), rows)
	}
	if !strings.Contains(rows[0], "Example Post") || !strings.Contains(rows[0], "https://example.com/post") {
		t.Fatalf("unexpected first row: %q", rows[0])
	}
	if !strings.Contains(rows[1], "Go Docs") || !strings.Contains(rows[1], "https://golang.org/doc") {
		t.Fatalf("unexpected second row: %q", rows[1])
	}
}

func TestResolveDuckDuckGoResultURL(t *testing.T) {
	got := resolveDuckDuckGoResultURL("//duckduckgo.com/l/?uddg=https%3A%2F%2Fexample.org%2Fpath")
	if got != "https://example.org/path" {
		t.Fatalf("unexpected resolved url: %q", got)
	}
	if resolveDuckDuckGoResultURL("https://duckduckgo.com/l/?x=1") != "" {
		t.Fatalf("expected empty for missing uddg")
	}
	if resolveDuckDuckGoResultURL("javascript:alert(1)") != "" {
		t.Fatalf("expected empty for invalid non-http link")
	}
}

func TestRunTaskStopMarksRuntime(t *testing.T) {
	rt := &toolRuntime{}
	raw, _ := json.Marshal(map[string]string{"reason": "stop now"})
	msg, err := rt.runTaskStop(raw)
	if err != nil {
		t.Fatalf("task-stop error: %v", err)
	}
	if msg != "stop now" {
		t.Fatalf("unexpected message: %q", msg)
	}
	if !rt.shouldStop() {
		t.Fatalf("expected stop requested")
	}
	if rt.stopMessage() != "stop now" {
		t.Fatalf("unexpected stop reason: %q", rt.stopMessage())
	}
}

func TestRunGoalCompleteMarksRuntime(t *testing.T) {
	rt := &toolRuntime{goalMode: true}
	raw, _ := json.Marshal(map[string]any{"summary": "all tests pass", "verified": true})
	msg, err := rt.runGoalComplete(raw)
	if err != nil {
		t.Fatalf("goal-complete error: %v", err)
	}
	if msg != "goal marked complete" {
		t.Fatalf("unexpected message: %q", msg)
	}
	if !rt.goalIsComplete() {
		t.Fatalf("expected goal complete")
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.goalSummary != "all tests pass" {
		t.Fatalf("unexpected summary: %q", rt.goalSummary)
	}
	if !rt.goalVerified {
		t.Fatalf("expected goalVerified to be true")
	}
}

func TestRunGoalCompleteRejectedOutsideGoalMode(t *testing.T) {
	rt := &toolRuntime{goalMode: false}
	raw, _ := json.Marshal(map[string]any{"summary": "done"})
	_, err := rt.runGoalComplete(raw)
	if err == nil {
		t.Fatalf("expected error when goal-complete called outside goal mode")
	}
	if !strings.Contains(err.Error(), "only available in goal mode") {
		t.Fatalf("unexpected error: %v", err)
	}
	if rt.goalIsComplete() {
		t.Fatalf("goal should not be marked complete in non-goal mode")
	}
}

func TestRunConfigToolSetAndGetPermission(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	rt := &toolRuntime{cwd: filepath.Join(home, "repo")}

	// First set should persist because key is not preset yet.
	setRaw, _ := json.Marshal(map[string]string{
		"action": "set",
		"key":    "permission",
		"value":  "restricted",
	})
	if _, err := rt.runConfigTool(setRaw); err != nil {
		t.Fatalf("config set error: %v", err)
	}

	getRaw, _ := json.Marshal(map[string]string{
		"action": "get",
		"key":    "permission",
	})
	out, err := rt.runConfigTool(getRaw)
	if err != nil {
		t.Fatalf("config get error: %v", err)
	}
	if out != "permission=restricted" {
		t.Fatalf("unexpected output: %q", out)
	}

	// Second set without force should not override preset values.
	setAgainRaw, _ := json.Marshal(map[string]string{
		"action": "set",
		"key":    "permission",
		"value":  "yolo",
	})
	out, err = rt.runConfigTool(setAgainRaw)
	if err != nil {
		t.Fatalf("config set again error: %v", err)
	}
	if !strings.Contains(out, "preset; unchanged") {
		t.Fatalf("expected preset unchanged message, got %q", out)
	}
	getRaw, _ = json.Marshal(map[string]string{
		"action": "get",
		"key":    "permission",
	})
	out, err = rt.runConfigTool(getRaw)
	if err != nil {
		t.Fatalf("config get error: %v", err)
	}
	if out != "permission=restricted" {
		t.Fatalf("expected unchanged preset permission, got %q", out)
	}

	// Force must override preset values.
	forceRaw, _ := json.Marshal(map[string]any{
		"action": "set",
		"key":    "permission",
		"value":  "yolo",
		"force":  true,
	})
	if _, err := rt.runConfigTool(forceRaw); err != nil {
		t.Fatalf("forced config set error: %v", err)
	}
	out, err = rt.runConfigTool(getRaw)
	if err != nil {
		t.Fatalf("config get after force error: %v", err)
	}
	if out != "permission=yolo" {
		t.Fatalf("expected forced permission yolo, got %q", out)
	}
}

func TestAuthorizeNetworkAccessAllowsWithoutApprovalInYolo(t *testing.T) {
	rt := &toolRuntime{
		cwd:        t.TempDir(),
		permission: "yolo",
	}
	if err := rt.authorizeNetworkAccess(context.Background(), "web-search", "example"); err != nil {
		t.Fatalf("expected no error in yolo mode, got %v", err)
	}
}

func taskTestRuntime(t *testing.T) *toolRuntime {
	t.Helper()
	globalDir := t.TempDir()
	return &toolRuntime{sessionDir: filepath.Join(globalDir, "sessions", "sess-1")}
}

func todoWrite(t *testing.T, rt *toolRuntime, args map[string]any) (string, error) {
	t.Helper()
	raw, _ := json.Marshal(args)
	return rt.runTodoWrite(raw)
}

func mustTodoMerge(t *testing.T, rt *toolRuntime, items ...map[string]any) {
	t.Helper()
	if _, err := todoWrite(t, rt, map[string]any{"merge": true, "todos": items}); err != nil {
		t.Fatalf("todo-write merge %v: %v", items, err)
	}
}

type todoListOut struct {
	Tasks []struct {
		ID           string   `json:"id"`
		Content      string   `json:"content"`
		Status       string   `json:"status"`
		Priority     string   `json:"priority"`
		Dependencies []string `json:"dependencies"`
		BlockedBy    []string `json:"blocked_by"`
		Ready        bool     `json:"ready"`
	} `json:"tasks"`
	Notes []string `json:"notes"`
}

func decodeTodoList(t *testing.T, out string) todoListOut {
	t.Helper()
	var list todoListOut
	if err := json.Unmarshal([]byte(out), &list); err != nil {
		t.Fatalf("decode todo list: %v (%s)", err, out)
	}
	return list
}

func TestTaskGraphDependencyEnforcement(t *testing.T) {
	rt := taskTestRuntime(t)
	mustTodoMerge(t, rt, map[string]any{"id": "a", "content": "first"})
	mustTodoMerge(t, rt, map[string]any{"id": "b", "content": "second", "dependencies": []string{"a"}})

	// An unknown dependency is dropped with a note, not stored.
	out, err := todoWrite(t, rt, map[string]any{"merge": true, "todos": []map[string]any{{"id": "x", "content": "broken", "dependencies": []string{"ghost"}}}})
	if err != nil {
		t.Fatalf("unknown dependency: %v", err)
	}
	list := decodeTodoList(t, out)
	if len(list.Notes) != 1 || !strings.Contains(list.Notes[0], "ghost") {
		t.Fatalf("expected a note about the unknown dependency, got %s", out)
	}
	for _, task := range list.Tasks {
		if task.ID == "x" && len(task.Dependencies) != 0 {
			t.Fatalf("unknown dependency kept: %s", out)
		}
	}
	// Cycle rejected: a cannot depend on b.
	if _, err := todoWrite(t, rt, map[string]any{"merge": true, "todos": []map[string]any{{"id": "a", "dependencies": []string{"b"}}}}); err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("expected cycle error, got %v", err)
	}
	// Invalid status rejected.
	if _, err := todoWrite(t, rt, map[string]any{"merge": true, "todos": []map[string]any{{"id": "b", "status": "bogus"}}}); err == nil || !strings.Contains(err.Error(), "invalid task status") {
		t.Fatalf("expected status error, got %v", err)
	}
	// Completing b while a is pending is refused.
	if _, err := todoWrite(t, rt, map[string]any{"merge": true, "todos": []map[string]any{{"id": "b", "status": "completed"}}}); err == nil || !strings.Contains(err.Error(), "unmet dependencies") {
		t.Fatalf("expected unmet-dependencies error, got %v", err)
	}
	// Complete a, then b becomes ready and completable.
	mustTodoMerge(t, rt, map[string]any{"id": "a", "status": "done"})
	out, err = todoWrite(t, rt, map[string]any{})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var ready []string
	for _, task := range decodeTodoList(t, out).Tasks {
		if task.Ready {
			ready = append(ready, task.ID)
		}
	}
	if strings.Join(ready, ",") != "b,x" {
		t.Fatalf("expected b and x ready, got %s", out)
	}
	mustTodoMerge(t, rt, map[string]any{"id": "b", "status": "completed"})
}

func TestTaskListBlockedByAndOrder(t *testing.T) {
	rt := taskTestRuntime(t)
	mustTodoMerge(t, rt, map[string]any{"id": "c", "content": "third", "dependencies": []string{}})
	mustTodoMerge(t, rt, map[string]any{"id": "a", "content": "first"})
	out, err := todoWrite(t, rt, map[string]any{"merge": true, "todos": []map[string]any{{"id": "c", "dependencies": []string{"a"}}}})
	if err != nil {
		t.Fatalf("add dep: %v", err)
	}
	list := decodeTodoList(t, out)
	if len(list.Tasks) != 2 || list.Tasks[0].ID != "a" || list.Tasks[1].ID != "c" {
		t.Fatalf("expected dependency order a,c; got %s", out)
	}
	if len(list.Tasks[1].BlockedBy) != 1 || list.Tasks[1].BlockedBy[0] != "a" || list.Tasks[1].Ready {
		t.Fatalf("expected c blocked_by [a] and not ready, got %s", out)
	}
	if list.Tasks[1].Content != "third" {
		t.Fatalf("merge without content dropped the stored content: %s", out)
	}
}

func TestTodoWriteReplaceMergeAndDelete(t *testing.T) {
	rt := taskTestRuntime(t)
	// A replace mints task-N for tasks without an ID, around the explicit ones.
	out, err := todoWrite(t, rt, map[string]any{"todos": []map[string]any{
		{"content": "plan"}, {"id": "task-1", "content": "named"}, {"content": "build", "status": "in_progress"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, task := range decodeTodoList(t, out).Tasks {
		ids = append(ids, task.ID)
	}
	if strings.Join(ids, ",") != "task-2,task-1,task-3" {
		t.Fatalf("minted ids = %v (%s)", ids, out)
	}
	// Merge touches only the named task.
	out, err = todoWrite(t, rt, map[string]any{"merge": true, "todos": []map[string]any{{"id": "task-3", "status": "completed"}}})
	if err != nil {
		t.Fatal(err)
	}
	list := decodeTodoList(t, out)
	if len(list.Tasks) != 3 || list.Tasks[2].Status != "completed" || list.Tasks[2].Content != "build" {
		t.Fatalf("merge result: %s", out)
	}
	// delete and clear_completed.
	out, err = todoWrite(t, rt, map[string]any{"delete": []string{"task-1", "nope"}, "clear_completed": true})
	if err != nil {
		t.Fatal(err)
	}
	list = decodeTodoList(t, out)
	if len(list.Tasks) != 1 || list.Tasks[0].ID != "task-2" {
		t.Fatalf("after delete: %s", out)
	}
	if len(list.Notes) != 1 || !strings.Contains(list.Notes[0], "nope") {
		t.Fatalf("expected a note for the unknown delete, got %s", out)
	}
	// An empty replace clears the list.
	out, err = todoWrite(t, rt, map[string]any{"todos": []map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	if list := decodeTodoList(t, out); len(list.Tasks) != 0 {
		t.Fatalf("empty replace kept tasks: %s", out)
	}
}

// Sub-agents share the parent's session folder: a worker's replace must not
// wipe the orchestrator's list, so below the top level todos always merge.
func TestTodoWriteSubAgentAlwaysMerges(t *testing.T) {
	parent := taskTestRuntime(t)
	mustTodoMerge(t, parent, map[string]any{"id": "orchestrate", "content": "the parent's task"})
	worker := &toolRuntime{sessionDir: parent.sessionDir, delegationDepth: 1}
	out, err := todoWrite(t, worker, map[string]any{"todos": []map[string]any{{"id": "slice", "content": "the worker's task"}}})
	if err != nil {
		t.Fatal(err)
	}
	list := decodeTodoList(t, out)
	if len(list.Tasks) != 2 || list.Tasks[0].ID != "orchestrate" {
		t.Fatalf("worker replace wiped the parent's list: %s", out)
	}
	if len(list.Notes) == 0 || !strings.Contains(list.Notes[0], "merged") {
		t.Fatalf("expected a note that the replace merged, got %s", out)
	}
}
