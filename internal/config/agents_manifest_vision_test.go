package config

import (
	"strings"
	"testing"
)

// visionMigrationManifest is a v3 manifest predating the view-image tool: one
// agent with the gating tool (file-read), one without.
const visionMigrationManifest = `
version = 3
default_agent = "reader"

[runtime]
default_permission = "ask-first"
default_timeout_sec = 120

[[tools]]
id = "file-read"
name = "File Reader"
kind = "builtin"
enabled = true
timeout_sec = 30
permitted_actions = ["read"]

[[tools]]
id = "comment"
name = "Comment"
kind = "builtin"
enabled = true
timeout_sec = 5
permitted_actions = ["read"]

[[agents]]
id = "reader"
name = "Reader"
mode = "orchestrator"
allowed_tools = ["file-read"]
permission = "ask-first"
enabled = true

[[agents]]
id = "blind"
name = "Blind"
mode = "worker"
allowed_tools = ["comment"]
permission = "ask-first"
enabled = true
`

func TestV5MigrationAddsViewImage(t *testing.T) {
	m, _, changed, err := DecodeAgentManifestWithMigrationInfo(strings.NewReader(visionMigrationManifest))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !changed || m.Version < 5 {
		t.Fatalf("expected v5 migration to fire (changed=%v version=%d)", changed, m.Version)
	}

	found := false
	for _, tool := range m.Tools {
		if tool.ID == "view-image" {
			found = true
		}
	}
	if !found {
		t.Fatal("migration must add the view-image tool definition")
	}

	allowed := func(agentID string) map[string]bool {
		for _, a := range m.Agents {
			if a.ID == agentID {
				set := map[string]bool{}
				for _, id := range a.AllowedTools {
					set[id] = true
				}
				return set
			}
		}
		t.Fatalf("agent %q missing", agentID)
		return nil
	}
	if !allowed("reader")["view-image"] {
		t.Fatal("reader (has file-read) should gain view-image")
	}
	if allowed("blind")["view-image"] {
		t.Fatal("blind agent (no file-read) must not gain view-image")
	}
}

func TestV5MigrationDoesNotDuplicateExistingTool(t *testing.T) {
	// A manifest that already defines its own view-image tool must keep it
	// (the migration only fills gaps).
	custom := visionMigrationManifest + `
[[tools]]
id = "view-image"
name = "My View Image"
kind = "builtin"
enabled = true
timeout_sec = 30
permitted_actions = ["read"]
`
	m, _, _, err := DecodeAgentManifestWithMigrationInfo(strings.NewReader(custom))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	n := 0
	for _, tool := range m.Tools {
		if tool.ID == "view-image" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("expected exactly one view-image tool after migration, got %d", n)
	}
}

func TestDefaultManifestHasViewImage(t *testing.T) {
	m := DefaultAgentManifest()
	found := false
	for _, tool := range m.Tools {
		if tool.ID == "view-image" {
			found = true
		}
	}
	if !found {
		t.Fatal("default manifest must define view-image")
	}
	for _, a := range m.Agents {
		set := map[string]bool{}
		for _, id := range a.AllowedTools {
			set[id] = true
		}
		if set["file-read"] && !set["view-image"] {
			t.Errorf("agent %s has file-read but no view-image", a.ID)
		}
	}
}
