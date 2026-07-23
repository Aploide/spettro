package config

import (
	"strings"
	"testing"
)

const ptyMigrationManifest = `
version = 7
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
id = "shell-exec"
name = "Shell Executor"
kind = "builtin"
enabled = true
timeout_sec = 5
requires_approval = true
permitted_actions = ["execute"]

[[agents]]
id = "coder"
name = "Coder"
mode = "orchestrator"
allowed_tools = ["file-read", "shell-exec"]
permission = "ask-first"
enabled = true

[[agents]]
id = "reader"
name = "Reader"
mode = "worker"
allowed_tools = ["file-read"]
permission = "ask-first"
enabled = true
`

func TestV8MigrationAddsPTYTools(t *testing.T) {
	m, _, changed, err := DecodeAgentManifestWithMigrationInfo(strings.NewReader(ptyMigrationManifest))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !changed || m.Version < 8 {
		t.Fatalf("expected v8 migration to fire (changed=%v version=%d)", changed, m.Version)
	}

	haveTool := map[string]bool{}
	for _, tool := range m.Tools {
		haveTool[tool.ID] = true
	}
	if !haveTool["pty-start"] || !haveTool["pty-write"] || !haveTool["pty-kill"] {
		t.Fatalf("migration must add the pty tool definitions, got %v", haveTool)
	}

	for _, ag := range m.Agents {
		found := map[string]bool{}
		for _, id := range ag.AllowedTools {
			found[id] = true
		}
		switch ag.ID {
		case "coder":
			if !found["pty-start"] || !found["pty-write"] || !found["pty-kill"] {
				t.Fatalf("coder (shell-exec) must gain the pty tools, got %v", ag.AllowedTools)
			}
		case "reader":
			if found["pty-start"] || found["pty-write"] || found["pty-kill"] {
				t.Fatalf("reader (no shell-exec) must gain no pty tools, got %v", ag.AllowedTools)
			}
		}
	}
}

func TestDefaultManifestIncludesPTYTools(t *testing.T) {
	m := DefaultAgentManifest()
	haveTool := map[string]bool{}
	for _, tool := range m.Tools {
		haveTool[tool.ID] = true
	}
	if !haveTool["pty-start"] || !haveTool["pty-write"] || !haveTool["pty-kill"] {
		t.Fatal("default manifest must define the pty tools")
	}
	for _, ag := range m.Agents {
		found := map[string]bool{}
		for _, id := range ag.AllowedTools {
			found[id] = true
		}
		if found["shell-exec"] && (!found["pty-start"] || !found["pty-write"] || !found["pty-kill"]) {
			t.Fatalf("agent %q holds shell-exec but lacks pty tools: %v", ag.ID, ag.AllowedTools)
		}
	}
}
