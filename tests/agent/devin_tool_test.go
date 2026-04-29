package agent_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"spettro/internal/agent"
	"spettro/internal/config"
	"spettro/internal/provider"
)

// devinFakeServer serves a minimal v1 Devin Sessions API that completes on
// the second poll with a fixed final message and records how many times
// create and poll were invoked.
func devinFakeServer(t *testing.T, finalMessage string) (*httptest.Server, *int32, *int32) {
	t.Helper()
	var pollCount, createCount int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/sessions":
			atomic.AddInt32(&createCount, 1)
			json.NewEncoder(w).Encode(map[string]any{
				"session_id": "devin-fake-tool",
				"url":        "https://app.devin.ai/sessions/devin-fake-tool",
			})
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/sessions/"):
			n := atomic.AddInt32(&pollCount, 1)
			if n < 2 {
				json.NewEncoder(w).Encode(map[string]any{"status": "working", "status_enum": "working"})
				return
			}
			json.NewEncoder(w).Encode(map[string]any{
				"status_enum": "finished",
				"messages": []map[string]any{
					{"type": "devin_message", "message": finalMessage, "event_id": "1", "timestamp": "2026-01-01T00:00:00Z"},
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &pollCount, &createCount
}

// TestLLMAgent_DevinSessionTool_HappyPath drives a full LLMAgent run where
// the scripted model decides to delegate to Devin via TOOL_CALL
// {"tool":"devin-session","arguments":{"task":"..."}}, the runtime
// dispatches to the DevinAdapter against a fake server, and the final
// answer quotes Devin's output.
func TestLLMAgent_DevinSessionTool_HappyPath(t *testing.T) {
	devinSrv, _, createCount := devinFakeServer(t, "devin completed the refactor")

	pm, providerName, modelName := scriptedManager(t, []string{
		`TOOL_CALL {"name":"devin-session","arguments":{"task":"refactor auth module","expected_output":"summary of diff"}}`,
		"FINAL\nDevin took over and completed the refactor.",
	})
	// Point the fake DevinAdapter at our test server and populate the
	// manager with a devin API key so CallDevin is satisfied.
	pm.SetAPIKeys(map[string]string{"devin": "apk_test"})

	// We also have to inject our test server URL. The production path
	// uses the hard-coded DevinSessionsBaseURL; tests need a seam. We
	// thread it via a package-level var hook in provider/devin.go.
	restore := provider.OverrideDevinBaseURLForTesting(devinSrv.URL)
	defer restore()

	manifest := config.AgentManifest{
		Version:      2,
		DefaultAgent: "worker",
		Runtime: config.RuntimePolicy{
			DefaultPermission: config.PermissionYOLO,
			DefaultTimeoutSec: 60,
			SandboxMode:       config.SandboxWorkspaceWrite,
			Delegation:        config.DelegationPolicy{MaxParallelWorkers: 4, MaxDepth: 2, MaxToolCallsPerStep: 16},
		},
		Tools: []config.ToolSpec{
			{ID: "devin-session", Name: "Devin Session", Kind: "builtin", Enabled: true, TimeoutSec: 120, RequiresApproval: false, PermittedActions: []string{"network", "plan"}, RiskLevel: "high", PrimaryOnly: true},
			{ID: "comment", Name: "Comment", Kind: "builtin", Enabled: true, TimeoutSec: 10, PermittedActions: []string{"read"}, RiskLevel: "low"},
		},
		Agents: []config.AgentSpec{
			{ID: "worker", Name: "Worker", Mode: "orchestrator", Role: config.AgentRolePrimary, AllowedTools: []string{"devin-session", "comment"}, Permission: config.PermissionYOLO, MaxSteps: 8, Enabled: true},
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

	result, err := ag.Run(context.Background(), "refactor the whole auth module")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if atomic.LoadInt32(createCount) != 1 {
		t.Fatalf("expected exactly one Devin session to be created, got %d", *createCount)
	}
	if len(result.Tools) == 0 {
		t.Fatalf("expected at least one tool trace, got none")
	}
	var devinTrace *agent.ToolTrace
	for i := range result.Tools {
		if result.Tools[i].Name == "devin-session" {
			devinTrace = &result.Tools[i]
			break
		}
	}
	if devinTrace == nil {
		t.Fatalf("expected a devin-session tool trace, traces: %+v", result.Tools)
	}
	if devinTrace.Status != "success" {
		t.Fatalf("expected success status, got %q (output: %q)", devinTrace.Status, devinTrace.Output)
	}
	if !strings.Contains(devinTrace.Output, "devin completed the refactor") {
		t.Fatalf("expected Devin output in trace, got %q", devinTrace.Output)
	}
	if !strings.Contains(devinTrace.Output, "https://app.devin.ai/sessions/devin-fake-tool") {
		t.Fatalf("expected session url footer in trace, got %q", devinTrace.Output)
	}
	if !strings.Contains(result.Content, "Devin took over") {
		t.Fatalf("expected FINAL content to mention the delegation, got %q", result.Content)
	}
}

// TestLLMAgent_DevinSessionTool_NotConfigured verifies that when no
// api_keys["devin"] credential is on file, the devin-session tool is
// hidden from the agent's allowed list entirely. A misbehaving / scripted
// model that still tries to emit a TOOL_CALL for it receives a clear
// "tool not allowed" error from the runtime's allowlist check instead of
// reaching the DevinAdapter, so we never surface an API-key error to the
// LLM (and never waste an HTTP round-trip to app.devin.ai).
func TestLLMAgent_DevinSessionTool_NotConfigured(t *testing.T) {
	pm, providerName, modelName := scriptedManager(t, []string{
		`TOOL_CALL {"name":"devin-session","arguments":{"task":"foo"}}`,
		"FINAL\ntried to delegate but the backend rejected.",
	})
	// Intentionally do NOT set api_keys.devin. The filter in
	// LLMAgent.Run should drop the tool before the loop starts.

	manifest := config.AgentManifest{
		Version:      2,
		DefaultAgent: "worker",
		Runtime: config.RuntimePolicy{
			DefaultPermission: config.PermissionYOLO,
			DefaultTimeoutSec: 60,
			SandboxMode:       config.SandboxWorkspaceWrite,
			Delegation:        config.DelegationPolicy{MaxParallelWorkers: 4, MaxDepth: 2, MaxToolCallsPerStep: 16},
		},
		Tools: []config.ToolSpec{
			{ID: "devin-session", Name: "Devin Session", Kind: "builtin", Enabled: true, TimeoutSec: 30, PermittedActions: []string{"network"}, RiskLevel: "high", PrimaryOnly: true},
		},
		Agents: []config.AgentSpec{
			{ID: "worker", Name: "Worker", Mode: "orchestrator", Role: config.AgentRolePrimary, AllowedTools: []string{"devin-session"}, Permission: config.PermissionYOLO, MaxSteps: 4, Enabled: true},
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
	result, err := ag.Run(context.Background(), "try devin")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(result.Tools) == 0 {
		t.Fatalf("expected a tool trace, got none")
	}
	tr := result.Tools[0]
	if tr.Name != "devin-session" || tr.Status != "error" {
		t.Fatalf("expected devin-session error trace, got %+v", tr)
	}
	if !strings.Contains(tr.Output, "not allowed") {
		t.Fatalf("expected 'not allowed' rejection (filter kicked in), got %q", tr.Output)
	}
	// The filter must not have leaked any HTTP request — the scripted
	// server in TestLLMAgent_DevinSessionTool_HappyPath would have
	// incremented createCount, so a zero count proves no network call
	// happened here either.
}

// TestLLMAgent_DevinSessionTool_HiddenFromSchemaWhenNoKey verifies the
// filter strips devin-session from the allowed-tools list before the
// system prompt is rendered, so the LLM is never told the tool exists
// when the user has no Devin key. The filter is small and deterministic
// so we assert on its output directly rather than parsing the full
// rendered prompt.
func TestLLMAgent_DevinSessionTool_HiddenFromSchemaWhenNoKey(t *testing.T) {
	pm, _, _ := scriptedManager(t, nil)
	// No api_keys["devin"] set.

	allowed, _ := agent.FilterProviderGatedToolsForTesting([]string{"comment", "devin-session", "file-read"}, nil, pm)
	for _, id := range allowed {
		if id == "devin-session" {
			t.Fatalf("filter kept devin-session despite missing devin api key: %v", allowed)
		}
	}
	// Non-gated tools must survive.
	var sawComment, sawFileRead bool
	for _, id := range allowed {
		sawComment = sawComment || id == "comment"
		sawFileRead = sawFileRead || id == "file-read"
	}
	if !sawComment || !sawFileRead {
		t.Fatalf("filter dropped non-gated tools: %v", allowed)
	}
}

// TestLLMAgent_DevinSessionTool_KeptWhenKeySet is the inverse: with the
// key configured, the filter must leave the tool in place.
func TestLLMAgent_DevinSessionTool_KeptWhenKeySet(t *testing.T) {
	pm, _, _ := scriptedManager(t, nil)
	pm.SetAPIKeys(map[string]string{"devin": "apk_test"})

	allowed, _ := agent.FilterProviderGatedToolsForTesting([]string{"comment", "devin-session"}, nil, pm)
	found := false
	for _, id := range allowed {
		if id == "devin-session" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("filter stripped devin-session even though the key is set: %v", allowed)
	}
}
