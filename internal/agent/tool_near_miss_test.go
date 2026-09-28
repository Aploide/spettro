package agent

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"spettro/internal/config"
	"spettro/internal/hooks"
)

func nearMissAllowed(names ...string) map[string]struct{} {
	out := make(map[string]struct{}, len(names))
	for _, n := range names {
		out[n] = struct{}{}
	}
	return out
}

// Underscore and case variants of an allowed tool run that tool, and the
// result carries the tool's own name.
func TestNearMissToolNameRoutesToAllowedTool(t *testing.T) {
	r, _ := newAliasTestRuntime(t)
	allowed := nearMissAllowed("file-read", "glob", "todo-write")
	for _, name := range []string{"file_read", "File_Read", "FILE-READ", " file_read "} {
		res := r.parallelExec(context.Background(), []toolCall{{Tool: name, Args: json.RawMessage(`{"path":"hello.go"}`)}}, allowed, nil)
		if res[0].status != "success" || res[0].name != "file-read" || !strings.Contains(res[0].output, "HelloWorld") {
			t.Errorf("%q: %s %s %q", name, res[0].status, res[0].name, res[0].output)
		}
	}
	res := r.parallelExec(context.Background(), []toolCall{{Tool: "todo_write", Args: json.RawMessage(`{"todos":[{"id":"1","content":"x"}]}`)}}, allowed, nil)
	if res[0].status != "success" || res[0].name != "todo-write" {
		t.Errorf("todo_write: %s %s %q", res[0].status, res[0].name, res[0].output)
	}
}

// A misspelt retired name reaches the retired name's canonical tool, with the
// argument conversion of the retired name.
func TestNearMissRetiredNameRoutesThroughAlias(t *testing.T) {
	r, _ := newAliasTestRuntime(t)
	res := r.parallelExec(context.Background(), []toolCall{{Tool: "task_create", Args: json.RawMessage(`{"id":"a","content":"write tests"}`)}}, nearMissAllowed("todo-write"), nil)
	if res[0].status != "success" || res[0].name != "todo-write" || !strings.Contains(res[0].output, "write tests") {
		t.Fatalf("task_create: %s %s %q", res[0].status, res[0].name, res[0].output)
	}
}

// Routing never reaches a tool the agent does not hold: web_fetch without
// web-fetch in the allow-list stays an error, and nothing runs.
func TestNearMissNeverWidensAllowList(t *testing.T) {
	r, dir := newAliasTestRuntime(t)
	res := r.parallelExec(context.Background(), []toolCall{
		{Tool: "file_write", Args: json.RawMessage(`{"path":"new.txt","content":"x"}`)},
		{Tool: "shell_exec", Args: json.RawMessage(`{"command":"touch ran"}`)},
	}, nearMissAllowed("file-read"), nil)
	for i, want := range []string{`tool "file_write" not allowed`, `not allowed`} {
		if res[i].status != "error" || !strings.Contains(res[i].output, want) {
			t.Errorf("call %d: %s %q, want error containing %q", i, res[i].status, res[i].output, want)
		}
	}
	for _, f := range []string{"new.txt", "ran"} {
		if fileExists(filepath.Join(dir, f)) {
			t.Errorf("%s was created by a call the agent is not allowed to make", f)
		}
	}
}

// An unknown name close to allowed tools gets a "did you mean" naming them;
// an unrelated one gets no guess; a known tool the agent does not hold keeps
// the plain "not allowed".
func TestNearMissErrorSuggestsClosestAllowedTools(t *testing.T) {
	r, _ := newAliasTestRuntime(t)
	allowed := nearMissAllowed("file-read", "file-edit", "file-write", "glob", "grep", "bash")
	cases := []struct {
		name, want, not string
	}{
		{"file-reed", `did you mean "file-read"`, ""},
		{"read_file", `did you mean "file-read"`, ""},
		{"str_replace_based_edit_tool", "no tool you can call has that name", "did you mean"},
		{"web-fetch", `tool "web-fetch" not allowed`, "no tool you can call has that name"},
	}
	for _, c := range cases {
		res := r.parallelExec(context.Background(), []toolCall{{Tool: c.name, Args: json.RawMessage(`{}`)}}, allowed, nil)
		out := res[0].output
		if res[0].status != "error" || !strings.Contains(out, c.want) || (c.not != "" && strings.Contains(out, c.not)) {
			t.Errorf("%q: %s %q (want %q, not %q)", c.name, res[0].status, out, c.want, c.not)
		}
	}
}

// Two allowed tools folding to the same key make the name ambiguous: the call
// is not routed to either.
func TestNearMissAmbiguousNameIsNotRouted(t *testing.T) {
	r, _ := newAliasTestRuntime(t)
	allowed := nearMissAllowed("deploy-app", "Deploy_App")
	call := toolCall{Tool: "deploy_app", Args: json.RawMessage(`{}`)}
	if got, ok := r.routeNearMissCall(call, allowed); ok {
		t.Fatalf("ambiguous name routed to %+v", got)
	}
}

// A name the operator's own tool holds is never re-routed, even when it folds
// to a built-in's name.
func TestNearMissLeavesOperatorToolsAlone(t *testing.T) {
	r, _ := newAliasTestRuntime(t)
	user := config.ToolSpec{ID: "File_Read", Kind: "script", EntryPoint: "./x.sh", Enabled: true}
	r.toolPolicies["File_Read"] = user
	if got, ok := r.routeNearMissCall(toolCall{Tool: "File_Read"}, nearMissAllowed("file-read")); ok {
		t.Fatalf("operator tool routed to %+v", got)
	}
}

// Hooks for the canonical tool fire on a routed call, and so do hooks written
// for the spelling the model used.
func TestNearMissHooksSeeBothNames(t *testing.T) {
	for _, matcher := range []string{"file-read", "file_read"} {
		r, _ := newAliasTestRuntime(t)
		r.hooksConfig = hooks.EffectiveConfig{Rules: []hooks.EffectiveRule{
			hookRule("deny", hooks.EventPreToolUse, matcher, `echo '{"decision":"deny","reason":"blocked by hook"}'`),
		}}
		res := r.parallelExec(context.Background(), []toolCall{{Tool: "file_read", Args: json.RawMessage(`{"path":"hello.go"}`)}}, nearMissAllowed("file-read"), nil)
		if res[0].status != "error" || !strings.Contains(res[0].output, "blocked by hook") {
			t.Errorf("matcher %q: %s %q", matcher, res[0].status, res[0].output)
		}
	}
}

func TestEditDistanceAndFold(t *testing.T) {
	if got := foldToolName(" Web_Fetch "); got != "web-fetch" {
		t.Errorf("fold = %q", got)
	}
	for _, c := range []struct {
		a, b string
		want int
	}{{"", "", 0}, {"abc", "", 3}, {"kitten", "sitting", 3}, {"file-read", "file-reed", 1}} {
		if got := editDistance(c.a, c.b); got != c.want {
			t.Errorf("editDistance(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
	if got := joinQuotedOr([]string{"a", "b", "c"}); got != `"a", "b" or "c"` {
		t.Errorf("joinQuotedOr = %s", got)
	}
}
