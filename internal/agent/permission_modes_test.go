package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"spettro/internal/config"
	"spettro/internal/provider"
)

// newPermRuntime creates a minimal toolRuntime for permission testing.
func newPermRuntime(permission config.PermissionLevel, callback ShellApprovalCallback, runtimeRules []config.PermissionRule) *toolRuntime {
	return &toolRuntime{
		permission:    permission,
		allowedShell:  map[string]struct{}{},
		shellApproval: callback,
		runtimeRules:  runtimeRules,
	}
}

// --- YOLO mode tests ---

func TestYOLO_AllowsNonWhitelistedCommandWithoutCallback(t *testing.T) {
	rt := newPermRuntime(config.PermissionYOLO, nil, nil)
	if err := rt.authorizeShellCommand(context.Background(), "shell-exec", "npm run build"); err != nil {
		t.Errorf("YOLO should allow any command without a callback: %v", err)
	}
}

func TestYOLO_BypassesPermissionRuleDeny(t *testing.T) {
	rules := []config.PermissionRule{
		{Permission: "execute", Pattern: "*", Action: config.RuleDeny},
	}
	rt := newPermRuntime(config.PermissionYOLO, nil, rules)
	// Even a deny-all rule must be bypassed in YOLO mode.
	if err := rt.authorizeShellCommand(context.Background(), "shell-exec", "npm run build"); err != nil {
		t.Errorf("YOLO should bypass permission rule deny: %v", err)
	}
}

func TestYOLO_AllowsDangerousGitReset(t *testing.T) {
	rt := newPermRuntime(config.PermissionYOLO, nil, nil)
	if err := rt.authorizeShellCommand(context.Background(), "shell-exec", "git reset --hard HEAD"); err != nil {
		t.Errorf("YOLO should allow dangerous commands like git reset --hard: %v", err)
	}
}

func TestYOLO_NeverInvokesApprovalCallback(t *testing.T) {
	called := false
	callback := func(_ context.Context, _ ShellApprovalRequest) (ShellApprovalDecision, error) {
		called = true
		return ShellApprovalDeny, nil
	}
	rt := newPermRuntime(config.PermissionYOLO, callback, nil)
	if err := rt.authorizeShellCommand(context.Background(), "shell-exec", "npm run build"); err != nil {
		t.Errorf("YOLO should allow command: %v", err)
	}
	if called {
		t.Error("YOLO should never invoke the approval callback")
	}
}

func TestYOLO_AllowsChainedNonWhitelistedCommands(t *testing.T) {
	rt := newPermRuntime(config.PermissionYOLO, nil, nil)
	cmd := "npm install && npm run build && npm test"
	if err := rt.authorizeShellCommand(context.Background(), "shell-exec", cmd); err != nil {
		t.Errorf("YOLO should allow chained commands: %v", err)
	}
}

// --- ask-first mode tests ---

func TestAskFirst_BlocksNonWhitelistedCommandWithoutCallback(t *testing.T) {
	rt := newPermRuntime(config.PermissionAskFirst, nil, nil)
	if err := rt.authorizeShellCommand(context.Background(), "shell-exec", "npm run build"); err == nil {
		t.Error("ask-first should block non-whitelisted commands when no callback is set")
	}
}

func TestAskFirst_AllowsWhitelistedCommands(t *testing.T) {
	rt := newPermRuntime(config.PermissionAskFirst, nil, nil)
	for _, cmd := range []string{"ls", "pwd", "git diff", "git status", "cat README.md", "go test ./..."} {
		if err := rt.authorizeShellCommand(context.Background(), "shell-exec", cmd); err != nil {
			t.Errorf("ask-first should allow whitelisted command %q: %v", cmd, err)
		}
	}
}

