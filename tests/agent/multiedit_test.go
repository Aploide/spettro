package agent_test

// Tests for the multi-edit builtin: several find/replace edits applied to one
// file in a single atomic tool call.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"spettro/internal/agent"
	"spettro/internal/config"
)

// multiEditManifest returns a minimal manifest whose single agent may call
// multi-edit (and comment), mirroring the default manifest's tool policy.
func multiEditManifest(t *testing.T, permission config.PermissionLevel) config.AgentManifest {
	t.Helper()
	manifest := config.AgentManifest{
		Version:      1,
		DefaultAgent: "code",
		Runtime: config.RuntimePolicy{
			DefaultPermission: permission,
			DefaultTimeoutSec: 60,
			LogToolCalls:      true,
		},
		Tools: []config.ToolSpec{
			{ID: "multi-edit", Name: "Multi Edit", Kind: "builtin", Enabled: true, TimeoutSec: 60, RequiresApproval: true, PermittedActions: []string{"write"}},
			{ID: "comment", Name: "Comment", Kind: "builtin", Enabled: true, TimeoutSec: 10, PermittedActions: []string{"read"}},
		},
		Agents: []config.AgentSpec{
			{
				ID:               "code",
				Name:             "Code",
				Mode:             "worker",
				AllowedTools:     []string{"multi-edit", "comment"},
				PermittedActions: []string{"read", "write"},
				Permission:       permission,
				MaxSteps:         4,
				Enabled:          true,
			},
		},
	}
	if err := manifest.Validate(); err != nil {
		t.Fatalf("manifest validate: %v", err)
	}
	return manifest
}

func multiEditAgent(t *testing.T, dir string, permission config.PermissionLevel, responses []string) agent.LLMAgent {
	t.Helper()
	pm, providerName, modelName := scriptedManager(t, responses)
	manifest := multiEditManifest(t, permission)
	return agent.LLMAgent{
		Spec:            manifest.Agents[0],
		ProviderManager: pm,
		ProviderName:    func() string { return providerName },
		ModelName:       func() string { return modelName },
		CWD:             dir,
		Manifest:        &manifest,
	}
}

func TestMultiEdit_SequentialEdits(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "main.go")
	os.WriteFile(path, []byte("package main\n\nfunc oldName() {}\n\nvar x = oldName\n"), 0o644) //nolint:errcheck

	// The second edit matches text produced by the first (renamed call site),
	// proving edits apply against the in-memory result of the previous one.
	ag := multiEditAgent(t, dir, config.PermissionYOLO, []string{
		`TOOL_CALL {"tool":"multi-edit","args":{"path":"main.go","edits":[` +
			`{"old_string":"func oldName() {}","new_string":"func newName() {}"},` +
			`{"old_string":"var x = oldName","new_string":"var x = newName"}]}}`,
		"FINAL\nRenamed.",
	})
	result, err := ag.Run(context.Background(), "Rename oldName to newName.")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(result.Tools) == 0 || result.Tools[0].Status != "success" {
		t.Fatalf("expected successful multi-edit trace, got: %+v", result.Tools)
	}
	data, _ := os.ReadFile(path)
	want := "package main\n\nfunc newName() {}\n\nvar x = newName\n"
	if string(data) != want {
		t.Errorf("unexpected file content:\n%q\nwant:\n%q", string(data), want)
	}
	if !strings.Contains(result.Tools[0].Output, "2 edits") {
		t.Errorf("expected edit count in output, got: %q", result.Tools[0].Output)
	}
}

func TestMultiEdit_FailedMatchLeavesFileUntouched(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "main.go")
	original := "package main\n\nfunc a() {}\n"
	os.WriteFile(path, []byte(original), 0o644) //nolint:errcheck

	// First edit matches, second does not: the whole call must fail and the
	// first edit must not be applied.
	ag := multiEditAgent(t, dir, config.PermissionYOLO, []string{
		`TOOL_CALL {"tool":"multi-edit","args":{"path":"main.go","edits":[` +
			`{"old_string":"func a() {}","new_string":"func b() {}"},` +
			`{"old_string":"does-not-exist","new_string":"whatever"}]}}`,
		"FINAL\nFailed.",
	})
	result, err := ag.Run(context.Background(), "Edit main.go.")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(result.Tools) == 0 || result.Tools[0].Status != "error" {
		t.Fatalf("expected multi-edit error trace, got: %+v", result.Tools)
	}
	if !strings.Contains(result.Tools[0].Output, "not found") {
		t.Errorf("expected not-found error, got: %q", result.Tools[0].Output)
	}
	data, _ := os.ReadFile(path)
	if string(data) != original {
		t.Errorf("file must be untouched after failed call, got: %q", string(data))
	}
}

