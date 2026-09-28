package agent

import (
	"context"
	"encoding/json"
	"runtime"
	"slices"
	"strings"
	"testing"

	"spettro/internal/config"
	"spettro/internal/hooks"
	"spettro/internal/provider"
)

// nameTestArgs are, for every canonical tool and retired name, arguments a
// call of the built-in accepts, and a piece of its output when it succeeds
// in the alias test workspace ("" when the result is not checked: a call
// that needs a language server or an earlier read fails in the built-in's
// own words, which is still the built-in running).
var nameTestArgs = map[string]struct {
	args string
	want string
}{
	"bash":           {`{"command":"echo hi"}`, "hi"},
	"shell-exec":     {`{"command":"echo hi"}`, "hi"},
	"bash-output":    {`{"command":"echo hi"}`, "hi"},
	"file-edit":      {`{"path":"hello.go","old_string":"hi","new_string":"ho"}`, ""},
	"multi-edit":     {`{"path":"hello.go","edits":[{"old_string":"hi","new_string":"ho"}]}`, ""},
	"grep":           {`{"pattern":"HelloWorld"}`, "hello.go"},
	"repo-search":    {`{"query":"HelloWorld"}`, "hello.go"},
	"glob":           {`{"pattern":"*.go"}`, "hello.go"},
	"ls":             {`{}`, "hello.go"},
	"todo-write":     {`{}`, ""},
	"task-create":    {`{"content":"write parser"}`, "write parser"},
	"task-update":    {`{"id":"ghost","status":"completed"}`, ""},
	"task-get":       {`{"id":"ghost"}`, ""},
	"task-list":      {`{}`, ""},
	"task-delete":    {`{"id":"ghost"}`, ""},
	"skill":          {`{"name":"none"}`, ""},
	"skill-read":     {`{"name":"none"}`, ""},
	"skill-list":     {`{}`, "[]"},
	"activate-skill": {`{"name":"none"}`, ""},
	"skill-activate": {`{"name":"none"}`, ""},
	"lsp":            {`{"op":"diagnostics"}`, ""},
	"diagnostics":    {`{}`, ""},
	"references":     {`{"path":"hello.go","symbol":"HelloWorld"}`, ""},
	"hover":          {`{"path":"hello.go","symbol":"HelloWorld"}`, ""},
	"lsp-restart":    {`{}`, ""},
}

// canonicalToolNames returns every tool a retired name routes to, sorted.
func canonicalToolNames() []string {
	var out []string
	for _, lt := range legacyTools {
		if !slices.Contains(out, lt.canonical) {
			out = append(out, lt.canonical)
		}
	}
	slices.Sort(out)
	return out
}

func builtinSpec(id string) config.ToolSpec {
	return config.ToolSpec{ID: id, Name: id, Kind: "builtin", Enabled: true, PermittedActions: []string{"read"}}
}

// userToolRuntime is the alias test runtime with a script tool of the
// operator's own called name, held by the agent next to the given built-ins.
// A PreToolUse hook denies every call of the operator's tool ("user-hook"),
// and an agent rule denies it outright when denyUser is set.
func userToolRuntime(t *testing.T, name string, builtins []string, denyUser bool) (*toolRuntime, map[string]struct{}) {
	t.Helper()
	r, _ := newAliasTestRuntime(t)
	user := config.ToolSpec{ID: name, Name: "Mine", Kind: "script", EntryPoint: "./mine.sh", Enabled: true, PermittedActions: []string{"read"}}
	r.manifest = &config.AgentManifest{Tools: []config.ToolSpec{user}}
	r.toolPolicies = map[string]config.ToolSpec{name: user}
	allowed := map[string]struct{}{name: {}}
	for _, id := range builtins {
		spec := builtinSpec(id)
		r.manifest.Tools = append(r.manifest.Tools, spec)
		r.toolPolicies[id] = spec
		allowed[id] = struct{}{}
	}
	if denyUser {
		r.agentRules = []config.PermissionRule{{Permission: "tool", Pattern: name, Action: config.RuleDeny}}
	} else {
		r.hooksConfig = hooks.EffectiveConfig{Rules: []hooks.EffectiveRule{
			hookRule("user-hook", hooks.EventPreToolUse, name, `echo '{"decision":"deny","reason":"user-hook"}'`),
		}}
	}
	return r, allowed
}

// runNamed runs one call through parallelExec, as the tool loop does, and
// returns its result and the names its traces carried.
func runNamed(r *toolRuntime, name string, allowed map[string]struct{}) (parallelResult, []string) {
	var traced []string
	res := r.parallelExec(context.Background(), []toolCall{{Tool: name, Args: json.RawMessage(nameTestArgs[name].args)}}, allowed, func(tr ToolTrace) {
		if tr.Name != "comment" {
			traced = append(traced, tr.Name)
		}
	})
	return res[0], traced
}

