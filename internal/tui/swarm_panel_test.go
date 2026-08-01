package tui

import (
	"fmt"
	"strings"
	"testing"

	"spettro/internal/agent"
	"spettro/internal/config"
)

func swarmTrace(name, item, status string) agent.ToolTrace {
	return agent.ToolTrace{
		AgentID: name,
		Name:    "agent",
		Status:  status,
		Args:    fmt.Sprintf(`{"agent":%q,"task":%q,"parent_agent_id":"coding","swarm":true}`, name, item),
	}
}

func TestApplyToolTrace_SwarmLifecycle(t *testing.T) {
	m := NewModelForTesting()
	manifest := config.DefaultAgentManifest()
	m.manifest = manifest

	m.applyToolTraceToObservability(swarmTrace("code#1", "a.go", "running"))
	m.applyToolTraceToObservability(swarmTrace("code#2", "b.go", "running"))
	if len(m.parallelAgents) != 2 {
		t.Fatalf("want 2 swarm entries, got %d", len(m.parallelAgents))
	}
	for _, a := range m.parallelAgents {
		if a.Kind != "swarm" || a.Status != "running" {
			t.Fatalf("bad entry: %+v", a)
		}
	}

	// Completion keeps swarm members listed with their outcome instead of
	// dropping them (unlike plain worker delegations).
	m.applyToolTraceToObservability(swarmTrace("code#1", "a.go", "success"))
	m.applyToolTraceToObservability(swarmTrace("code#2", "b.go", "error"))
	if len(m.parallelAgents) != 2 {
		t.Fatalf("swarm entries must persist after completion, got %d", len(m.parallelAgents))
	}
	if m.parallelAgents[0].Status != "done" || m.parallelAgents[1].Status != "failed" {
		t.Fatalf("bad statuses: %+v", m.parallelAgents)
	}
}

func TestSidePanelSwarmLines_RendersMembers(t *testing.T) {
	m := NewModelForTesting()
	manifest := config.DefaultAgentManifest()
	m.manifest = manifest

	if lines := m.sidePanelSwarmLines(48); lines != nil {
		t.Fatalf("no swarm → no lines, got %v", lines)
	}

	m.applyToolTraceToObservability(swarmTrace("code#1", "fix a.go", "running"))
	m.applyToolTraceToObservability(swarmTrace("code#2", "fix b.go", "running"))
	m.applyToolTraceToObservability(swarmTrace("code#2", "fix b.go", "error"))
	// Live tool activity from a member should surface as "what it's doing".
	m.applyToolTraceToObservability(agent.ToolTrace{AgentID: "code#1", Name: "file-read", Status: "running", Args: `{"path":"a.go"}`})

	lines := m.sidePanelSwarmLines(48)
	if len(lines) != 3 {
		t.Fatalf("want header + 2 members, got %d: %v", len(lines), lines)
	}
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "1 running · 0 done · 1 failed") {
		t.Fatalf("bad header: %s", lines[0])
	}
	if !strings.Contains(joined, "code#1") || !strings.Contains(joined, "code#2") {
		t.Fatalf("missing member names: %s", joined)
	}
	if !strings.Contains(joined, "a.go") {
		t.Fatalf("missing live activity: %s", joined)
	}
}

func TestSwarmSpecID(t *testing.T) {
	for in, want := range map[string]string{"code#3": "code", "code": "code", "#3": "#3"} {
		if got := swarmSpecID(in); got != want {
			t.Fatalf("swarmSpecID(%q) = %q, want %q", in, got, want)
		}
	}
}
