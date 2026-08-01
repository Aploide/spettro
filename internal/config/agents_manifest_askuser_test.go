package config

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// preV10Manifest is a minimal manifest from before the ask-user grant: an
// orchestrator a human talks to, a primary agent that already holds the tool
// but not the "ask" action family (the shape the shipped default had, where
// the grant was inert), and a deliberately restricted worker.
const preV10Manifest = `
version = 9
default_agent = "planner"

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

[[agents]]
id = "planner"
name = "Planner"
description = "p"
skill = "planning"
mode = "orchestrator"
role = "orchestrator"
allowed_tools = ["file-read"]
permission = "ask-first"
permitted_actions = ["read", "plan"]
enabled = true

[[agents]]
id = "chat"
name = "Chat"
description = "c"
skill = "conversation"
mode = "chat"
role = "primary"
allowed_tools = ["ask-user"]
permission = "ask-first"
permitted_actions = ["read"]
enabled = true

[[agents]]
id = "worker"
name = "Worker"
description = "w"
skill = "analysis"
mode = "worker"
role = "worker"
allowed_tools = ["file-read"]
permission = "ask-first"
permitted_actions = ["read"]
enabled = true
`

func TestV10MigrationGrantsAskUserToPrimaryAgents(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, AgentManifestFilename)
	if err := os.WriteFile(path, []byte(preV10Manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := LoadAgentManifest(path)
	if err != nil {
		t.Fatal(err)
	}
	if m.Version < 10 {
		t.Fatalf("manifest not migrated: version %d", m.Version)
	}
	haveTool := false
	for _, tool := range m.Tools {
		if tool.ID == "ask-user" {
			haveTool = true
		}
	}
	if !haveTool {
		t.Fatal("ask-user definition not added")
	}

	for _, id := range []string{"planner", "chat"} {
		a, ok := m.AgentByID(id)
		if !ok {
			t.Fatalf("agent %q missing after migration", id)
		}
		if !slices.Contains(a.AllowedTools, "ask-user") {
			t.Fatalf("agent %q must gain the ask-user grant", id)
		}
		// The allow-list alone is inert: resolveToolPolicies drops a tool
		// whose action family the agent does not hold.
		if !slices.Contains(a.PermittedActions, "ask") {
			t.Fatalf("agent %q must gain the ask action family", id)
		}
	}

	w, _ := m.AgentByID("worker")
	if slices.Contains(w.AllowedTools, "ask-user") {
		t.Fatal("restricted worker must not gain ask-user")
	}
	if slices.Contains(w.PermittedActions, "ask") {
		t.Fatal("restricted worker must not gain the ask action family")
	}
}

// A second load must be a no-op: the grant is appended once, not on every
// migration pass.
func TestV10MigrationIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, AgentManifestFilename)
	if err := os.WriteFile(path, []byte(preV10Manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	first, err := LoadAgentManifestForProject(dir)
	if err != nil {
		t.Fatal(err)
	}
	second, err := LoadAgentManifestForProject(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range second.Agents {
		got := 0
		for _, id := range a.AllowedTools {
			if id == "ask-user" {
				got++
			}
		}
		if got > 1 {
			t.Fatalf("agent %q accumulated %d ask-user grants", a.ID, got)
		}
	}
	if len(first.Tools) != len(second.Tools) {
		t.Fatalf("tool list grew on re-migration: %d -> %d", len(first.Tools), len(second.Tools))
	}
}

func TestDefaultManifestGrantsAskUserToHumanFacingAgents(t *testing.T) {
	m := DefaultAgentManifest()
	granted := map[string]bool{}
	for _, a := range m.Agents {
		if slices.Contains(a.AllowedTools, "ask-user") {
			granted[a.ID] = true
			if !slices.Contains(a.PermittedActions, "ask") {
				t.Fatalf("agent %q holds ask-user without the ask action family", a.ID)
			}
		}
	}
	for _, id := range []string{"plan", "coding", "ask"} {
		if !granted[id] {
			t.Fatalf("agent %q must be able to ask the user a question", id)
		}
	}
	// Settled: workers and subagents stay without it, `code` included — a
	// nested worker's question would interrupt the user mid-orchestration.
	for _, id := range []string{"code", "explore", "git", "test", "review", "docs"} {
		if granted[id] {
			t.Fatalf("worker/subagent %q must not hold ask-user", id)
		}
	}
}