func TestAskFirst_BlocksDangerousCommandsEvenWithApprovaingCallback(t *testing.T) {
	callback := func(_ context.Context, _ ShellApprovalRequest) (ShellApprovalDecision, error) {
		return ShellApprovalAllowOnce, nil
	}
	rt := newPermRuntime(config.PermissionAskFirst, callback, nil)
	if err := rt.authorizeShellCommand(context.Background(), "shell-exec", "git reset --hard HEAD"); err == nil {
		t.Error("ask-first should block dangerous commands (git reset --hard) regardless of callback")
	}
	if err := rt.authorizeShellCommand(context.Background(), "shell-exec", "rm -rf /"); err == nil {
		t.Error("ask-first should block dangerous commands (rm -rf /) regardless of callback")
	}
}

func TestAskFirst_InvokesCallbackForNonWhitelisted(t *testing.T) {
	called := false
	callback := func(_ context.Context, req ShellApprovalRequest) (ShellApprovalDecision, error) {
		called = true
		return ShellApprovalAllowOnce, nil
	}
	rt := newPermRuntime(config.PermissionAskFirst, callback, nil)
	if err := rt.authorizeShellCommand(context.Background(), "shell-exec", "npm run build"); err != nil {
		t.Errorf("callback returned allow-once so command should succeed: %v", err)
	}
	if !called {
		t.Error("ask-first should invoke the approval callback for non-whitelisted commands")
	}
}

func TestAskFirst_DeniesWhenCallbackDenies(t *testing.T) {
	callback := func(_ context.Context, _ ShellApprovalRequest) (ShellApprovalDecision, error) {
		return ShellApprovalDeny, nil
	}
	rt := newPermRuntime(config.PermissionAskFirst, callback, nil)
	if err := rt.authorizeShellCommand(context.Background(), "shell-exec", "npm run build"); err == nil {
		t.Error("ask-first should return an error when the callback denies the command")
	}
}

func TestAskFirst_RespectsPermissionRuleDeny(t *testing.T) {
	rules := []config.PermissionRule{
		{Permission: "execute", Pattern: "*", Action: config.RuleDeny},
	}
	callback := func(_ context.Context, _ ShellApprovalRequest) (ShellApprovalDecision, error) {
		return ShellApprovalAllowOnce, nil
	}
	rt := newPermRuntime(config.PermissionAskFirst, callback, rules)
	// Permission rule deny must block even when the callback would approve.
	if err := rt.authorizeShellCommand(context.Background(), "shell-exec", "npm run build"); err == nil {
		t.Error("ask-first should block when a permission rule denies the command")
	}
}

func TestAskFirst_RespectsPermissionRuleAllow_SkipsCallback(t *testing.T) {
	rules := []config.PermissionRule{
		{Permission: "execute", Pattern: "*", Action: config.RuleAllow},
	}
	callbackCalled := false
	callback := func(_ context.Context, _ ShellApprovalRequest) (ShellApprovalDecision, error) {
		callbackCalled = true
		return ShellApprovalDeny, nil
	}
	rt := newPermRuntime(config.PermissionAskFirst, callback, rules)
	if err := rt.authorizeShellCommand(context.Background(), "shell-exec", "npm run build"); err != nil {
		t.Errorf("ask-first with an allow rule should succeed without invoking the callback: %v", err)
	}
	if callbackCalled {
		t.Error("ask-first should not invoke the callback when an allow rule matches")
	}
}

func TestAskFirst_AllowAlways_PersistsToSession(t *testing.T) {
	cwd := t.TempDir()
	calls := 0
	callback := func(_ context.Context, _ ShellApprovalRequest) (ShellApprovalDecision, error) {
		calls++
		return ShellApprovalAllowAlways, nil
	}
	rt := newPermRuntime(config.PermissionAskFirst, callback, nil)
	rt.cwd = cwd

	if err := rt.authorizeShellCommand(context.Background(), "shell-exec", "npm run build"); err != nil {
		t.Fatalf("first call should succeed: %v", err)
	}
	// Second call for the same command must not invoke the callback again.
	if err := rt.authorizeShellCommand(context.Background(), "shell-exec", "npm run build"); err != nil {
		t.Fatalf("second call should succeed (already approved for session): %v", err)
	}
	if calls != 1 {
		t.Errorf("expected callback to be called exactly once, got %d", calls)
	}
}

