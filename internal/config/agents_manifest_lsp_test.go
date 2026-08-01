package config

import (
	"strings"
	"testing"
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

func TestV6MigrationAddsLSPDeepTools(t *testing.T) {
	m, _, changed, err := DecodeAgentManifestWithMigrationInfo(strings.NewReader(lspMigrationManifest))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !changed || m.Version < 6 {
		t.Fatalf("expected v6 migration to fire (changed=%v version=%d)", changed, m.Version)
	}

	haveTool := map[string]bool{}
	for _, tool := range m.Tools {
		haveTool[tool.ID] = true
	}
	if !haveTool["hover"] || !haveTool["rename-symbol"] {
		t.Fatalf("migration must add hover and rename-symbol tool definitions, got %v", haveTool)
	}

	allowed := func(agentID string) map[string]bool {
		for _, ag := range m.Agents {
			if ag.ID == agentID {
				out := map[string]bool{}
				for _, id := range ag.AllowedTools {
					out[id] = true
				}
				return out
			}
		}
		t.Fatalf("agent %q not found", agentID)
		return nil
	}

	coder := allowed("coder")
	if !coder["hover"] || !coder["rename-symbol"] {
		t.Fatalf("coder (references + file-edit) must gain both tools, got %v", coder)
	}
	reader := allowed("reader")
	if !reader["hover"] || reader["rename-symbol"] {
		t.Fatalf("reader (references only) must gain hover but not rename-symbol, got %v", reader)
	}
	blind := allowed("blind")
	if blind["hover"] || blind["rename-symbol"] {
		t.Fatalf("blind (no references) must gain neither tool, got %v", blind)
	}
}

func TestDefaultManifestIncludesLSPDeepTools(t *testing.T) {
	m := DefaultAgentManifest()
	haveTool := map[string]bool{}
	for _, tool := range m.Tools {
		haveTool[tool.ID] = true
	}
	if !haveTool["hover"] || !haveTool["rename-symbol"] {
		t.Fatal("default manifest must define hover and rename-symbol")
	}
	for _, ag := range m.Agents {
		if ag.ID != "code" && ag.ID != "coding" {
			continue
		}
		found := map[string]bool{}
		for _, id := range ag.AllowedTools {
			found[id] = true
		}
		if !found["hover"] || !found["rename-symbol"] {
			t.Fatalf("agent %q must allow hover and rename-symbol, got %v", ag.ID, ag.AllowedTools)
		}
	}
}
