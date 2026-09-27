package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"spettro/internal/config"
	"spettro/internal/hooks"
	"spettro/internal/provider"
)

// newAliasTestRuntime is a YOLO runtime over a small workspace, with a
// session folder for the task list and a symbol-index searcher.
func newAliasTestRuntime(t *testing.T) (*toolRuntime, string) {
	t.Helper()
	dir := t.TempDir()
	writeTestFile(t, dir, "hello.go", "package main\n\nfunc HelloWorld() string { return \"hi\" }\n")
	if err := os.MkdirAll(filepath.Join(dir, "pkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, dir, "pkg/use.go", "package pkg\n\nvar _ = HelloWorld\n")
	return &toolRuntime{
		cwd:           dir,
		permission:    config.PermissionYOLO,
		readSet:       map[string]struct{}{},
		requiredReads: map[string]struct{}{},
		allowedShell:  map[string]struct{}{},
		toolPolicies:  map[string]config.ToolSpec{},
		searcher:      NewRepoSearcher(dir),
		sessionDir:    filepath.Join(t.TempDir(), "sessions", "sess-1"),
	}, dir
}

func aliasCall(t *testing.T, tool string, args any) toolCall {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	return toolCall{Tool: tool, Args: raw}
}

// Every retired name routes to a canonical tool that is itself a real,
// advertised built-in, and never to another retired name.
func TestLegacyToolsRouteToAdvertisedTools(t *testing.T) {
	for name, lt := range legacyTools {
		if _, retired := legacyTools[lt.canonical]; retired {
			t.Errorf("%s routes to %s, which is itself retired", name, lt.canonical)
		}
		if _, ok := builtinNativeToolSchemas[lt.canonical]; !ok {
			t.Errorf("%s routes to %s, which has no schema", name, lt.canonical)
		}
		if _, ok := builtinNativeToolSchemas[name]; ok {
			t.Errorf("retired %s still has a schema, so it would be advertised", name)
		}
		if _, ok := builtinNativeToolDescs[name]; ok {
			t.Errorf("retired %s still has a description", name)
		}
	}
	if got := LegacyToolNames("bash"); strings.Join(got, ",") != "bash-output,shell-exec" {
		t.Errorf("LegacyToolNames(bash) = %v", got)
	}
	if got := LegacyToolNames("file-read"); got != nil {
		t.Errorf("LegacyToolNames(file-read) = %v, want none", got)
	}
}

// A call under a retired name gives the same result as the canonical call
// it maps to, runs under the canonical name (trace and allow-list), and
// needs only the canonical tool to be allowed.
func TestLegacyToolCallsMatchCanonicalCalls(t *testing.T) {
	cases := []struct {
		legacy    toolCall
		canonical toolCall
	}{
		{toolCall{Tool: "shell-exec", Args: json.RawMessage(`{"command":"echo hi"}`)}, toolCall{Tool: "bash", Args: json.RawMessage(`{"command":"echo hi"}`)}},
		{toolCall{Tool: "repo-search", Args: json.RawMessage(`{"query":"HelloWorld"}`)}, toolCall{Tool: "grep", Args: json.RawMessage(`{"symbol":"HelloWorld"}`)}},
		{toolCall{Tool: "repo-search", Args: json.RawMessage(`{"query":""}`)}, toolCall{Tool: "grep", Args: json.RawMessage(`{"symbol":""}`)}},
		{toolCall{Tool: "ls", Args: json.RawMessage(`{"path":"pkg"}`)}, toolCall{Tool: "glob", Args: json.RawMessage(`{"path":"pkg"}`)}},
		{toolCall{Tool: "ls", Args: json.RawMessage(`{}`)}, toolCall{Tool: "glob", Args: json.RawMessage(`{}`)}},
		{toolCall{Tool: "task-list", Args: json.RawMessage(`{}`)}, toolCall{Tool: "todo-write", Args: json.RawMessage(`{}`)}},
		{toolCall{Tool: "skill-activate", Args: json.RawMessage(`{"name":"none"}`)}, toolCall{Tool: "skill", Args: json.RawMessage(`{"name":"none"}`)}},
		{toolCall{Tool: "activate-skill", Args: json.RawMessage(`{"name":"none"}`)}, toolCall{Tool: "skill", Args: json.RawMessage(`{"name":"none"}`)}},
		{toolCall{Tool: "skill-read", Args: json.RawMessage(`{"skill":"none"}`)}, toolCall{Tool: "skill", Args: json.RawMessage(`{"skill":"none"}`)}},
		{toolCall{Tool: "skill-list", Args: json.RawMessage(`{"query":"x"}`)}, toolCall{Tool: "skill", Args: json.RawMessage(`{"query":"x"}`)}},
	}
	for _, c := range cases {
		r, _ := newAliasTestRuntime(t)
		allowed := map[string]struct{}{c.canonical.Tool: {}}
		var traces []ToolTrace
		got := r.parallelExec(context.Background(), []toolCall{c.legacy}, allowed, func(tr ToolTrace) { traces = append(traces, tr) })
		want := r.parallelExec(context.Background(), []toolCall{c.canonical}, allowed, nil)
		if got[0].output != want[0].output || got[0].status != want[0].status {
			t.Errorf("%s %s:\n got %s %q\nwant %s %q", c.legacy.Tool, c.legacy.Args, got[0].status, got[0].output, want[0].status, want[0].output)
		}
		if got[0].name != c.canonical.Tool {
			t.Errorf("%s result named %q, want %q", c.legacy.Tool, got[0].name, c.canonical.Tool)
		}
		for _, tr := range traces {
			if tr.Name == c.legacy.Tool {
				t.Errorf("%s leaked into a trace under its retired name", c.legacy.Tool)
			}
		}
	}
}

func TestLsAliasListsDirectoryEntries(t *testing.T) {
	r, _ := newAliasTestRuntime(t)
	out, err := r.execute(context.Background(), aliasCall(t, "ls", map[string]any{}), map[string]struct{}{"glob": {}})
	if err != nil {
		t.Fatal(err)
	}
	if out != "hello.go\npkg/" {
		t.Fatalf("listing = %q", out)
	}
}

func TestRepoSearchAliasListsDefinitionsFirst(t *testing.T) {
	r, _ := newAliasTestRuntime(t)
	out, err := r.execute(context.Background(), aliasCall(t, "repo-search", map[string]any{"query": "HelloWorld"}), map[string]struct{}{"grep": {}})
	if err != nil {
		t.Fatal(err)
	}
	def, use := strings.Index(out, "hello.go"), strings.Index(out, "pkg/use.go")
	if def < 0 || use < 0 || def > use {
		t.Fatalf("expected the definition in hello.go before the usage:\n%s", out)
	}
	if _, err := r.execute(context.Background(), aliasCall(t, "grep", map[string]any{"pattern": "x", "symbol": "y"}), map[string]struct{}{"grep": {}}); err == nil {
		t.Fatal("grep with both pattern and symbol should be refused")
	}
}

// The task-* family maps onto todo-write: create/update merge one task,
// delete removes it, get/list read the whole list.
func TestTaskAliasesMapOntoTodoWrite(t *testing.T) {
	r, _ := newAliasTestRuntime(t)
	allowed := map[string]struct{}{"todo-write": {}}
	run := func(tool string, args any) todoListOut {
		t.Helper()
		out, err := r.execute(context.Background(), aliasCall(t, tool, args), allowed)
		if err != nil {
			t.Fatalf("%s: %v", tool, err)
		}
		return decodeTodoList(t, out)
	}
	run("task-create", map[string]any{"id": "a", "content": "first"})
	run("task-create", map[string]any{"content": "second", "dependencies": []string{"a"}})
	list := run("task-update", map[string]any{"id": "a", "status": "completed"})
	if len(list.Tasks) != 2 || list.Tasks[0].Status != "completed" || list.Tasks[0].Content != "first" {
		t.Fatalf("after update: %+v", list)
	}
	if !list.Tasks[1].Ready || list.Tasks[1].ID != "task-1" {
		t.Fatalf("second task should be task-1 and ready: %+v", list)
	}
	if got := run("task-get", map[string]any{"id": "a"}); len(got.Tasks) != 2 {
		t.Fatalf("task-get returns the whole list: %+v", got)
	}
	if got := run("task-delete", map[string]any{"clear_completed": true}); len(got.Tasks) != 1 || got.Tasks[0].ID != "task-1" {
		t.Fatalf("after clear_completed: %+v", got)
	}
	if got := run("task-delete", map[string]any{"id": "task-1"}); len(got.Tasks) != 0 {
		t.Fatalf("after delete: %+v", got)
	}
	if _, err := r.execute(context.Background(), aliasCall(t, "task-update", map[string]any{"status": "completed"}), allowed); err == nil || !strings.Contains(err.Error(), "task-update args: id is required") {
		t.Fatalf("task-update without id: %v", err)
	}
}

// task-update keeps its old contract: an empty dependencies list leaves the
// stored ones alone, and an unknown ID is an error, not a new task.
func TestTaskUpdateAliasKeepsItsContract(t *testing.T) {
	r, _ := newAliasTestRuntime(t)
	allowed := map[string]struct{}{"todo-write": {}}
	exec := func(tool string, args any) (todoListOut, error) {
		t.Helper()
		out, err := r.execute(context.Background(), aliasCall(t, tool, args), allowed)
		if err != nil {
			return todoListOut{}, err
		}
		return decodeTodoList(t, out), nil
	}
	if _, err := exec("task-create", map[string]any{"id": "a", "content": "first"}); err != nil {
		t.Fatal(err)
	}
	if _, err := exec("task-create", map[string]any{"id": "b", "content": "second", "dependencies": []string{"a"}}); err != nil {
		t.Fatal(err)
	}
	list, err := exec("task-update", map[string]any{"id": "b", "priority": "high", "dependencies": []string{}})
	if err != nil {
		t.Fatal(err)
	}
	for _, task := range list.Tasks {
		if task.ID == "b" && (strings.Join(task.Dependencies, ",") != "a" || task.Ready || task.Priority != "high") {
			t.Fatalf("task-update with dependencies [] changed b: %+v", task)
		}
	}
	for _, args := range []map[string]any{
		{"id": "ghost", "content": "boo"},
		{"id": "ghost", "status": "completed"},
	} {
		if _, err := exec("task-update", args); err == nil || !strings.Contains(err.Error(), `task "ghost" not found`) {
			t.Fatalf("task-update %v on an unknown id: %v", args, err)
		}
	}
	if list, _ := exec("task-list", map[string]any{}); len(list.Tasks) != 2 {
		t.Fatalf("an unknown id must not add a task: %+v", list)
	}
	// todo-write's own merge still reads [] as "clear".
	list, err = exec("todo-write", map[string]any{"merge": true, "todos": []any{map[string]any{"id": "b", "dependencies": []string{}}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, task := range list.Tasks {
		if task.ID == "b" && len(task.Dependencies) != 0 {
			t.Fatalf("todo-write merge with [] should clear: %+v", task)
		}
	}
}

// A sub-agent's full todo-write becomes a merge (it must not wipe the
// parent's list); writing the same ID-less list again with new statuses
// updates those tasks instead of adding copies.
func TestSubAgentTodoWriteRewriteDoesNotDuplicate(t *testing.T) {
	r, _ := newAliasTestRuntime(t)
	allowed := map[string]struct{}{"todo-write": {}}
	write := func(args any) todoListOut {
		t.Helper()
		out, err := r.execute(context.Background(), aliasCall(t, "todo-write", args), allowed)
		if err != nil {
			t.Fatal(err)
		}
		return decodeTodoList(t, out)
	}
	write(map[string]any{"todos": []any{map[string]any{"id": "orch", "content": "orchestrator task"}}})
	r.delegationDepth = 1
	write(map[string]any{"todos": []any{
		map[string]any{"content": "write parser", "status": "in_progress"},
		map[string]any{"content": "add tests"},
	}})
	list := write(map[string]any{"todos": []any{
		map[string]any{"content": "write parser", "status": "completed"},
		map[string]any{"content": "add tests", "status": "in_progress"},
		map[string]any{"content": "add tests", "status": "pending"},
	}})
	// The third entry repeats a content already matched in this call, so it
	// is a new task.
	if len(list.Tasks) != 4 {
		t.Fatalf("want orch, the 2 updated tasks and 1 new one, got %+v", list.Tasks)
	}
	status := map[string]string{}
	for _, task := range list.Tasks {
		status[task.ID] = task.Content + "|" + task.Status
	}
	want := map[string]string{
		"orch":   "orchestrator task|pending",
		"task-1": "write parser|completed",
		"task-2": "add tests|in_progress",
		"task-3": "add tests|pending",
	}
	for id, w := range want {
		if status[id] != w {
			t.Fatalf("task %s = %q, want %q (list %v)", id, status[id], w, status)
		}
	}
}

// An alias is never a way around the allow-list: the canonical tool must be
// allowed, and an argument-conversion failure is an error result, not a run.
func TestLegacyToolNeedsCanonicalAllowed(t *testing.T) {
	r, _ := newAliasTestRuntime(t)
	res := r.parallelExec(context.Background(), []toolCall{
		{Tool: "shell-exec", Args: json.RawMessage(`{"command":"echo hi"}`)},
		{Tool: "task-delete", Args: json.RawMessage(`{}`)},
	}, map[string]struct{}{"shell-exec": {}, "task-delete": {}}, nil)
	if res[0].status != "error" || !strings.Contains(res[0].output, `tool "bash" not allowed`) {
		t.Errorf("shell-exec without bash allowed: %s %q", res[0].status, res[0].output)
	}
	if res[1].status != "error" || !strings.Contains(res[1].output, "task-delete args") {
		t.Errorf("task-delete without id: %s %q", res[1].status, res[1].output)
	}
}

// A manifest tool of another kind that happens to carry a retired name (a
// user's own "ls" script) is that tool, not an alias.
func TestCustomToolNamedLikeRetiredToolIsNotRerouted(t *testing.T) {
	r, _ := newAliasTestRuntime(t)
	r.toolPolicies["ls"] = config.ToolSpec{ID: "ls", Kind: "script"}
	call := toolCall{Tool: "ls", Args: json.RawMessage(`{}`)}
	got, err := r.canonicalCall(call)
	if err != nil || got.Tool != "ls" {
		t.Fatalf("custom ls rerouted to %q (%v)", got.Tool, err)
	}
}

// tool-search lists each tool once, under its canonical name.
func TestToolSearchHidesAliases(t *testing.T) {
	r, _ := newAliasTestRuntime(t)
	bash := config.ToolSpec{ID: "bash", Name: "Bash", Kind: "builtin", Aliases: []string{"shell-exec", "bash-output"}}
	r.toolPolicies = map[string]config.ToolSpec{"bash": bash, "shell-exec": bash, "bash-output": bash}
	allowed := map[string]struct{}{"bash": {}, "shell-exec": {}, "bash-output": {}, "ls": {}}
	out, err := r.runToolSearch(allowed, []byte(`{"query":""}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "- bash (") {
		t.Fatalf("bash missing:\n%s", out)
	}
	for _, alias := range []string{"shell-exec", "bash-output", "- ls ("} {
		if strings.Contains(out, alias) {
			t.Errorf("tool-search lists alias %q:\n%s", alias, out)
		}
	}
}

// A hook written for shell-exec, which was the very same tool as bash, keeps
// guarding the shell now that the model only sees bash.
func TestHooksForSameToolNameFireOnCanonical(t *testing.T) {
	r, _ := newAliasTestRuntime(t)
	r.hooksConfig = hooks.EffectiveConfig{Rules: []hooks.EffectiveRule{
		hookRule("no-shell", hooks.EventPreToolUse, "shell-exec", `echo '{"decision":"deny","reason":"no shell here"}'`),
	}}
	res := r.parallelExec(context.Background(), []toolCall{{Tool: "bash", Args: json.RawMessage(`{"command":"echo hi"}`)}}, map[string]struct{}{"bash": {}}, nil)
	if res[0].status != "error" || !strings.Contains(res[0].output, "no shell here") {
		t.Fatalf("shell-exec hook did not fire on bash: %s %q", res[0].status, res[0].output)
	}
}

// A hook on the canonical tool fires however the model named it, so an alias
// is no way around it.
func TestHooksForCanonicalFireOnAliasCalls(t *testing.T) {
	r, _ := newAliasTestRuntime(t)
	r.hooksConfig = hooks.EffectiveConfig{Rules: []hooks.EffectiveRule{
		hookRule("no-glob", hooks.EventPreToolUse, "glob", `echo '{"decision":"deny","reason":"no glob"}'`),
	}}
	res := r.parallelExec(context.Background(), []toolCall{{Tool: "ls", Args: json.RawMessage(`{}`)}}, map[string]struct{}{"glob": {}}, nil)
	if res[0].status != "error" || !strings.Contains(res[0].output, "no glob") {
		t.Fatalf("glob hook did not fire on ls: %s %q", res[0].status, res[0].output)
	}
}

// A hook written for a retired name that was a narrower operation fires when
// the model calls that name, and on nothing else the canonical tool does: a
// deny on task-delete must not block creating or reading tasks, and one on ls
// must not block a glob pattern search.
func TestHooksForNarrowerRetiredNamesStayNarrow(t *testing.T) {
	r, _ := newAliasTestRuntime(t)
	r.hooksConfig = hooks.EffectiveConfig{Rules: []hooks.EffectiveRule{
		hookRule("no-deletes", hooks.EventPreToolUse, "task-delete", `echo '{"decision":"deny","reason":"no deletes"}'`),
		hookRule("no-ls", hooks.EventPreToolUse, "ls", `echo '{"decision":"deny","reason":"no ls"}'`),
		hookRule("no-multi", hooks.EventPreToolUse, "multi-edit", `echo '{"decision":"deny","reason":"no multi"}'`),
		hookRule("no-repo-search", hooks.EventPreToolUse, "repo-search", `echo '{"decision":"deny","reason":"no repo-search"}'`),
	}}
	allowed := map[string]struct{}{"todo-write": {}, "glob": {}, "grep": {}}
	for _, c := range []toolCall{
		aliasCall(t, "task-create", map[string]any{"content": "write parser"}),
		aliasCall(t, "todo-write", map[string]any{}),
		aliasCall(t, "task-list", map[string]any{}),
		aliasCall(t, "glob", map[string]any{"pattern": "*.go"}),
		aliasCall(t, "grep", map[string]any{"pattern": "HelloWorld"}),
	} {
		res := r.parallelExec(context.Background(), []toolCall{c}, allowed, nil)
		if res[0].status == "error" {
			t.Errorf("%s %s blocked: %q", c.Tool, c.Args, res[0].output)
		}
	}
	for _, c := range []struct {
		call toolCall
		want string
	}{
		{aliasCall(t, "task-delete", map[string]any{"id": "task-1"}), "no deletes"},
		{aliasCall(t, "ls", map[string]any{}), "no ls"},
		{aliasCall(t, "repo-search", map[string]any{"query": "HelloWorld"}), "no repo-search"},
	} {
		res := r.parallelExec(context.Background(), []toolCall{c.call}, allowed, nil)
		if res[0].status != "error" || !strings.Contains(res[0].output, c.want) {
			t.Errorf("%s not blocked by its own hook: %s %q", c.call.Tool, res[0].status, res[0].output)
		}
	}
}

// A hook copied under both shell-exec and bash runs once per bash call; two
// different hooks both run.
func TestHooksCopiedUnderBothShellNamesRunOnce(t *testing.T) {
	r, _ := newAliasTestRuntime(t)
	log := filepath.Join(t.TempDir(), "hook.log")
	logCmd := func(tag string) string { return "echo " + tag + " >> " + log + "; echo '{\"decision\":\"allow\"}'" }
	r.hooksConfig = hooks.EffectiveConfig{Rules: []hooks.EffectiveRule{
		hookRule("copy-old", hooks.EventPostToolUse, "shell-exec", logCmd("same")),
		hookRule("copy-new", hooks.EventPostToolUse, "bash", logCmd("same")),
		hookRule("other", hooks.EventPostToolUse, "shell-exec", logCmd("other")),
	}}
	allowed := map[string]struct{}{"bash": {}}
	for _, name := range []string{"bash", "shell-exec"} {
		if err := os.WriteFile(log, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		res := r.parallelExec(context.Background(), []toolCall{{Tool: name, Args: json.RawMessage(`{"command":"echo hi"}`)}}, allowed, nil)
		if res[0].status == "error" {
			t.Fatalf("%s: %q", name, res[0].output)
		}
		raw, _ := os.ReadFile(log)
		if got := strings.Fields(string(raw)); strings.Join(got, ",") != "same,other" {
			t.Errorf("%s call ran hooks %v, want [same other]", name, got)
		}
	}
}

// The loop detector signs a retired call and its canonical call alike.
func TestLoopCallsUseCanonicalNames(t *testing.T) {
	calls := loopCalls([]provider.NativeTool{
		{Name: "shell-exec", Args: json.RawMessage(`{"command":"go test"}`)},
		{Name: "repo-search", Args: json.RawMessage(`{"query":"Foo"}`)},
	})
	if calls[0].Tool != "bash" || string(calls[0].Args) != `{"command":"go test"}` {
		t.Errorf("shell-exec signed as %s %s", calls[0].Tool, calls[0].Args)
	}
	if calls[1].Tool != "grep" || string(calls[1].Args) != `{"symbol":"Foo"}` {
		t.Errorf("repo-search signed as %s %s", calls[1].Tool, calls[1].Args)
	}
}
