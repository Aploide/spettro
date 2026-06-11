package tui

import "testing"

// TestAgentDoneSeparatesCostFromOccupancy verifies EFF-3 at the TUI level:
// across runs, totalTokensUsed accumulates (cost) while contextTokens is
// REPLACED by the latest run's occupancy estimate (not summed).
func TestAgentDoneSeparatesCostFromOccupancy(t *testing.T) {
	m := NewModelForTesting()

	// First run.
	m.thinking = true
	out, _ := m.update(agentDoneMsg{content: "one", tokensUsed: 750, contextTokens: 400})
	m = out.(Model)
	if m.totalTokensUsed != 750 {
		t.Fatalf("after run 1: totalTokensUsed = %d, want 750", m.totalTokensUsed)
	}
	if m.contextTokens != 400 {
		t.Fatalf("after run 1: contextTokens = %d, want 400", m.contextTokens)
	}

	// Second run: cost accumulates, occupancy is replaced (not summed).
	m.thinking = true
	out, _ = m.update(agentDoneMsg{content: "two", tokensUsed: 600, contextTokens: 500})
	m = out.(Model)
	if m.totalTokensUsed != 1350 {
		t.Fatalf("after run 2: totalTokensUsed = %d, want 1350 (cost accumulates)", m.totalTokensUsed)
	}
	if m.contextTokens != 500 {
		t.Fatalf("after run 2: contextTokens = %d, want 500 (occupancy replaced, not summed)", m.contextTokens)
	}
}

// TestEvaluateCompactUsesOccupancy verifies the compaction evaluation reads
// contextTokens (occupancy), not totalTokensUsed (cost). With a huge cost but a
// tiny occupancy the gauge must stay calm.
func TestEvaluateCompactUsesOccupancy(t *testing.T) {
	m := NewModelForTesting()
	m.cfg.ActiveProvider = "anthropic" // 200k default window
	m.totalTokensUsed = 5_000_000      // enormous cumulative cost
	m.contextTokens = 1000             // tiny actual occupancy

	eval := m.evaluateCompact()
	if eval.IsBlocking || eval.IsError || eval.IsWarning || eval.ShouldAutoCompact {
		t.Fatalf("low occupancy must not trip the gauge despite high cost: %+v", eval)
	}

	// Now raise occupancy past the window: the gauge must react.
	m.contextTokens = 5_000_000
	eval = m.evaluateCompact()
	if !eval.IsBlocking {
		t.Fatalf("occupancy above the window should block: %+v", eval)
	}
}

// TestCompactResetsOccupancy verifies a successful compaction zeroes the
// occupancy gauge (and the cumulative cost, preserving prior behavior).
func TestCompactResetsOccupancy(t *testing.T) {
	m := NewModelForTesting()
	m.thinking = true
	m.totalTokensUsed = 1000
	m.contextTokens = 900
	m.messages = []ChatMessage{{Role: RoleUser, Content: "x"}}

	out, _ := m.update(compactDoneMsg{summary: "summary"})
	m = out.(Model)
	if m.contextTokens != 0 {
		t.Fatalf("compaction should reset contextTokens to 0, got %d", m.contextTokens)
	}
	if m.totalTokensUsed != 0 {
		t.Fatalf("compaction should reset totalTokensUsed to 0, got %d", m.totalTokensUsed)
	}
}