// --- restricted mode tests ---

func TestRestricted_BlocksNonWhitelistedCommandWithoutCallback(t *testing.T) {
	rt := newPermRuntime(config.PermissionRestricted, nil, nil)
	if err := rt.authorizeShellCommand(context.Background(), "shell-exec", "npm run build"); err == nil {
		t.Error("restricted should block non-whitelisted commands when no callback is set")
	}
}

func TestRestricted_AllowsWhitelistedCommands(t *testing.T) {
	rt := newPermRuntime(config.PermissionRestricted, nil, nil)
	for _, cmd := range []string{"ls", "pwd", "git diff", "go build ./..."} {
		if err := rt.authorizeShellCommand(context.Background(), "shell-exec", cmd); err != nil {
			t.Errorf("restricted should allow whitelisted command %q: %v", cmd, err)
		}
	}
}

func TestRestricted_BlocksDangerousCommands(t *testing.T) {
	callback := func(_ context.Context, _ ShellApprovalRequest) (ShellApprovalDecision, error) {
		return ShellApprovalAllowOnce, nil
	}
	rt := newPermRuntime(config.PermissionRestricted, callback, nil)
	if err := rt.authorizeShellCommand(context.Background(), "shell-exec", "git reset --hard HEAD"); err == nil {
		t.Error("restricted should block dangerous commands regardless of callback")
	}
}

func TestRestricted_RespectsPermissionRuleDeny(t *testing.T) {
	rules := []config.PermissionRule{
		{Permission: "execute", Pattern: "*", Action: config.RuleDeny},
	}
	callback := func(_ context.Context, _ ShellApprovalRequest) (ShellApprovalDecision, error) {
		return ShellApprovalAllowOnce, nil
	}
	rt := newPermRuntime(config.PermissionRestricted, callback, rules)
	if err := rt.authorizeShellCommand(context.Background(), "shell-exec", "npm run build"); err == nil {
		t.Error("restricted should block when a permission rule denies the command")
	}
}

// --- top-level Execute pre-approval distinction ---

func TestCoder_AskFirst_RequiresPreApproval(t *testing.T) {
	c := Coder{}
	_, err := c.Execute(context.Background(), "do stuff", config.PermissionAskFirst, false)
	if err == nil {
		t.Error("Coder.Execute in ask-first mode must fail without pre-approval")
	}
	_, err = c.Execute(context.Background(), "do stuff", config.PermissionAskFirst, true)
	if err != nil {
		t.Errorf("Coder.Execute in ask-first mode must succeed with pre-approval: %v", err)
	}
}

func TestCoder_Restricted_NoPreApprovalRequired(t *testing.T) {
	c := Coder{}
	_, err := c.Execute(context.Background(), "do stuff", config.PermissionRestricted, false)
	if err != nil {
		t.Errorf("Coder.Execute in restricted mode must not require pre-approval: %v", err)
	}
}

func TestCoder_YOLO_NoPreApprovalRequired(t *testing.T) {
	c := Coder{}
	_, err := c.Execute(context.Background(), "do stuff", config.PermissionYOLO, false)
	if err != nil {
		t.Errorf("Coder.Execute in yolo mode must not require pre-approval: %v", err)
	}
}

// --- sub-agent permission inheritance ---

// newScriptedManager creates a local HTTP provider that serves canned LLM responses.
func newScriptedManager(t *testing.T, responses []string) (*provider.Manager, string, string) {
	t.Helper()
	var idx atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		i := int(idx.Add(1)) - 1
		if i >= len(responses) {
			http.Error(w, "no more scripted responses", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		resp := map[string]any{
			"id":     "chatcmpl-test",
			"object": "chat.completion",
			"choices": []map[string]any{
				{"index": 0, "message": map[string]any{"role": "assistant", "content": responses[i]}, "finish_reason": "stop"},
			},
			"usage": map[string]any{"total_tokens": 30},
		}
		json.NewEncoder(w).Encode(resp) //nolint:errcheck
	}))
	t.Cleanup(srv.Close)
	pm := provider.NewManager()
	pm.AddLocalModels([]provider.Model{{Provider: srv.URL, Name: "test-model", Local: true}})
	return pm, srv.URL, "test-model"
}

