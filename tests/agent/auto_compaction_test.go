package agent_test

import (
	"context"
	"strings"
	"testing"

	"spettro/internal/agent"
	"spettro/internal/compact"
	"spettro/internal/config"
	"spettro/internal/provider"
)

// longHistory builds a carried conversation big enough to cross the
// auto-compaction threshold of a 1000-token context window (effective window
// ~500 tokens after the reserved-output cut; threshold ~85% of that).
func longHistory(turns, chars int) []provider.Message {
	msgs := make([]provider.Message, 0, turns)
	for i := range turns {
		role := provider.RoleUser
		if i%2 == 1 {
			role = provider.RoleAssistant
		}
		msgs = append(msgs, provider.Message{Role: role, Content: strings.Repeat("y", chars)})
	}
	return msgs
}

// TestAutoCompaction_GoalModeThresholdCrossing verifies the in-loop trigger
// end to end: a goal-mode run whose carried history already crowds the
// context window compacts automatically before the first model step, keeps
// the recent tail verbatim, and finishes normally.
func TestAutoCompaction_GoalModeThresholdCrossing(t *testing.T) {
	// Request #1 is the summarizer call issued by the in-loop compaction;
	// request #2 is the main model step. scriptedManager fails the test on
	// any extra (or missing) request, pinning the trigger count.
	responses := []string{
		"summary-of-earlier-progress",
		"FINAL\nGoal finished after compaction.",
	}
	pm, providerName, modelName := scriptedManager(t, responses)

	spec := config.AgentSpec{
		ID:           "test-runner",
		Mode:         "worker",
		AllowedTools: []string{"comment"},
		Permission:   config.PermissionYOLO,
		Enabled:      true,
	}
	var notices []string
	a := agent.LLMAgent{
		Spec:            spec,
		ProviderManager: pm,
		ProviderName:    func() string { return providerName },
		ModelName:       func() string { return modelName },
		CWD:             t.TempDir(),
		GoalMode:        true,
		ContextWindow:   1000,
		Messages:        longHistory(12, 400),
		Compact:         compact.Config{AutoEnabled: true, AutoThresholdPct: 85, MaxFailures: 3},
		ToolCallback: func(tr agent.ToolTrace) {
			if tr.Name == "comment" {
				notices = append(notices, tr.Output)
			}
		},
	}

	result, err := a.Run(context.Background(), "keep working toward the goal")
	if err != nil {
		t.Fatalf("agent.Run failed: %v", err)
	}
	if !strings.Contains(result.Content, "Goal finished after compaction") {
		t.Errorf("unexpected final content: %q", result.Content)
	}
	var summarized bool
	for _, m := range result.Messages {
		if strings.Contains(m.Content, "[earlier progress summarized]") {
			summarized = true
			if !strings.Contains(m.Content, "summary-of-earlier-progress") {
				t.Errorf("summary message missing summarizer output: %q", m.Content)
			}
		}
	}
	if !summarized {
		t.Fatal("no summary message inserted — auto compaction did not fire")
	}
	if len(result.Messages) >= 13 {
		t.Errorf("history did not shrink: %d messages", len(result.Messages))
	}
	var noticed bool
	for _, n := range notices {
		if strings.Contains(n, "compacted") && strings.Contains(n, "tokens") {
			noticed = true
		}
	}
	if !noticed {
		t.Errorf("no compaction notice surfaced to the host; notices: %q", notices)
	}
}

// TestAutoCompaction_DisabledFlagRespected verifies the off switch: with the
// same oversized history, an explicitly disabled policy must not issue a
// summarizer call (scriptedManager fails the test on any extra request).
func TestAutoCompaction_DisabledFlagRespected(t *testing.T) {
	responses := []string{"FINAL\nDone without compaction."}
	pm, providerName, modelName := scriptedManager(t, responses)

	spec := config.AgentSpec{
		ID:           "test-runner",
		Mode:         "worker",
		AllowedTools: []string{"comment"},
		Permission:   config.PermissionYOLO,
		Enabled:      true,
	}
	a := agent.LLMAgent{
		Spec:            spec,
		ProviderManager: pm,
		ProviderName:    func() string { return providerName },
		ModelName:       func() string { return modelName },
		CWD:             t.TempDir(),
		GoalMode:        true,
		ContextWindow:   1000,
		Messages:        longHistory(12, 400),
		Compact:         compact.Config{AutoEnabled: false, AutoThresholdPct: 85, MaxFailures: 3},
	}

	result, err := a.Run(context.Background(), "keep working toward the goal")
	if err != nil {
		t.Fatalf("agent.Run failed: %v", err)
	}
	for _, m := range result.Messages {
		if strings.Contains(m.Content, "[earlier progress summarized]") {
			t.Fatal("history was compacted despite the off switch")
		}
	}
}