// assertBuiltinRan checks that a call ran the built-in named as it was: no
// refusal for the operator's tool, no rule or hook of the operator's tool,
// and the built-in's output when it is known.
func assertBuiltinRan(t *testing.T, label, name string, res parallelResult) {
	t.Helper()
	for _, bad := range []string{"cannot run script tools", "not allowed", "user-hook", "denied by policy"} {
		if strings.Contains(res.output, bad) {
			t.Errorf("%s: %s did not run the built-in (%q): %s", label, name, bad, res.output)
		}
	}
	if want := nameTestArgs[name].want; want != "" && (res.status != "success" || !strings.Contains(res.output, want)) {
		t.Errorf("%s: %s = %s %q, want output with %q", label, name, res.status, res.output, want)
	}
}

func advertisedNames(r *toolRuntime, allowed []string) []string {
	var names []string
	for _, s := range r.buildToolSurface(allowed, "").specs() {
		names = append(names, s.Name)
	}
	return names
}

// A tool of the operator's own that shares a canonical or retired built-in
// name wins every call made by that name, whichever name it is, and the
// built-ins it shadows stay reachable under their own names (see
// tool_names.go and docs/tools.md):
//
//   - the operator's tool is never rewritten into a built-in, never runs a
//     built-in's code, and is never advertised with a built-in's schema; its
//     own hooks and rules apply to calls of it;
//   - with a retired name taken, the canonical built-in still runs, and
//     neither the hooks nor the rules written for the operator's tool touch
//     it;
//   - with a canonical name taken, every retired built-in of that tool that
//     the agent holds runs under its own name, is advertised under it, and
//     never becomes a call of the operator's tool.
func TestUserToolSharingABuiltinName(t *testing.T) {
	stubLSPAvailable(t, true)
	names := canonicalToolNames()
	for name := range legacyTools {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		if _, ok := nameTestArgs[name]; !ok {
			t.Fatalf("no test arguments for %s", name)
		}
		t.Run(name, func(t *testing.T) {
			// The operator's tool itself.
			r, allowed := userToolRuntime(t, name, nil, false)
			if got, err := r.canonicalCall(toolCall{Tool: name, Args: json.RawMessage(nameTestArgs[name].args)}); err != nil || got.Tool != name || got.CalledAs != "" {
				t.Fatalf("a call of the user's %s became %+v, %v", name, got, err)
			}
			res, _ := runNamed(r, name, allowed)
			if res.status != "error" || !strings.Contains(res.output, "user-hook") {
				t.Errorf("the user's hook on %s did not apply to its call: %s %q", name, res.status, res.output)
			}
			r.hooksConfig = hooks.EffectiveConfig{}
			res, traced := runNamed(r, name, allowed)
			if res.status != "error" || !strings.Contains(res.output, `tool "`+name+`" is a script tool`) {
				t.Errorf("a call of the user's %s must reach it and run nothing: %s %q", name, res.status, res.output)
			}
			if slices.ContainsFunc(traced, func(n string) bool { return n != name }) {
				t.Errorf("a call of the user's %s traced as %v", name, traced)
			}
			if got := advertisedNames(r, []string{name}); len(got) != 0 {
				t.Errorf("the user's %s advertised as built-in tools %v", name, got)
			}

			if lt, retired := legacyTools[name]; retired {
				// A retired name taken: its canonical built-in is untouched,
				// by the user's hook and by a rule denying the user's tool.
				for _, denyUser := range []bool{false, true} {
					r, allowed := userToolRuntime(t, name, []string{lt.canonical}, denyUser)
					res, traced := runNamed(r, lt.canonical, allowed)
					assertBuiltinRan(t, "canonical next to the user's tool", lt.canonical, res)
					if slices.Contains(traced, name) {
						t.Errorf("a %s call traced under the user's %s", lt.canonical, name)
					}
				}
				r, _ := userToolRuntime(t, name, []string{lt.canonical}, false)
				if got := advertisedNames(r, []string{lt.canonical, name}); !slices.Equal(got, []string{lt.canonical}) {
					t.Errorf("advertised %v, want %s alone", got, lt.canonical)
				}
				return
			}

			// A canonical name taken: each retired built-in stands unfolded.
			retired := LegacyToolNames(name)
			for _, denyUser := range []bool{false, true} {
				r, allowed := userToolRuntime(t, name, retired, denyUser)
				for _, old := range retired {
					if got, err := r.canonicalCall(toolCall{Tool: old, Args: json.RawMessage(nameTestArgs[old].args)}); err != nil || got.Tool != old {
						t.Errorf("%s became a call of %+v, %v with the user's %s", old, got, err, name)
					}
					res, traced := runNamed(r, old, allowed)
					assertBuiltinRan(t, "unfolded", old, res)
					if slices.Contains(traced, name) {
						t.Errorf("an unfolded %s call traced as the user's %s", old, name)
					}
				}
			}
			r, allowed = userToolRuntime(t, name, retired, false)
			got := advertisedNames(r, append([]string{name}, retired...))
			if !slices.Equal(got, retired) {
				t.Errorf("advertised %v, want the unfolded %v and not the user's %s", got, retired, name)
			}
			for _, spec := range r.unfoldedToolSpecs(retired) {
				if spec.Description == "" || !json.Valid(spec.Schema) {
					t.Errorf("unfolded %s advertised without a description or valid schema: %+v", spec.Name, spec)
				}
			}
			// Hooks written for an unfolded name fire on its calls.
			for _, old := range retired {
				r.hooksConfig = hooks.EffectiveConfig{Rules: []hooks.EffectiveRule{
					hookRule("old-hook", hooks.EventPreToolUse, old, `echo '{"decision":"deny","reason":"old-hook"}'`),
				}}
				if res, _ := runNamed(r, old, allowed); !strings.Contains(res.output, "old-hook") {
					t.Errorf("the hook for %s did not fire on its unfolded call: %q", old, res.output)
				}
			}
			// Not held, a retired name never reaches the user's tool.
			r, allowed = userToolRuntime(t, name, nil, false)
			for _, old := range retired {
				if res, _ := runNamed(r, old, allowed); !strings.Contains(res.output, `tool "`+old+`" not allowed`) {
					t.Errorf("%s, not held, must not reach the user's %s: %q", old, name, res.output)
				}
			}
		})
	}
}

