package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"

	"spettro/internal/agent"
)

// TestLiveUsageUpdatesWithoutDoubleCounting verifies the per-request usage
// path: usageEventMsg updates the counters mid-run, and the agentDoneMsg that
// follows only adds the remainder of the run's cost, never re-adding what the
// live events already applied.
func TestLiveUsageUpdatesWithoutDoubleCounting(t *testing.T) {
	m := NewModelForTesting()

	m.thinking = true
	out, _ := m.update(usageEventMsg{event: agent.UsageEvent{StepTokens: 1000, TotalTokens: 1000, ContextTokens: 1000}})
	m = out.(Model)
	if m.totalTokensUsed != 1000 || m.contextTokens != 1000 {
		t.Fatalf("after step 1: total = %d, context = %d, want 1000/1000", m.totalTokensUsed, m.contextTokens)
	}
	out, _ = m.update(usageEventMsg{event: agent.UsageEvent{StepTokens: 1500, TotalTokens: 2500, ContextTokens: 1500}})
	m = out.(Model)
	if m.totalTokensUsed != 2500 || m.contextTokens != 1500 {
		t.Fatalf("after step 2: total = %d, context = %d, want 2500/1500", m.totalTokensUsed, m.contextTokens)
	}

	// The run completes reporting the same cumulative cost: nothing to add.
	out, _ = m.update(agentDoneMsg{content: "done", tokensUsed: 2500, contextTokens: 1500})
	m = out.(Model)
	if m.totalTokensUsed != 2500 {
		t.Fatalf("after done: totalTokensUsed = %d, want 2500 (no double count)", m.totalTokensUsed)
	}

	// A second run whose done message reports MORE than the live events
	// delivered (e.g. a dropped event): only the remainder is added.
	m.thinking = true
	out, _ = m.update(usageEventMsg{event: agent.UsageEvent{StepTokens: 300, TotalTokens: 300, ContextTokens: 1600}})
	m = out.(Model)
	out, _ = m.update(agentDoneMsg{content: "done", tokensUsed: 500, contextTokens: 1600})
	m = out.(Model)
	if m.totalTokensUsed != 3000 {
		t.Fatalf("after run 2: totalTokensUsed = %d, want 3000 (2500 + 500)", m.totalTokensUsed)
	}
}

// TestWorkingIndicatorShowsElapsedAndTokens verifies the live readout that
// replaced the status-bar ticker: visible above the input while a run
// streams, absent when idle, and gone from the status bar entirely.
func TestWorkingIndicatorShowsElapsedAndTokens(t *testing.T) {
	m := NewModelForTesting()

	if ind := m.viewWorkingIndicator(80); ind != "" {
		t.Fatalf("idle indicator should be empty, got %q", ind)
	}

	m.thinking = true
	m.agentStartAt = time.Now().Add(-158 * time.Second)
	m.workingVerb = "Unravelling"
	out, _ := m.update(usageEventMsg{event: agent.UsageEvent{
		StepTokens: 1200, TotalTokens: 1200, ContextTokens: 1200,
	}})
	m = out.(Model)

	ind := ansi.Strip(m.viewWorkingIndicator(80))
	if !strings.Contains(ind, "Unravelling…") {
		t.Fatalf("indicator missing the run's verb: %q", ind)
	}
	if !strings.Contains(ind, "(2m 38s · ↓ 1.2k tokens)") {
		t.Fatalf("indicator missing elapsed/tokens: %q", ind)
	}
	if msg := m.statusBarMessage(); msg != "" {
		t.Fatalf("the ticker must be gone from the status bar, got %q", msg)
	}

	m.thinking = false
	if ind := m.viewWorkingIndicator(80); ind != "" {
		t.Fatalf("indicator should clear when idle, got %q", ind)
	}
}
