package tui_test

import (
	"strings"
	"testing"

	"spettro/internal/config"
	"spettro/internal/tui"
)

func TestPrimaryAgentIDsForTesting_UsesRoles(t *testing.T) {
	manifest := config.AgentManifest{
		Agents: []config.AgentSpec{
			{ID: "plan", Enabled: true, Role: config.AgentRoleOrchestrator},
			{ID: "worker", Enabled: true, Role: config.AgentRoleWorker},
			{ID: "ask", Enabled: true, Role: config.AgentRolePrimary},
			{ID: "disabled", Enabled: false, Role: config.AgentRolePrimary},
		},
	}
	ids := tui.PrimaryAgentIDsForTesting(manifest)
	joined := strings.Join(ids, ",")
	if strings.Contains(joined, "worker") {
		t.Fatalf("worker role should not be part of primary tab cycle: %v", ids)
	}
	if !strings.Contains(joined, "plan") || !strings.Contains(joined, "ask") {
		t.Fatalf("expected primary/orchestrator agents in cycle, got: %v", ids)
	}
}

func TestSanitizeToolOutputForTesting_FormatsSubagentEnvelope(t *testing.T) {
	out := tui.SanitizeToolOutputForTesting(`{"agent":"review","status":"ok","summary":"looks good","tool_trace_count":2,"tokens_used":44}`, 20)
	for _, want := range []string{"sub-agent: review", "status: ok", "tools: 2  tokens: 44", "looks good"} {
		if !strings.Contains(out, want) {
			t.Fatalf("expected %q in formatted output, got: %s", want, out)
		}
	}
}

