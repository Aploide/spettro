package agent_test

import (
	"context"
	"strings"
	"testing"

	"spettro/internal/agent"
	"spettro/internal/config"
)

// TestLoopDetection_NudgeThenAbort verifies the run loop stops an agent that
// keeps issuing the same tool call: after the first detection it injects a
// nudge and keeps going; when the repetition continues, the run ends with a
// clear user-facing message instead of burning tokens forever.
func TestLoopDetection_NudgeThenAbort(t *testing.T) {
	// Six identical responses: the default threshold (3 identical consecutive
	// calls) trips the nudge on step 3, counters reset, and the second trip on
	// step 6 aborts the run. No further requests should be made.
	responses := make([]string, 6)
	for i := range responses {
		responses[i] = `TOOL_CALL {"tool":"comment","args":{"message":"same"}}`
	}

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
		MaxTokens:       0,
	}

	result, err := a.Run(context.Background(), "Do something.")
	if err != nil {
		t.Fatalf("agent.Run failed: %v", err)
	}
	if !strings.Contains(result.Content, "repeating the same actions") {
		t.Errorf("expected loop-stop message, got: %q", result.Content)
	}
	// The abort fires before the sixth call executes: 5 tool traces, not 6.
	if len(result.Tools) > 5 {
		t.Errorf("expected the repeated call to be skipped on abort, got %d traces", len(result.Tools))
	}
}

// TestLoopDetection_Disabled verifies the manifest off switch: with
// loop_detection.disabled the same repeated sequence runs to completion.
func TestLoopDetection_Disabled(t *testing.T) {
	responses := make([]string, 0, 11)
	for range 10 {
		responses = append(responses, `TOOL_CALL {"tool":"comment","args":{"message":"same"}}`)
	}
	responses = append(responses, "FINAL\nDone repeating.")

	pm, providerName, modelName := scriptedManager(t, responses)

	spec := config.AgentSpec{
		ID:           "test-runner",
		Mode:         "worker",
		AllowedTools: []string{"comment"},
		Permission:   config.PermissionYOLO,
		Enabled:      true,
	}
	manifest := &config.AgentManifest{
		Runtime: config.RuntimePolicy{
			LoopDetection: config.LoopDetectionPolicy{Disabled: true},
		},
		Agents: []config.AgentSpec{spec},
	}

	a := agent.LLMAgent{
		Spec:            spec,
		ProviderManager: pm,
		ProviderName:    func() string { return providerName },
		ModelName:       func() string { return modelName },
		CWD:             t.TempDir(),
		Manifest:        manifest,
		MaxTokens:       0,
	}

	result, err := a.Run(context.Background(), "Do something.")
	if err != nil {
		t.Fatalf("agent.Run failed: %v", err)
	}
	if !strings.Contains(result.Content, "Done repeating") {
		t.Errorf("expected final answer with detection disabled, got: %q", result.Content)
	}
	if len(result.Tools) != 10 {
		t.Errorf("expected 10 tool traces, got %d", len(result.Tools))
	}
}