// TestYOLO_PropagatesPermissionToSubAgents verifies that when an orchestrator runs in
// YOLO mode it spawns sub-agents that also run in YOLO mode (so they never invoke
// the approval callback for shell commands).
func TestYOLO_PropagatesPermissionToSubAgents(t *testing.T) {
	// Orchestrator asks the "code" sub-agent to run a shell command.
	// If permission is NOT propagated, the sub-agent (spec.Permission = restricted)
	// would invoke the approval callback and block.
	pm, providerURL, modelName := newScriptedManager(t, []string{
		// Orchestrator delegates to "code" worker
		`TOOL_CALL {"name":"agent","arguments":{"target":"code","task":"run npm build"}}`,
		// Code worker runs shell-exec
		`TOOL_CALL {"name":"shell-exec","arguments":{"command":"npm run build"}}`,
		"FINAL\ndone",
	})

	callbackInvoked := false
	callback := func(_ context.Context, _ ShellApprovalRequest) (ShellApprovalDecision, error) {
		callbackInvoked = true
		return ShellApprovalDeny, nil
	}

	manifest := config.AgentManifest{
		Version:      2,
		DefaultAgent: "orch",
		Runtime: config.RuntimePolicy{
			DefaultPermission: config.PermissionAskFirst,
			DefaultTimeoutSec: 60,
			LogToolCalls:      true,
			Delegation:        config.DelegationPolicy{MaxParallelWorkers: 2, MaxDepth: 3},
		},
		Tools: []config.ToolSpec{
			{ID: "agent", Name: "Agent", Kind: "builtin", Enabled: true, TimeoutSec: 60, PermittedActions: []string{"read", "write", "execute", "git", "search", "plan", "ask"}, PrimaryOnly: true},
			{ID: "shell-exec", Name: "Shell", Kind: "builtin", Enabled: true, TimeoutSec: 30, RequiresApproval: true, PermittedActions: []string{"execute"}},
			{ID: "comment", Name: "Comment", Kind: "builtin", Enabled: true, TimeoutSec: 5, PermittedActions: []string{"read"}},
		},
		Agents: []config.AgentSpec{
			{
				ID:               "orch",
				Name:             "Orch",
				Mode:             "orchestrator",
				Role:             config.AgentRolePrimary,
				AllowedTools:     []string{"agent", "comment"},
				PermittedActions: []string{"read", "write", "execute", "git", "search", "plan", "ask"},
				Permission:       config.PermissionYOLO,
				MaxSteps:         4,
				Enabled:          true,
			},
			{
				ID:               "code",
				Name:             "Code",
				Mode:             "worker",
				Role:             config.AgentRoleWorker,
				AllowedTools:     []string{"shell-exec", "comment"},
				PermittedActions: []string{"execute"},
				// Intentionally set to restricted in the manifest — YOLO must override it.
				Permission: config.PermissionRestricted,
				MaxSteps:   4,
				Enabled:    true,
			},
		},
	}
	if err := manifest.Validate(); err != nil {
		t.Fatalf("manifest validate: %v", err)
	}

	ag := LLMAgent{
		Spec:            manifest.Agents[0],
		ProviderManager: pm,
		ProviderName:    func() string { return providerURL },
		ModelName:       func() string { return modelName },
		CWD:             t.TempDir(),
		Manifest:        &manifest,
		ShellApproval:   callback,
	}

	// Run in YOLO — the sub-agent must NOT invoke the approval callback.
	ag.Spec.Permission = config.PermissionYOLO
	_, err := ag.Run(context.Background(), "build the project")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if callbackInvoked {
		t.Error("approval callback was invoked on a sub-agent even though the parent runs in YOLO mode — permission did not propagate")
	}
}
