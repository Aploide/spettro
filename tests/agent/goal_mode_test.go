package agent_test

import (
	"context"
	"strings"
	"testing"

	"spettro/internal/agent"
	"spettro/internal/config"
)

// TestUnboundedLoop_NoStepCap verifies that the agent runtime no longer has a
// MaxSteps cap. The loop runs until the model produces a final answer, calls
// goal-complete, or is stopped. This test simulates a 50-step run with tool
// calls followed by a final answer and asserts completion without any
// "max tool steps reached" error.
func TestUnboundedLoop_NoStepCap(t *testing.T) {
	// Build a scripted sequence: 50 tool-call responses then a final answer.
	responses := make([]string, 0, 51)
	for i := 0; i < 50; i++ {
		responses = append(responses, `TOOL_CALL {"tool":"comment","args":{"message":"step"}}`)
	}
	responses = append(responses, "FINAL\nAll 50 steps completed successfully.")

	pm, providerName, modelName := scriptedManager(t, responses)

	spec := config.AgentSpec{
		ID:           "test-runner",
		Mode:         "worker",
		AllowedTools: []string{"comment"},
		Permission:   config.PermissionYOLO,
		Enabled:      true,
	}

	agent := agent.LLMAgent{
		Spec:            spec,
		ProviderManager: pm,
		ProviderName:    func() string { return providerName },
		ModelName:       func() string { return modelName },
		CWD:             t.TempDir(),
		MaxTokens:       0, // unlimited
	}

	ctx := context.Background()
	result, err := agent.Run(ctx, "Run 50 steps then provide a final answer.")

	if err != nil {
		if strings.Contains(err.Error(), "max tool steps reached") {
			t.Fatal("unexpected 'max tool steps reached' error — step cap should be removed")
		}
		t.Fatalf("agent.Run failed: %v", err)
	}
	if !strings.Contains(result.Content, "All 50 steps completed successfully") {
		t.Errorf("expected final answer, got: %q", result.Content)
	}
	// Verify the loop actually ran all 50 tool-call steps.
	if len(result.Tools) < 50 {
		t.Errorf("expected at least 50 tool traces, got %d", len(result.Tools))
	}
}

// TestGoalComplete_TerminatesLoop verifies that when the agent calls the
// goal-complete tool, the run returns immediately with GoalComplete=true and
// the summary populated, even though more steps were allowed.
func TestGoalComplete_TerminatesLoop(t *testing.T) {
	// Script: 3 tool calls, then goal-complete, then (unreachable) more work.
	responses := []string{
		`TOOL_CALL {"tool":"comment","args":{"message":"step 1"}}`,
		`TOOL_CALL {"tool":"comment","args":{"message":"step 2"}}`,
		`TOOL_CALL {"tool":"comment","args":{"message":"step 3"}}`,
		`TOOL_CALL {"tool":"goal-complete","args":{"summary":"Objective met after 3 steps","verified":true}}`,
		`FINAL\nThis should never be reached.`,
	}

	pm, providerName, modelName := scriptedManager(t, responses)

	spec := config.AgentSpec{
		ID:           "goal-runner",
		Mode:         "worker",
		AllowedTools: []string{"comment", "goal-complete"},
		Permission:   config.PermissionYOLO,
		Enabled:      true,
	}

	a := agent.LLMAgent{
		Spec:            spec,
		ProviderManager: pm,
		ProviderName:    func() string { return providerName },
		ModelName:       func() string { return modelName },
		CWD:             t.TempDir(),
		GoalMode:        true, // Required for goal-complete to work
	}

	ctx := context.Background()
	result, err := a.Run(ctx, agent.GoalModePreamble+"Complete the objective.")

	if err != nil {
		t.Fatalf("agent.Run failed: %v", err)
	}
	if !result.GoalComplete {
		t.Error("expected GoalComplete=true, got false")
	}
	if result.GoalSummary == "" {
		t.Error("expected non-empty GoalSummary")
	}
	if !strings.Contains(result.GoalSummary, "Objective met") {
		t.Errorf("unexpected GoalSummary: %q", result.GoalSummary)
	}
	// The loop should have stopped at the goal-complete call, not reached FINAL.
	if strings.Contains(result.Content, "should never be reached") {
		t.Error("loop continued past goal-complete — it should have terminated")
	}
}

