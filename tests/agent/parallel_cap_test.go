package agent_test

import (
	"context"
	"strings"
	"testing"

	"spettro/internal/agent"
	"spettro/internal/config"
)

// TestLLMAgent_ParallelToolCallsAreCapped verifies that a single LLM response
// with more tool calls than `runtime.delegation.max_tool_calls_per_step` is
// trimmed: the first N execute, and the remainder receive a synthetic deny
// trace pushed back into the conversation so the model can adapt.
//
// Regression: previously parallelExec only bounded `agent` calls — `bash`,
// `comment`, `file-write` etc. could each spawn one goroutine per call,
// allowing the LLM to fan out arbitrarily many concurrent tools and balloon
// history/cost in a single step.
func TestLLMAgent_ParallelToolCallsAreCapped(t *testing.T) {
	const cap_ = 5
	const batch = 8

	var firstResponse strings.Builder
	for i := range batch {
		firstResponse.WriteString(`TOOL_CALL {"name":"comment","arguments":{"message":"step ` + itoa(i) + `"}}` + "\n")
	}

	pm, providerName, modelName := scriptedManager(t, []string{
		strings.TrimRight(firstResponse.String(), "\n"),
		"FINAL\ndone",
	})

	manifest := config.AgentManifest{
		Version:      2,
		DefaultAgent: "worker",
		Runtime: config.RuntimePolicy{
			DefaultPermission: config.PermissionYOLO,
			DefaultTimeoutSec: 60,
			SandboxMode:       config.SandboxWorkspaceWrite,
			Delegation: config.DelegationPolicy{
				MaxParallelWorkers:  4,
				MaxDepth:            2,
				MaxToolCallsPerStep: cap_,
			},
		},
		Tools: []config.ToolSpec{
			{ID: "comment", Name: "Comment", Kind: "builtin", Enabled: true, TimeoutSec: 10, PermittedActions: []string{"read"}, RiskLevel: "low"},
		},
		Agents: []config.AgentSpec{
			{ID: "worker", Name: "Worker", Mode: "worker", Role: config.AgentRoleWorker, AllowedTools: []string{"comment"}, Permission: config.PermissionYOLO, MaxSteps: 4, Enabled: true},
		},
	}
	if err := manifest.Validate(); err != nil {
		t.Fatalf("manifest validate: %v", err)
	}

	ag := agent.LLMAgent{
		Spec:            manifest.Agents[0],
		ProviderManager: pm,
		ProviderName:    func() string { return providerName },
		ModelName:       func() string { return modelName },
		CWD:             t.TempDir(),
		Manifest:        &manifest,
	}

	result, err := ag.Run(context.Background(), "spam tools")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(result.Tools) != batch {
		t.Fatalf("expected %d tool traces, got %d: %+v", batch, len(result.Tools), result.Tools)
	}
	var successes, denies int
	for _, tr := range result.Tools {
		switch tr.Status {
		case "success":
			successes++
		case "error":
			if !strings.Contains(tr.Output, "too many tool calls in one step") {
				t.Fatalf("error trace did not mention the cap: %q", tr.Output)
			}
			denies++
		default:
			t.Fatalf("unexpected status %q in trace %+v", tr.Status, tr)
		}
	}
	if successes != cap_ {
		t.Fatalf("expected %d successful traces, got %d", cap_, successes)
	}
	if denies != batch-cap_ {
		t.Fatalf("expected %d deny traces, got %d", batch-cap_, denies)
	}
}

// itoa avoids dragging in strconv just for tests.
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [20]byte
	pos := len(b)
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(b[pos:])
}

// TestLLMAgent_ParallelToolCallsCapDefault verifies that when the manifest
// does not set max_tool_calls_per_step, the runtime falls back to the
// hard-coded default of 32 rather than running an unbounded batch.
func TestLLMAgent_ParallelToolCallsCapDefault(t *testing.T) {
	const batch = 40 // > default cap of 32

	var firstResponse strings.Builder
	for i := range batch {
		firstResponse.WriteString(`TOOL_CALL {"name":"comment","arguments":{"message":"call ` + itoa(i) + `"}}` + "\n")
	}

	pm, providerName, modelName := scriptedManager(t, []string{
		strings.TrimRight(firstResponse.String(), "\n"),
		"FINAL\ndone",
	})

	// Manifest with MaxToolCallsPerStep == 0 — the LLMAgent.Run path should
	// fall back to the hard-coded default of 32. We do not call Validate()
	// here on purpose: Validate now rejects 0 (so user-facing manifests must
	// set it), but in-code construction still relies on the defaulting in
	// agent.go for backward compatibility.
	manifest := config.AgentManifest{
		Version:      2,
		DefaultAgent: "worker",
		Runtime: config.RuntimePolicy{
			DefaultPermission: config.PermissionYOLO,
			DefaultTimeoutSec: 60,
			SandboxMode:       config.SandboxWorkspaceWrite,
			Delegation:        config.DelegationPolicy{MaxParallelWorkers: 4, MaxDepth: 2 /* MaxToolCallsPerStep unset */},
		},
		Tools: []config.ToolSpec{
			{ID: "comment", Name: "Comment", Kind: "builtin", Enabled: true, TimeoutSec: 10, PermittedActions: []string{"read"}, RiskLevel: "low"},
		},
		Agents: []config.AgentSpec{
			{ID: "worker", Name: "Worker", Mode: "worker", Role: config.AgentRoleWorker, AllowedTools: []string{"comment"}, Permission: config.PermissionYOLO, MaxSteps: 4, Enabled: true},
		},
	}

	ag := agent.LLMAgent{
		Spec:            manifest.Agents[0],
		ProviderManager: pm,
		ProviderName:    func() string { return providerName },
		ModelName:       func() string { return modelName },
		CWD:             t.TempDir(),
		Manifest:        &manifest,
	}
	result, err := ag.Run(context.Background(), "spam tools")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(result.Tools) != batch {
		t.Fatalf("expected %d tool traces, got %d", batch, len(result.Tools))
	}
	var successes, denies int
	for _, tr := range result.Tools {
		switch tr.Status {
		case "success":
			successes++
		case "error":
			denies++
		}
	}
	if successes != 32 {
		t.Fatalf("expected default cap of 32 successes, got %d", successes)
	}
	if denies != batch-32 {
		t.Fatalf("expected %d deny traces, got %d", batch-32, denies)
	}
}
