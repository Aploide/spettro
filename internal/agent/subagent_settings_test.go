package agent

import (
	"context"
	"testing"

	"spettro/internal/config"
	"spettro/internal/provider"
)

func delegationManifest() config.AgentManifest {
	return config.AgentManifest{
		Version:      2,
		DefaultAgent: "orch",
		Runtime: config.RuntimePolicy{
			DefaultPermission: config.PermissionYOLO,
			DefaultTimeoutSec: 60,
			Delegation:        config.DelegationPolicy{MaxParallelWorkers: 2, MaxDepth: 3},
		},
		Tools: []config.ToolSpec{
			{ID: "agent", Name: "Agent", Kind: "builtin", Enabled: true, TimeoutSec: 60, PermittedActions: []string{"read", "plan"}, PrimaryOnly: true},
			{ID: "comment", Name: "Comment", Kind: "builtin", Enabled: true, TimeoutSec: 5, PermittedActions: []string{"read"}},
		},
		Agents: []config.AgentSpec{
			{ID: "orch", Name: "Orch", Mode: "orchestrator", Role: config.AgentRolePrimary, AllowedTools: []string{"agent", "comment"},
				PermittedActions: []string{"read", "plan"}, Permission: config.PermissionYOLO, MaxSteps: 4, Enabled: true, Handoffs: []string{"code"}},
			{ID: "code", Name: "Code", Mode: "worker", Role: config.AgentRoleWorker, AllowedTools: []string{"comment"},
				PermittedActions: []string{"read"}, Permission: config.PermissionYOLO, MaxSteps: 4, Enabled: true},
		},
	}
}

// Sub-agents inherit the user's output cap and the thinking level the
// parent's model actually accepted, instead of the automatic cap and the
// level the model already rejected.
func TestSubAgentsInheritOutputCapAndAcceptedThinking(t *testing.T) {
	fastRetries(t)
	pm, url, ls := newLoopServer(t,
		loopReply{status: 400, errMsg: "unsupported value for reasoning_effort: high"},
		loopReply{toolName: "agent", toolArgs: `{"target":"code","task":"say hi"}`},
		loopReply{content: "hi from code"},
		loopReply{content: "done"},
	)
	manifest := delegationManifest()
	if err := manifest.Validate(); err != nil {
		t.Fatal(err)
	}
	ag := LLMAgent{
		Spec:            manifest.Agents[0],
		ProviderManager: pm,
		ProviderName:    func() string { return url },
		ModelName:       func() string { return "m" },
		CWD:             t.TempDir(),
		Manifest:        &manifest,
		MaxOutputTokens: 8192,
		Thinking:        provider.ThinkingHigh,
	}
	if _, err := ag.Run(context.Background(), "delegate"); err != nil {
		t.Fatal(err)
	}
	reqs := ls.requests()
	if len(reqs) != 4 {
		t.Fatalf("requests = %d", len(reqs))
	}
	sub := reqs[2]
	if got := sub["max_tokens"]; got != float64(8192) {
		t.Fatalf("sub-agent max_tokens = %v, want the configured 8192", got)
	}
	if got, want := sub["reasoning_effort"], reqs[1]["reasoning_effort"]; got != want {
		t.Fatalf("sub-agent reasoning_effort = %v, want the parent's accepted %v", got, want)
	}
}