func TestMultiEdit_AmbiguousMatchFailsWithoutReplaceAll(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "notes.txt")
	original := "foo\nfoo\n"
	os.WriteFile(path, []byte(original), 0o644) //nolint:errcheck

	ag := multiEditAgent(t, dir, config.PermissionYOLO, []string{
		`TOOL_CALL {"tool":"multi-edit","args":{"path":"notes.txt","edits":[` +
			`{"old_string":"foo","new_string":"bar"}]}}`,
		"FINAL\nFailed.",
	})
	result, err := ag.Run(context.Background(), "Edit notes.txt.")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(result.Tools) == 0 || result.Tools[0].Status != "error" {
		t.Fatalf("expected ambiguous-match error trace, got: %+v", result.Tools)
	}
	if !strings.Contains(result.Tools[0].Output, "matches 2 times") {
		t.Errorf("expected ambiguity error, got: %q", result.Tools[0].Output)
	}
	data, _ := os.ReadFile(path)
	if string(data) != original {
		t.Errorf("file must be untouched, got: %q", string(data))
	}
}

func TestMultiEdit_ReplaceAll(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "notes.txt")
	os.WriteFile(path, []byte("foo\nfoo\nbaz\n"), 0o644) //nolint:errcheck

	ag := multiEditAgent(t, dir, config.PermissionYOLO, []string{
		`TOOL_CALL {"tool":"multi-edit","args":{"path":"notes.txt","edits":[` +
			`{"old_string":"foo","new_string":"bar","replace_all":true},` +
			`{"old_string":"baz","new_string":"qux"}]}}`,
		"FINAL\nDone.",
	})
	result, err := ag.Run(context.Background(), "Edit notes.txt.")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(result.Tools) == 0 || result.Tools[0].Status != "success" {
		t.Fatalf("expected successful trace, got: %+v", result.Tools)
	}
	data, _ := os.ReadFile(path)
	if string(data) != "bar\nbar\nqux\n" {
		t.Errorf("unexpected content: %q", string(data))
	}
	if !strings.Contains(result.Tools[0].Output, "3 replacements") {
		t.Errorf("expected replacement count, got: %q", result.Tools[0].Output)
	}
}

func TestMultiEdit_ApprovalShowsCombinedDiff(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "notes.txt")
	original := "alpha\nbeta\n"
	os.WriteFile(path, []byte(original), 0o644) //nolint:errcheck

	ag := multiEditAgent(t, dir, config.PermissionRestricted, []string{
		`TOOL_CALL {"tool":"multi-edit","args":{"path":"notes.txt","edits":[` +
			`{"old_string":"alpha","new_string":"ALPHA"},` +
			`{"old_string":"beta","new_string":"BETA"}]}}`,
		"FINAL\nDenied.",
	})
	var gotDiff string
	ag.ShellApproval = func(_ context.Context, req agent.ShellApprovalRequest) (agent.ShellApprovalDecision, error) {
		gotDiff = req.Diff
		return agent.ShellApprovalDeny, nil
	}
	result, err := ag.Run(context.Background(), "Edit notes.txt.")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// The approval prompt must carry the combined diff of both edits.
	if !strings.Contains(gotDiff, "+ALPHA") || !strings.Contains(gotDiff, "+BETA") {
		t.Errorf("expected combined diff in approval request, got: %q", gotDiff)
	}
	if len(result.Tools) == 0 || result.Tools[0].Status != "error" {
		t.Fatalf("expected denied trace, got: %+v", result.Tools)
	}
	data, _ := os.ReadFile(path)
	if string(data) != original {
		t.Errorf("file must be untouched after denial, got: %q", string(data))
	}
}
