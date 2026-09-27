package config

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// preV11Manifest is a minimal manifest from before the general-purpose
// subagent: an orchestrator that already delegates (and can read and search,
// all the new agent can do here), one whose handoffs an operator
// deliberately emptied, and a worker. It declares only a subset of
// the tools the shipped spec asks for, so the migration has to drop the rest
// instead of writing an allow-list Validate would reject.
const preV11Manifest = `
version = 10
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

[[tools]]
id = "grep"
name = "Grep"
description = "searches"
kind = "builtin"
enabled = true
timeout_sec = 30
permitted_actions = ["read", "search"]

[[agents]]
id = "planner"
name = "Planner"
description = "p"
skill = "planning"
mode = "orchestrator"
role = "orchestrator"
allowed_tools = ["file-read", "grep"]
permission = "ask-first"
permitted_actions = ["read", "search", "plan"]
handoffs = ["worker"]
enabled = true

[[agents]]
id = "solo"
name = "Solo"
description = "s"
skill = "conversation"
mode = "orchestrator"
role = "primary"
allowed_tools = ["file-read"]
permission = "ask-first"
permitted_actions = ["read"]
handoffs = []
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

func TestV11MigrationAddsGeneralPurposeAgent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, AgentManifestFilename)
	if err := os.WriteFile(path, []byte(preV11Manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := LoadAgentManifest(path)
	if err != nil {
		t.Fatal(err)
	}
	if m.Version < 11 {
		t.Fatalf("manifest not migrated: version %d", m.Version)
	}
	gp, ok := m.AgentByID("general-purpose")
	if !ok {
		t.Fatal("general-purpose agent not added")
	}
	if gp.Role != AgentRoleSubagent {
		t.Fatalf("role = %q, want subagent (reachable from orchestrators and workers alike)", gp.Role)
	}
	// Only the tools this manifest actually declares survive; Validate
	// rejects an agent that references an unknown tool.
	if !slices.Equal(gp.AllowedTools, []string{"grep", "file-read"}) {
		t.Fatalf("allowed_tools = %v, want only the declared tools", gp.AllowedTools)
	}
	if err := m.Validate(); err != nil {
		t.Fatalf("migrated manifest must validate: %v", err)
	}

	planner, _ := m.AgentByID("planner")
	if !slices.Contains(planner.Handoffs, "general-purpose") {
		t.Fatalf("delegating orchestrator must gain the handoff, got %v", planner.Handoffs)
	}
	// An operator who emptied an agent's handoffs turned delegation off on
	// purpose; the migration must not switch it back on.
	solo, _ := m.AgentByID("solo")
	if len(solo.Handoffs) != 0 {
		t.Fatalf("non-delegating agent gained handoffs: %v", solo.Handoffs)
	}
	w, _ := m.AgentByID("worker")
	if slices.Contains(w.Handoffs, "general-purpose") {
		t.Fatalf("worker must not gain a handoff (the agent tool is primary-only): %v", w.Handoffs)
	}
}

// Repeated loads must never duplicate the agent entry or the handoff.
func TestV11MigrationIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, AgentManifestFilename)
	if err := os.WriteFile(path, []byte(preV11Manifest), 0o600); err != nil {
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
	if len(first.Agents) != len(second.Agents) {
		t.Fatalf("agent list grew on re-migration: %d -> %d", len(first.Agents), len(second.Agents))
	}
	planner, _ := second.AgentByID("planner")
	got := 0
	for _, id := range planner.Handoffs {
		if id == "general-purpose" {
			got++
		}
	}
	if got != 1 {
		t.Fatalf("planner accumulated %d general-purpose handoffs", got)
	}
}

func TestDefaultManifestShipsGeneralPurposeAgent(t *testing.T) {
	m := DefaultAgentManifest()
	gp, ok := m.AgentByID("general-purpose")
	if !ok {
		t.Fatal("default manifest must ship the general-purpose agent")
	}
	// It is a fallback for open-ended work, so it needs the read, write, and
	// execute surface a specialist split across three agents would have.
	for _, id := range []string{"grep", "file-read", "file-edit", "bash", "web-fetch"} {
		if !slices.Contains(gp.AllowedTools, id) {
			t.Fatalf("general-purpose is missing %q", id)
		}
	}
	// primary_only keeps the agent tool out of reach anyway; leaving it off
	// the allow-list makes that explicit rather than silently dropped.
	if slices.Contains(gp.AllowedTools, "agent") {
		t.Fatal("general-purpose must not fan out further")
	}
	if gp.Mode != "worker" {
		t.Fatalf("mode = %q, want worker (keeps it out of the user-facing mode selector)", gp.Mode)
	}
	for _, id := range []string{"plan", "coding", "ask"} {
		a, _ := m.AgentByID(id)
		if !slices.Contains(a.Handoffs, "general-purpose") {
			t.Fatalf("agent %q must be able to delegate to general-purpose", id)
		}
	}
}
