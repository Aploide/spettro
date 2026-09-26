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
		{aliasCall(t, "references", map[string]any{"path": "a.fk", "symbol": "Foo", "kind": "callers"}), `references args: kind must be "references" or "definition"`},
		{aliasCall(t, "references", map[string]any{"symbol": "Foo"}), "lsp references: path is required"},
		{aliasCall(t, "hover", map[string]any{"path": "a.fk"}), "lsp hover: symbol or line is required"},
		{toolCall{Tool: "diagnostics", Args: json.RawMessage(`[]`)}, "diagnostics args:"},
	} {
		_, err := rt.execute(ctx, c.call, allowed)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s %s: err = %v, want %q", c.call.Tool, c.call.Args, err, c.want)
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
		{toolCall{Tool: "references", Args: json.RawMessage(`{"path":"a.go","kind":""}`)}, `{"op":"references","path":"a.go"}`},
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

// A permission rule naming a retired language-server tool still denies the
// lsp op that replaced it (the v13 migration writes such rules for the ops an
// agent did not have), by tool or by permission family.
func TestRulesOnRetiredLSPToolsDenyTheirOp(t *testing.T) {
	rt := lspTestRuntime(t)
	spec := config.ToolSpec{ID: "lsp", Name: "LSP", Kind: "builtin", Enabled: true, PermittedActions: []string{"read", "search"}}
	rt.toolPolicies = map[string]config.ToolSpec{"lsp": spec}
	rt.agentRules = []config.PermissionRule{{Permission: "tool", Pattern: "lsp-restart", Action: config.RuleDeny}}
	rt.runtimeRules = []config.PermissionRule{{Permission: "search", Pattern: "hover", Action: config.RuleDeny}}
	allowed := map[string]struct{}{"lsp": {}}
	ctx := context.Background()
	for _, c := range []struct {
		call toolCall
		want string
	}{
		{aliasCall(t, "lsp", map[string]any{"op": "restart"}), `lsp op "restart" denied by policy`},
		{aliasCall(t, "lsp-restart", map[string]any{}), `lsp op "restart" denied by policy`},
		{aliasCall(t, "lsp", map[string]any{"op": "hover", "path": "a.fk", "symbol": "Foo"}), `lsp op "hover" denied by policy for permission "search"`},
	} {
		if _, err := rt.execute(ctx, c.call, allowed); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s %s: err = %v, want %q", c.call.Tool, c.call.Args, err, c.want)
		}
	}
	if out, err := rt.execute(ctx, aliasCall(t, "lsp", map[string]any{"op": "definition", "path": "a.fk", "symbol": "Foo"}), allowed); err != nil || out != "a.fk:1:1" {
		t.Fatalf("an op no rule names must run: %q, %v", out, err)
	}

	// A user's own tool called lsp is not the built-in: the rules stay its
	// own business.
	rt.toolPolicies["lsp"] = config.ToolSpec{ID: "lsp", Kind: "script"}
	if err := rt.lspOpDenied(aliasCall(t, "lsp", map[string]any{"op": "restart"}), rt.toolPolicies["lsp"]); err != nil {
		t.Fatalf("custom lsp tool: %v", err)
	}
}
