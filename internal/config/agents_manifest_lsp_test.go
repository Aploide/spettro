package config

import (
	"slices"
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2"
)

const lspMigrationManifest = `
version = 5
default_agent = "coder"

[metadata]
name = "test"

[runtime]
default_permission = "ask-first"
default_timeout_sec = 60

[[tools]]
id = "file-read"
name = "File Reader"
kind = "builtin"
enabled = true
timeout_sec = 5
permitted_actions = ["read"]

[[tools]]
id = "file-edit"
name = "File Edit"
kind = "builtin"
enabled = true
timeout_sec = 5
permitted_actions = ["write"]

[[tools]]
id = "references"
name = "LSP References"
kind = "builtin"
enabled = true
timeout_sec = 5
permitted_actions = ["read", "search"]

[[agents]]
id = "coder"
name = "Coder"
mode = "orchestrator"
allowed_tools = ["file-read", "file-edit", "references"]
permission = "ask-first"
enabled = true

[[agents]]
id = "reader"
name = "Reader"
mode = "worker"
allowed_tools = ["file-read", "references"]
permission = "ask-first"
enabled = true

[[agents]]
id = "blind"
name = "Blind"
mode = "worker"
allowed_tools = ["file-read"]
permission = "ask-first"
enabled = true
`

// agentAllowed returns an agent's allow-list as a set.
func agentAllowed(t *testing.T, m AgentManifest, agentID string) map[string]bool {
	t.Helper()
	a, ok := m.AgentByID(agentID)
	if !ok {
		t.Fatalf("agent %q not found", agentID)
	}
	out := map[string]bool{}
	for _, id := range a.AllowedTools {
		out[id] = true
	}
	return out
}

// The v6 retrofit on its own: hover for agents holding references, and
// rename-symbol for those also holding file-edit.
func TestV6RetrofitAddsLSPDeepTools(t *testing.T) {
	var m AgentManifest
	if err := toml.Unmarshal([]byte(lspMigrationManifest), &m); err != nil {
		t.Fatal(err)
	}
	m.ensureLSPDeepTools()
	if !hasTool(m, "hover") || !hasTool(m, "rename-symbol") {
		t.Fatalf("v6 must add hover and rename-symbol tool definitions, got %v", m.Tools)
	}
	if coder := agentAllowed(t, m, "coder"); !coder["hover"] || !coder["rename-symbol"] {
		t.Fatalf("coder (references + file-edit) must gain both tools, got %v", coder)
	}
	if reader := agentAllowed(t, m, "reader"); !reader["hover"] || reader["rename-symbol"] {
		t.Fatalf("reader (references only) must gain hover but not rename-symbol, got %v", reader)
	}
	if blind := agentAllowed(t, m, "blind"); blind["hover"] || blind["rename-symbol"] {
		t.Fatalf("blind (no references) must gain neither tool, got %v", blind)
	}
}

// Through every migration: v6 adds hover and rename-symbol, v13 folds
// references and hover into lsp.
func TestV6MigrationAddsLSPDeepTools(t *testing.T) {
	m, _, changed, err := DecodeAgentManifestWithMigrationInfo(strings.NewReader(lspMigrationManifest))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !changed || m.Version != latestManifestVersion {
		t.Fatalf("expected the migrations to fire (changed=%v version=%d)", changed, m.Version)
	}
	if !hasTool(m, "lsp") || !hasTool(m, "rename-symbol") || hasTool(m, "hover") || hasTool(m, "references") {
		t.Fatalf("want lsp and rename-symbol defined, hover and references folded, got %v", m.Tools)
	}
	if coder := agentAllowed(t, m, "coder"); !coder["lsp"] || !coder["rename-symbol"] {
		t.Fatalf("coder (references + file-edit) must hold lsp and rename-symbol, got %v", coder)
	}
	if reader := agentAllowed(t, m, "reader"); !reader["lsp"] || reader["rename-symbol"] {
		t.Fatalf("reader (references only) must hold lsp but not rename-symbol, got %v", reader)
	}
	if blind := agentAllowed(t, m, "blind"); blind["lsp"] || blind["rename-symbol"] {
		t.Fatalf("blind (no references) must hold neither tool, got %v", blind)
	}
}

func TestDefaultManifestIncludesLSPDeepTools(t *testing.T) {
	m := DefaultAgentManifest()
	lsp := toolByID(t, m, "lsp")
	if !slices.Contains(lsp.Aliases, "hover") || !hasTool(m, "rename-symbol") || hasTool(m, "hover") {
		t.Fatalf("default manifest must define lsp (answering to hover) and rename-symbol, got lsp aliases %v", lsp.Aliases)
	}
	for _, id := range []string{"code", "coding"} {
		if found := agentAllowed(t, m, id); !found["lsp"] || !found["rename-symbol"] {
			t.Fatalf("agent %q must allow lsp and rename-symbol, got %v", id, found)
		}
	}
}