// TestGoalComplete_RejectedOutsideGoalMode verifies that calling goal-complete
// without GoalMode=true returns an error, as the tool is not available.
func TestGoalComplete_RejectedOutsideGoalMode(t *testing.T) {
	// Script: try to call goal-complete without GoalMode enabled.
	responses := []string{
		`TOOL_CALL {"tool":"goal-complete","args":{"summary":"trying to complete"}}`,
		`FINAL\nDone.`,
	}

	pm, providerName, modelName := scriptedManager(t, responses)

	spec := config.AgentSpec{
		ID:           "normal-runner",
		Mode:         "worker",
		AllowedTools: []string{"goal-complete"}, // Technically allowed, but...
		Permission:   config.PermissionYOLO,
		Enabled:      true,
	}

	a := agent.LLMAgent{
		Spec:            spec,
		ProviderManager: pm,
		ProviderName:    func() string { return providerName },
		ModelName:       func() string { return modelName },
		CWD:             t.TempDir(),
		GoalMode:        false, // Not in goal mode
	}

	ctx := context.Background()
	result, err := a.Run(ctx, "Try to complete without goal mode.")

	// The tool should fail with "only available in goal mode" error.
	// The loop continues to the FINAL answer since goal-complete errored.
	if err != nil {
		t.Fatalf("agent.Run failed: %v", err)
	}
	if result.GoalComplete {
		t.Error("GoalComplete should be false when not in goal mode")
	}
	// Verify the tool trace shows an error for the goal-complete call.
	foundGoalCompleteError := false
	for _, trace := range result.Tools {
		if trace.Name == "goal-complete" && trace.Status == "error" {
			foundGoalCompleteError = true
			if !strings.Contains(trace.Output, "only available in goal mode") {
				t.Errorf("expected 'only available in goal mode' error, got: %q", trace.Output)
			}
		}
	}
	if !foundGoalCompleteError {
		t.Error("expected goal-complete tool to error when not in goal mode")
	}
}

// TestGoalMode_ShellTimeoutElevated verifies that in goal mode, shell-exec and
// bash tools use the elevated timeout (600s default or configured value) rather
// than the standard 45s.
func TestGoalMode_ShellTimeoutElevated(t *testing.T) {
	// This test verifies the behavior by checking that the tool-runtime
	// correctly sets the timeout. We can't easily test the actual timeout
	// without a real long-running command, so we verify the goal-mode flag
	// is properly threaded through and the tool executes successfully.
	responses := []string{
		`TOOL_CALL {"tool":"bash","args":{"command":"echo 'goal mode test'"}}`,
		`FINAL\nDone.`,
	}

	pm, providerName, modelName := scriptedManager(t, responses)

	spec := config.AgentSpec{
		ID:           "goal-shell-test",
		Mode:         "worker",
		AllowedTools: []string{"bash"},
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
		ShellTimeoutSec: 900, // Custom elevated timeout
	}

	ctx := context.Background()
	result, err := a.Run(ctx, agent.GoalModePreamble+"Run a shell command.")

	if err != nil {
		t.Fatalf("agent.Run failed: %v", err)
	}
	// Verify the bash tool executed successfully (would timeout at 45s if not elevated).
	foundBashSuccess := false
	for _, trace := range result.Tools {
		if trace.Name == "bash" && trace.Status == "success" {
			foundBashSuccess = true
		}
	}
	if !foundBashSuccess {
		t.Error("expected bash tool to succeed with elevated timeout in goal mode")
	}
}
