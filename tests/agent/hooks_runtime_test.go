package agent_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"spettro/internal/agent"
	"spettro/internal/config"
)

func TestRuntimeHook_PreToolUseDenyBlocksTool(t *testing.T) {
	pm, providerName, modelName := scriptedManager(t, []string{
		`TOOL_CALL {"tool":"bash","args":{"command":"echo hello"}}`,
		"FINAL\ndone",
	})
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".spettro"), 0o755); err != nil {
		t.Fatal(err)
	}
	hooksJSON := `{"hooks":[{"id":"deny-bash","event":"PreToolUse","matcher":"bash","command":"echo '{\"decision\":\"deny\",\"reason\":\"blocked by test\"}'"}]}`
	if err := os.WriteFile(filepath.Join(dir, ".spettro", "hooks.json"), []byte(hooksJSON), 0o644); err != nil {
		t.Fatal(err)
	}

	ag := agent.LLMAgent{
		Spec:            config.AgentSpec{ID: "code", Mode: "worker", AllowedTools: []string{"bash"}, Permission: config.PermissionRestricted, MaxSteps: 4, Enabled: true},
		ProviderManager: pm,
		ProviderName:    func() string { return providerName },
		ModelName:       func() string { return modelName },
		CWD:             dir,
	}
	result, err := ag.Run(context.Background(), "run command")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(result.Tools) == 0 {
		t.Fatalf("expected tool trace")
	}
	if result.Tools[0].Status != "error" || !strings.Contains(result.Tools[0].Output, "blocked by hook") {
		t.Fatalf("expected blocked by hook, got %+v", result.Tools[0])
	}
}

func TestRuntimeHook_PreToolUseMutateArgs(t *testing.T) {
	pm, providerName, modelName := scriptedManager(t, []string{
		`TOOL_CALL {"tool":"bash","args":{"command":"echo original"}}`,
		"FINAL\ndone",
	})
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".spettro"), 0o755); err != nil {
		t.Fatal(err)
	}
	hooksJSON := `{"hooks":[{"id":"rewrite-bash","event":"PreToolUse","matcher":"bash","command":"echo '{\"decision\":\"allow\",\"updated_args\":{\"command\":\"echo rewritten\"}}'"}]}`
	if err := os.WriteFile(filepath.Join(dir, ".spettro", "hooks.json"), []byte(hooksJSON), 0o644); err != nil {
		t.Fatal(err)
	}

	ag := agent.LLMAgent{
		Spec:            config.AgentSpec{ID: "code", Mode: "worker", AllowedTools: []string{"bash"}, Permission: config.PermissionRestricted, MaxSteps: 4, Enabled: true},
		ProviderManager: pm,
		ProviderName:    func() string { return providerName },
		ModelName:       func() string { return modelName },
		CWD:             dir,
		ShellApproval: func(context.Context, agent.ShellApprovalRequest) (agent.ShellApprovalDecision, error) {
			return agent.ShellApprovalAllowOnce, nil
		},
	}
	result, err := ag.Run(context.Background(), "run command")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(result.Tools) == 0 {
		t.Fatalf("expected tool trace")
	}
	if result.Tools[0].Status != "success" || !strings.Contains(result.Tools[0].Output, "rewritten") {
		t.Fatalf("expected rewritten output, got %+v", result.Tools[0])
	}
}

func TestRuntimeHook_PermissionRequestAllowBypassesPrompt(t *testing.T) {
	pm, providerName, modelName := scriptedManager(t, []string{
		`TOOL_CALL {"tool":"shell-exec","args":{"command":"echo from-hook"}}`,
		"FINAL\ndone",
	})
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".spettro"), 0o755); err != nil {
		t.Fatal(err)
	}
	hooksJSON := `{"hooks":[{"id":"allow-shell","event":"PermissionRequest","matcher":"shell-exec","command":"echo '{\"decision\":\"allow\",\"reason\":\"approved by hook\"}'"}]}`
	if err := os.WriteFile(filepath.Join(dir, ".spettro", "hooks.json"), []byte(hooksJSON), 0o644); err != nil {
		t.Fatal(err)
	}

	ag := agent.LLMAgent{
		Spec:            config.AgentSpec{ID: "code", Mode: "worker", AllowedTools: []string{"bash"}, Permission: config.PermissionRestricted, MaxSteps: 4, Enabled: true},
		ProviderManager: pm,
		ProviderName:    func() string { return providerName },
		ModelName:       func() string { return modelName },
		CWD:             dir,
	}
	result, err := ag.Run(context.Background(), "run shell")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(result.Tools) == 0 || result.Tools[0].Status != "success" {
		t.Fatalf("expected successful shell-exec via hook allowance, got %+v", result.Tools)
	}
	if !strings.Contains(result.Tools[0].Output, "from-hook") {
		t.Fatalf("expected command output, got %+v", result.Tools[0])
	}
}
