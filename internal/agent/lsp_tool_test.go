package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"spettro/internal/config"
	"spettro/internal/hooks"
	"spettro/internal/lsp/lsptest"
)

// lspTestRuntime is a runtime over a workspace served by the scripted
// language server, holding one file, a.fk, with an error on line 2 and the
// symbol Foo on line 3 (column 6).
func lspTestRuntime(t *testing.T) *toolRuntime {
	t.Helper()
	rt, dir := fakeLSPRuntime(t, lsptest.Options{})
	if err := os.WriteFile(filepath.Join(dir, "a.fk"), []byte("fine\nERR here\nfunc Foo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return rt
}

// Each retired language-server tool is the lsp op that replaced it: the same
// output for the same arguments, references' kind modes included.
func TestLSPAliasesMatchLSPOps(t *testing.T) {
	rt := lspTestRuntime(t)
	ctx := context.Background()
	allowed := map[string]struct{}{"lsp": {}}
	cases := []struct {
		legacy    toolCall
		canonical toolCall
		want      string
	}{
		{aliasCall(t, "diagnostics", map[string]any{"path": "a.fk"}), aliasCall(t, "lsp", map[string]any{"op": "diagnostics", "path": "a.fk"}), "a.fk:2:1 [error] bad thing: ERR here"},
		{aliasCall(t, "diagnostics", map[string]any{}), aliasCall(t, "lsp", map[string]any{"op": "diagnostics"}), "a.fk:2:1"},
		{aliasCall(t, "references", map[string]any{"path": "a.fk", "symbol": "Foo"}), aliasCall(t, "lsp", map[string]any{"op": "references", "path": "a.fk", "symbol": "Foo"}), "a.fk:1:1\na.fk:3:6"},
		{aliasCall(t, "references", map[string]any{"path": "a.fk", "symbol": "Foo", "kind": "references"}), aliasCall(t, "lsp", map[string]any{"op": "references", "path": "a.fk", "symbol": "Foo"}), "a.fk:1:1\na.fk:3:6"},
		{aliasCall(t, "references", map[string]any{"path": "a.fk", "line": 3, "character": 6, "kind": "definition"}), aliasCall(t, "lsp", map[string]any{"op": "definition", "path": "a.fk", "line": 3, "character": 6}), "a.fk:1:1"},
		{aliasCall(t, "references", map[string]any{"path": "a.fk", "symbol": "Foo", "kind": "definition"}), aliasCall(t, "lsp", map[string]any{"op": "references", "path": "a.fk", "symbol": "Foo", "kind": "definition"}), "a.fk:1:1"},
		{aliasCall(t, "hover", map[string]any{"path": "a.fk", "symbol": "Foo"}), aliasCall(t, "lsp", map[string]any{"op": "hover", "path": "a.fk", "symbol": "Foo"}), "hover 2:5"},
		{aliasCall(t, "hover", map[string]any{"path": "a.fk", "line": 1}), aliasCall(t, "lsp", map[string]any{"op": "HOVER", "path": "a.fk", "line": 1}), "hover 0:0"},
	}
	for _, c := range cases {
		got, err := rt.execute(ctx, c.legacy, allowed)
		if err != nil {
			t.Fatalf("%s %s: %v", c.legacy.Tool, c.legacy.Args, err)
		}
		want, err := rt.execute(ctx, c.canonical, allowed)
		if err != nil {
			t.Fatalf("lsp %s: %v", c.canonical.Args, err)
		}
		if got != want {
			t.Errorf("%s %s = %q, lsp %s = %q", c.legacy.Tool, c.legacy.Args, got, c.canonical.Args, want)
		}
		if !strings.Contains(want, c.want) {
			t.Errorf("lsp %s = %q, want it to contain %q", c.canonical.Args, want, c.want)
		}
	}

	// A restart stops the server, so each one gets a running server to stop.
	var restarts []string
	for _, call := range []toolCall{
		aliasCall(t, "lsp-restart", map[string]any{"server": "fake"}),
		aliasCall(t, "lsp", map[string]any{"op": "restart", "server": "fake"}),
	} {
		if _, err := rt.execute(ctx, aliasCall(t, "lsp", map[string]any{"op": "hover", "path": "a.fk", "line": 1}), allowed); err != nil {
			t.Fatal(err)
		}
		out, err := rt.execute(ctx, call, allowed)
		if err != nil {
			t.Fatalf("%s %s: %v", call.Tool, call.Args, err)
		}
		restarts = append(restarts, out)
	}
	if restarts[0] != restarts[1] || !strings.HasPrefix(restarts[0], "restarted lsp server(s): fake") {
		t.Errorf("lsp-restart = %q, lsp restart = %q", restarts[0], restarts[1])
	}
}

// The old tools' argument errors stay errors, and lsp names what it needs.
func TestLSPArgumentErrors(t *testing.T) {
	rt := lspTestRuntime(t)
	ctx := context.Background()
	allowed := map[string]struct{}{"lsp": {}}
	for _, c := range []struct {
		call toolCall
		want string
	}{
		{aliasCall(t, "lsp", map[string]any{}), "lsp: op is required (diagnostics, references, definition, hover or restart)"},
		{aliasCall(t, "lsp", map[string]any{"op": "rename"}), `lsp: unknown op "rename"`},
		{aliasCall(t, "lsp", map[string]any{"op": "hover", "symbol": "Foo"}), "lsp hover: path is required"},
		{aliasCall(t, "lsp", map[string]any{"op": "definition", "path": "a.fk"}), "lsp definition: symbol or line is required"},
		{aliasCall(t, "lsp", map[string]any{"op": "references", "path": "a.fk", "symbol": "Foo", "kind": "callers"}), `kind must be "references" or "definition"`},
		// Called by an old name, the errors are the old tool's, checked in
		// its order (path and position before kind).
		{aliasCall(t, "references", map[string]any{"path": "a.fk", "symbol": "Foo", "kind": "callers"}), `references: kind must be "references" or "definition"`},
		{aliasCall(t, "references", map[string]any{"kind": "callers"}), "references: path is required"},
		{aliasCall(t, "references", map[string]any{"path": "a.fk", "kind": 7}), "references args: json: cannot unmarshal"},
		{aliasCall(t, "references", map[string]any{"symbol": "Foo"}), "references: path is required"},
		{aliasCall(t, "hover", map[string]any{"path": "a.fk"}), "hover: symbol or line is required"},
		{toolCall{Tool: "diagnostics", Args: json.RawMessage(`[]`)}, "diagnostics args:"},
	} {
		_, err := rt.execute(ctx, c.call, allowed)
		if err == nil || !strings.HasPrefix(err.Error(), c.want) && !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s %s: err = %v, want %q", c.call.Tool, c.call.Args, err, c.want)
		}
		if c.call.Tool != "lsp" && err != nil && strings.HasPrefix(err.Error(), "lsp") {
			t.Errorf("%s %s: err = %v, want the old tool's wording", c.call.Tool, c.call.Args, err)
		}
	}
}

// Each op reads only its own arguments, as the tool it replaced did, so an
// argument of another op, of any type, never fails the call.
func TestLSPOpsIgnoreOtherOpsArguments(t *testing.T) {
	rt := lspTestRuntime(t)
	ctx := context.Background()
	allowed := map[string]struct{}{"lsp": {}}
	for _, c := range []struct {
		call toolCall
		want string
	}{
		{toolCall{Tool: "diagnostics", Args: json.RawMessage(`{"path":"a.fk","line":"3"}`)}, "bad thing"},
		{toolCall{Tool: "lsp", Args: json.RawMessage(`{"op":"diagnostics","path":"a.fk","symbol":1,"server":false}`)}, "bad thing"},
		{toolCall{Tool: "hover", Args: json.RawMessage(`{"path":"a.fk","symbol":"Foo","server":1}`)}, "hover 2:5"},
		{toolCall{Tool: "references", Args: json.RawMessage(`{"path":"a.fk","symbol":"Foo","server":[1]}`)}, "a.fk:3:6"},
		{toolCall{Tool: "lsp-restart", Args: json.RawMessage(`{"server":"fake","path":1}`)}, "fake"},
		{toolCall{Tool: "lsp", Args: json.RawMessage(`{"op":"restart","line":"x"}`)}, ""},
	} {
		out, err := rt.execute(ctx, c.call, allowed)
		if err != nil || !strings.Contains(out, c.want) {
			t.Errorf("%s %s = %q, %v; want it to contain %q", c.call.Tool, c.call.Args, out, err, c.want)
		}
	}
}

// A retired call becomes the lsp call its op names; nothing else about the
// arguments changes, and an "op" the old tool would have ignored does not
// pick another op.
func TestLSPAliasArgs(t *testing.T) {
	for _, c := range []struct {
		call toolCall
		want string
	}{
		{toolCall{Tool: "diagnostics", Args: json.RawMessage(`{}`)}, `{"op":"diagnostics"}`},
		{toolCall{Tool: "diagnostics", Args: json.RawMessage(`null`)}, `{"op":"diagnostics"}`},
		{toolCall{Tool: "hover", Args: json.RawMessage(`{"path":"a.go","line":3,"op":"restart"}`)}, `{"line":3,"op":"hover","path":"a.go"}`},
		{toolCall{Tool: "references", Args: json.RawMessage(`{"path":"a.go","kind":"definition","symbol":"X"}`)}, `{"op":"definition","path":"a.go","symbol":"X"}`},
		{toolCall{Tool: "references", Args: json.RawMessage(`{"path":"a.go","kind":""}`)}, `{"kind":"","op":"references","path":"a.go"}`},
		// Any other kind stays for the lsp tool to reject, as references did.
		{toolCall{Tool: "references", Args: json.RawMessage(`{"path":"a.go","kind":"callers"}`)}, `{"kind":"callers","op":"references","path":"a.go"}`},
		{toolCall{Tool: "lsp-restart", Args: json.RawMessage(`{"server":"gopls"}`)}, `{"op":"restart","server":"gopls"}`},
	} {
		got, err := canonicalToolCall(c.call)
		if err != nil {
			t.Fatalf("%s %s: %v", c.call.Tool, c.call.Args, err)
		}
		if got.Tool != "lsp" || got.CalledAs != c.call.Tool || string(got.Args) != c.want {
			t.Errorf("%s %s -> %s %s (as %q), want lsp %s", c.call.Tool, c.call.Args, got.Tool, got.Args, got.CalledAs, c.want)
		}
	}
	if got := LegacyToolNames("lsp"); strings.Join(got, ",") != "diagnostics,hover,lsp-restart,references" {
		t.Errorf("LegacyToolNames(lsp) = %v", got)
	}
}

// Lookups run together with the other read-only calls; a restart runs alone,
// as lsp-restart did, so it never stops a server under a concurrent lookup.
func TestLSPRestartRunsAlone(t *testing.T) {
	lspCall := func(op string) toolCall {
		return toolCall{Tool: "lsp", Args: json.RawMessage(`{"op":"` + op + `"}`)}
	}
	calls := []toolCall{lspCall("diagnostics"), {Tool: "file-read"}, lspCall("hover"), lspCall("restart"), lspCall("references"), lspCall("definition")}
	got := planToolBatches(calls, []int{0, 1, 2, 3, 4, 5})
	if want := [][]int{{0, 1, 2}, {3}, {4, 5}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("batches = %v, want %v", got, want)
	}
}

// A hook written for a retired language-server tool fires on the lsp op that
// replaced it, whether the model calls the old name or lsp, and on no other
// op.
func TestHooksForRetiredLSPToolsFireOnTheirOp(t *testing.T) {
	rt := lspTestRuntime(t)
	rt.hooksConfig = hooks.EffectiveConfig{Rules: []hooks.EffectiveRule{
		hookRule("no-restart", hooks.EventPreToolUse, "lsp-restart", `echo '{"decision":"deny","reason":"no restarts"}'`),
		hookRule("no-refs", hooks.EventPreToolUse, "references", `echo '{"decision":"deny","reason":"no lookups"}'`),
	}}
	allowed := map[string]struct{}{"lsp": {}}
	for _, c := range []struct {
		call toolCall
		want string // "" when the call must go through
	}{
		{aliasCall(t, "lsp", map[string]any{"op": "restart"}), "no restarts"},
		{aliasCall(t, "lsp-restart", map[string]any{}), "no restarts"},
		{aliasCall(t, "lsp", map[string]any{"op": "definition", "path": "a.fk", "symbol": "Foo"}), "no lookups"},
		{aliasCall(t, "references", map[string]any{"path": "a.fk", "symbol": "Foo"}), "no lookups"},
		{aliasCall(t, "lsp", map[string]any{"op": "hover", "path": "a.fk", "symbol": "Foo"}), ""},
		{aliasCall(t, "diagnostics", map[string]any{"path": "a.fk"}), ""},
	} {
		res := rt.parallelExec(context.Background(), []toolCall{c.call}, allowed, nil)
		switch {
		case c.want == "" && res[0].status == "error":
			t.Errorf("%s %s blocked: %q", c.call.Tool, c.call.Args, res[0].output)
		case c.want != "" && (res[0].status != "error" || !strings.Contains(res[0].output, c.want)):
			t.Errorf("%s %s not blocked by its hook: %s %q", c.call.Tool, c.call.Args, res[0].status, res[0].output)
		}
	}
}

// lsp-op rules pick the ops an agent may call, and only they do: a rule
// naming a retired tool, or a catch-all rule the lsp tool is an exception to,
// never takes an op away, and a rule of the tool's own that asks does not
// give one back.
func TestLSPOpRulesDecideTheOps(t *testing.T) {
	rt := lspTestRuntime(t)
	spec := config.ToolSpec{ID: "lsp", Name: "LSP", Kind: "builtin", Enabled: true, PermittedActions: []string{"read", "search"},
		PermissionRules: []config.PermissionRule{{Permission: "tool", Pattern: "*", Action: config.RuleAsk}}}
	rt.toolPolicies = map[string]config.ToolSpec{"lsp": spec}
	allowed := map[string]struct{}{"lsp": {}}
	ctx := context.Background()
	rule := func(perm, pat string, action config.RuleAction) config.PermissionRule {
		return config.PermissionRule{Permission: perm, Pattern: pat, Action: action}
	}
	hover := aliasCall(t, "lsp", map[string]any{"op": "hover", "path": "a.fk", "symbol": "Foo"})
	definition := aliasCall(t, "lsp", map[string]any{"op": "definition", "path": "a.fk", "symbol": "Foo"})
	restart := aliasCall(t, "lsp", map[string]any{"op": "restart", "server": "none"})
	for _, c := range []struct {
		name          string
		runtime, agnt []config.PermissionRule
		denied        []toolCall
		runs          []toolCall
	}{
		{
			name:   "an agent-level lsp-op deny, the tool's own ask rule after it",
			agnt:   []config.PermissionRule{rule(config.LSPOpPermission, "restart", config.RuleDeny), rule(config.LSPOpPermission, "references", config.RuleDeny), rule(config.LSPOpPermission, "definition", config.RuleDeny)},
			denied: []toolCall{restart, aliasCall(t, "lsp-restart", map[string]any{}), definition, aliasCall(t, "references", map[string]any{"path": "a.fk", "symbol": "Foo"})},
			runs:   []toolCall{hover},
		},
		{
			name:    "a runtime lsp-op deny, lifted for one op by the agent",
			runtime: []config.PermissionRule{rule(config.LSPOpPermission, "*", config.RuleDeny)},
			agnt:    []config.PermissionRule{rule(config.LSPOpPermission, "hover", config.RuleAllow)},
			denied:  []toolCall{restart, definition},
			runs:    []toolCall{hover},
		},
		{
			name: "an allow-list written as a tool deny-all plus an lsp allow",
			agnt: []config.PermissionRule{rule("tool", "*", config.RuleDeny), rule("tool", "lsp", config.RuleAllow)},
			runs: []toolCall{hover, definition, restart},
		},
		{
			name: "the same by permission family",
			agnt: []config.PermissionRule{rule("search", "*", config.RuleDeny), rule("search", "lsp", config.RuleAllow)},
			runs: []toolCall{hover, definition, restart},
		},
		{
			name: "a deny-all on every permission, lsp allowed on every permission",
			agnt: []config.PermissionRule{rule("*", "*", config.RuleDeny), rule("*", "lsp", config.RuleAllow)},
			runs: []toolCall{hover, definition, restart},
		},
		{
			name: "rules naming the retired tools",
			agnt: []config.PermissionRule{rule("tool", "hover", config.RuleDeny), rule("tool", "references", config.RuleDeny), rule("tool", "lsp-restart", config.RuleDeny)},
			runs: []toolCall{hover, definition, restart},
		},
	} {
		rt.runtimeRules, rt.agentRules = c.runtime, c.agnt
		for _, call := range c.denied {
			if _, err := rt.execute(ctx, call, allowed); err == nil || !strings.Contains(err.Error(), "denied by policy (a lsp-op rule denies it)") {
				t.Errorf("%s: %s %s: err = %v, want an lsp-op deny", c.name, call.Tool, call.Args, err)
			}
		}
		for _, call := range c.runs {
			if _, err := rt.execute(ctx, call, allowed); err != nil {
				t.Errorf("%s: %s %s: %v", c.name, call.Tool, call.Args, err)
			}
		}
	}

	// A user's own tool called lsp is not the built-in: the rules stay its
	// own business.
	rt.agentRules = []config.PermissionRule{rule(config.LSPOpPermission, "*", config.RuleDeny)}
	rt.toolPolicies["lsp"] = config.ToolSpec{ID: "lsp", Kind: "script"}
	if err := rt.lspOpDenied(restart, rt.toolPolicies["lsp"]); err != nil {
		t.Fatalf("custom lsp tool: %v", err)
	}
}

// A retired name the operator's own tool answers to belongs to that tool: a
// call to it is not rewritten into lsp, and neither its hooks nor its rules
// reach lsp {op: "hover"}.
func TestUserToolSharingARetiredLSPName(t *testing.T) {
	rt := lspTestRuntime(t)
	lspSpec := config.ToolSpec{ID: "lsp", Name: "LSP", Kind: "builtin", Enabled: true, PermittedActions: []string{"read", "search"}, Aliases: []string{"diagnostics", "references", "lsp-restart"}}
	hoverScript := config.ToolSpec{ID: "hover", Name: "My hover", Kind: "script", EntryPoint: "./hover.sh", Enabled: true, PermittedActions: []string{"read"}}
	rt.manifest = &config.AgentManifest{Tools: []config.ToolSpec{lspSpec, hoverScript}}
	rt.toolPolicies = map[string]config.ToolSpec{"lsp": lspSpec}
	rt.agentRules = []config.PermissionRule{{Permission: "tool", Pattern: "hover", Action: config.RuleDeny}}
	rt.hooksConfig = hooks.EffectiveConfig{Rules: []hooks.EffectiveRule{
		hookRule("guard-script", hooks.EventPreToolUse, "hover", `echo '{"decision":"deny","reason":"script hook"}'`),
	}}
	call := toolCall{Tool: "hover", Args: json.RawMessage(`{"path":"a.fk","symbol":"Foo"}`)}
	if got, err := rt.canonicalCall(call); err != nil || got.Tool != "hover" || got.CalledAs != "" {
		t.Fatalf("a call of the user's hover became %+v, %v", got, err)
	}
	res := rt.parallelExec(context.Background(), []toolCall{aliasCall(t, "lsp", map[string]any{"op": "hover", "path": "a.fk", "symbol": "Foo"})}, map[string]struct{}{"lsp": {}}, nil)
	if res[0].status == "error" || !strings.Contains(res[0].output, "hover 2:5") {
		t.Fatalf("lsp hover blocked by the user's hover tool's hook or rule: %s %q", res[0].status, res[0].output)
	}
}

// With a tool of the operator's own called lsp, v13 leaves the language-server
// built-ins unfolded: the one an agent holds runs under its own name and is
// advertised under it, and a retired name is never turned into a call of the
// operator's lsp.
func TestUnfoldedLSPToolsKeepWorking(t *testing.T) {
	rt := lspTestRuntime(t)
	userLSP := config.ToolSpec{ID: "lsp", Name: "My LSP", Kind: "script", EntryPoint: "./lsp.sh", Enabled: true, PermittedActions: []string{"read"}}
	diag := config.ToolSpec{ID: "diagnostics", Name: "LSP Diagnostics", Kind: "builtin", Enabled: true, PermittedActions: []string{"read", "search"}}
	rt.manifest = &config.AgentManifest{Tools: []config.ToolSpec{userLSP, diag}}
	rt.toolPolicies = map[string]config.ToolSpec{"diagnostics": diag, "lsp": userLSP}
	ctx := context.Background()

	call := toolCall{Tool: "diagnostics", Args: json.RawMessage(`{"path":"a.fk"}`)}
	if got, err := rt.canonicalCall(call); err != nil || got.Tool != "diagnostics" {
		t.Fatalf("diagnostics became %+v, %v", got, err)
	}
	out, err := rt.execute(ctx, call, map[string]struct{}{"diagnostics": {}})
	if err != nil || !strings.Contains(out, "bad thing") {
		t.Fatalf("diagnostics = %q, %v", out, err)
	}
	for _, name := range []string{"lsp-restart", "hover"} {
		res := rt.parallelExec(ctx, []toolCall{{Tool: name, Args: json.RawMessage(`{}`)}}, map[string]struct{}{"diagnostics": {}, "lsp": {}}, nil)
		if res[0].status != "error" || !strings.Contains(res[0].output, `tool "`+name+`" not allowed`) {
			t.Errorf("%s, not held, must not reach the user's lsp: %s %q", name, res[0].status, res[0].output)
		}
	}
	specs := rt.unfoldedLSPToolSpecs([]string{"file-read", "diagnostics", "lsp"})
	if len(specs) != 1 || specs[0].Name != "diagnostics" || len(specs[0].Schema) == 0 || specs[0].Description == "" {
		t.Fatalf("advertised = %+v, want diagnostics alone", specs)
	}
	if !concurrentCall(call) || concurrentCall(toolCall{Tool: "lsp-restart"}) {
		t.Fatal("an unfolded lookup runs with its neighbours, a restart alone")
	}

	// Folded (the lsp tool is the built-in), nothing is advertised under the
	// old names.
	rt.manifest = &config.AgentManifest{Tools: []config.ToolSpec{{ID: "lsp", Kind: "builtin", Aliases: []string{"diagnostics"}}}}
	rt.toolPolicies = map[string]config.ToolSpec{"lsp": rt.manifest.Tools[0], "diagnostics": rt.manifest.Tools[0]}
	if specs := rt.unfoldedLSPToolSpecs([]string{"diagnostics", "lsp"}); len(specs) != 0 {
		t.Fatalf("folded: advertised %+v", specs)
	}
}