// An unfolded built-in keeps its own policy while its code runs: the shell's
// approval is decided by shell-exec's manifest entry, not by the entry of
// the operator's tool called bash.
func TestUnfoldedBuiltinUsesItsOwnPolicy(t *testing.T) {
	r, allowed := userToolRuntime(t, "bash", []string{"shell-exec"}, false)
	r.permission = config.PermissionAskFirst
	shell := builtinSpec("shell-exec")
	shell.RequiresApproval = true
	r.toolPolicies["shell-exec"] = shell
	var asked []string
	r.shellApproval = func(_ context.Context, req ShellApprovalRequest) (ShellApprovalDecision, error) {
		asked = append(asked, req.Command)
		return ShellApprovalDeny, nil
	}
	res, _ := runNamed(r, "shell-exec", allowed)
	if len(asked) != 1 || res.status != "error" {
		t.Fatalf("shell-exec needs approval by its own entry: asked %v, result %s %q", asked, res.status, res.output)
	}
}

// An unfolded shell-exec is the shell's code, so its output gets the shell's
// history budget (head and tail kept), not the 2000-character head-only
// default of an unknown name: the end of a build or test log is where the
// errors are. The shell-exec output must match plain bash's exactly.
func TestUnfoldedShellKeepsTheShellOutputBudget(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses seq")
	}
	args := json.RawMessage(`{"command":"seq 1 20000"}`)
	plain, _ := newAliasTestRuntime(t)
	want, err := plain.executeWithTimeout(context.Background(), toolCall{Tool: "bash", Args: args}, map[string]struct{}{"bash": {}})
	if err != nil {
		t.Fatalf("plain bash: %v", err)
	}
	r, allowed := userToolRuntime(t, "bash", []string{"shell-exec"}, false)
	got, err := r.executeWithTimeout(context.Background(), toolCall{Tool: "shell-exec", Args: args}, allowed)
	if err != nil {
		t.Fatalf("unfolded shell-exec: %v", err)
	}
	// The two results differ only in the spool entry the footer names, so
	// the lengths match when the same budget applied.
	if !strings.Contains(got, "\n20000") {
		t.Errorf("unfolded shell-exec lost the tail of its output (%d chars)", len(got))
	}
	if len(got) != len(want) {
		t.Errorf("unfolded shell-exec kept %d chars, plain bash %d", len(got), len(want))
	}
}

// The loop detector signs a call by the tool it reaches: the operator's own
// ls is not glob.
func TestLoopCallsKeepUserToolNames(t *testing.T) {
	r, _ := userToolRuntime(t, "ls", nil, false)
	calls := r.loopCalls([]provider.NativeTool{{Name: "ls", Args: json.RawMessage(`{}`)}})
	if calls[0].Tool != "ls" {
		t.Fatalf("the user's ls signed as %s", calls[0].Tool)
	}
}
