package config

import (
	"os"
	"path/filepath"
	"testing"
)

// preV9Manifest is a minimal manifest from before the tool-output tool
// existed, with one agent trusted with file-read and one without.
const preV9Manifest = `
version = 8
default_agent = "reader"

[metadata]
name = "t"
description = "t"

[runtime]
default_permission = "ask-first"
default_timeout_sec = 60

[[tools]]
id = "file-read"
name = "File Reader"
description = "reads"
kind = "builtin"
enabled = true
timeout_sec = 30
permitted_actions = ["read"]

[[tools]]
id = "comment"
name = "Comment"
description = "notes"
kind = "builtin"
enabled = true
timeout_sec = 5
permitted_actions = ["read"]

[[agents]]
id = "reader"
name = "Reader"
description = "r"
skill = "analysis"
mode = "worker"
allowed_tools = ["file-read"]
permission = "ask-first"
permitted_actions = ["read"]
enabled = true

[[agents]]
id = "talker"
name = "Talker"
description = "t"
skill = "conversation"
mode = "worker"
allowed_tools = ["comment"]
permission = "ask-first"
permitted_actions = ["read"]
enabled = true
`

func TestV9MigrationAddsToolOutput(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, AgentManifestFilename)
	if err := os.WriteFile(path, []byte(preV9Manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := LoadAgentManifest(path)
	if err != nil {
		t.Fatal(err)
	}
	if m.Version < 9 {
		t.Fatalf("manifest not migrated: version %d", m.Version)
	}
	haveTool := false
	for _, tool := range m.Tools {
		if tool.ID == "tool-output" {
			haveTool = true
		}
	}
	if !haveTool {
		t.Fatal("tool-output definition not added")
	}
	for _, a := range m.Agents {
		allowed := map[string]bool{}
		for _, id := range a.AllowedTools {
			allowed[id] = true
		}
		switch a.ID {
		case "reader":
			if !allowed["tool-output"] {
				t.Fatal("file-read agent must gain tool-output")
			}
		case "talker":
			if allowed["tool-output"] {
				t.Fatal("agent without file-read must not gain tool-output")
			}
		}
	}
}

func TestDefaultManifestGrantsToolOutputWithFileRead(t *testing.T) {
	m := DefaultAgentManifest()
	for _, a := range m.Agents {
		allowed := map[string]bool{}
		for _, id := range a.AllowedTools {
			allowed[id] = true
		}
		if allowed["file-read"] && !allowed["tool-output"] {
			t.Fatalf("agent %q has file-read but not tool-output", a.ID)
		}
	}
}
