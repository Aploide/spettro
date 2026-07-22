package agent_test

// Integration tests for LLMCoder using a scripted HTTP server.
// Tests the full pipeline: LLM response → tool execution → loop → result.

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"spettro/internal/agent"
	"spettro/internal/config"
)

func coder(srv *httptest.Server, dir string) agent.LLMCoder {
	pm, providerName := testProvider(srv)
	return agent.LLMCoder{
		ProviderManager: pm,
		ProviderName:    func() string { return providerName },
		ModelName:       func() string { return "fake-model" },
		CWD:             dir,
	}
}

// ── basic execution ────────────────────────────────────────────────────────────

func TestCoder_ReadAndFinish(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module myapp\ngo 1.22\n"), 0o644) //nolint:errcheck

	srv := scriptedServer(t, []string{
		`TOOL_CALL {"tool":"file-read","args":{"path":"go.mod"}}`,
		"FINAL\nRead go.mod successfully.",
	})
	result, err := coder(srv, dir).Execute(context.Background(), "Read go.mod.", config.PermissionYOLO, true)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Content != "Read go.mod successfully." {
		t.Errorf("unexpected content: %q", result.Content)
	}
	if len(result.Tools) != 1 || result.Tools[0].Name != "file-read" {
		t.Errorf("unexpected tool traces: %+v", result.Tools)
	}
}

func TestCoder_RepoSearchThenFinish(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n"), 0o644) //nolint:errcheck

	srv := scriptedServer(t, []string{
		`TOOL_CALL {"tool":"repo-search","args":{"query":""}}`,
		"FINAL\nFound files.",
	})
	result, err := coder(srv, dir).Execute(context.Background(), "List files.", config.PermissionYOLO, true)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(result.Content, "Found files.") {
		t.Errorf("unexpected content: %q", result.Content)
	}
}

// ── file write behaviour ───────────────────────────────────────────────────────

func TestCoder_WriteNewFile(t *testing.T) {
	dir := t.TempDir()

	srv := scriptedServer(t, []string{
		`TOOL_CALL {"tool":"file-write","args":{"path":"SPETTRO.md","content":"# SPETTRO.md\n"}}`,
		"FINAL\nSPETTRO.md created.",
	})
	result, err := coder(srv, dir).Execute(context.Background(), "Write SPETTRO.md.", config.PermissionYOLO, true)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Content != "SPETTRO.md created." {
		t.Errorf("unexpected content: %q", result.Content)
	}
	data, err := os.ReadFile(filepath.Join(dir, "SPETTRO.md"))
	if err != nil {
		t.Fatalf("SPETTRO.md not created: %v", err)
	}
	if !strings.Contains(string(data), "# SPETTRO.md") {
		t.Errorf("unexpected file content: %q", string(data))
	}
}

func TestCoder_WriteExistingFileWithoutRead_ToolErrors(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "existing.txt"), []byte("original\n"), 0o644) //nolint:errcheck

	// LLM tries to write existing file without reading it first.
	// Tool returns an error which is fed back to the LLM.
	// LLM then outputs FINAL with a message about the error.
	srv := scriptedServer(t, []string{
		`TOOL_CALL {"tool":"file-write","args":{"path":"existing.txt","content":"overwrite\n"}}`,
		"FINAL\nCould not write: read first.",
	})
	result, err := coder(srv, dir).Execute(context.Background(), "Overwrite existing.txt.", config.PermissionYOLO, true)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	// File must not have been overwritten.
	data, _ := os.ReadFile(filepath.Join(dir, "existing.txt"))
	if string(data) != "original\n" {
		t.Errorf("file was incorrectly overwritten: %q", string(data))
	}
	// Tool trace should show the error.
	if len(result.Tools) == 0 || result.Tools[0].Status != "error" {
		t.Errorf("expected tool error trace, got: %+v", result.Tools)
	}
}

func TestCoder_WriteExistingFileAfterRead_Succeeds(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "existing.txt"), []byte("original\n"), 0o644) //nolint:errcheck

	srv := scriptedServer(t, []string{
		`TOOL_CALL {"tool":"file-read","args":{"path":"existing.txt"}}`,
		`TOOL_CALL {"tool":"file-write","args":{"path":"existing.txt","content":"updated\n"}}`,
		"FINAL\nUpdated.",
	})
	_, err := coder(srv, dir).Execute(context.Background(), "Update existing.txt.", config.PermissionYOLO, true)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "existing.txt"))
	if string(data) != "updated\n" {
		t.Errorf("expected 'updated\\n', got %q", string(data))
	}
}

func TestCoder_ReadOutsideWorkspace_ToolErrors(t *testing.T) {
	dir := t.TempDir()

	srv := scriptedServer(t, []string{
		`TOOL_CALL {"tool":"file-read","args":{"path":"/etc/passwd"}}`,
		"FINAL\nBlocked.",
	})
	result, err := coder(srv, dir).Execute(context.Background(), "Read /etc/passwd.", config.PermissionYOLO, true)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(result.Tools) == 0 || result.Tools[0].Status != "error" {
		t.Errorf("expected tool error for out-of-workspace read, got: %+v", result.Tools)
	}
}

// ── nudge behaviour ────────────────────────────────────────────────────────────

func TestCoder_AcceptsFinalWithoutTool(t *testing.T) {
	dir := t.TempDir()

	// FINAL without any tool is accepted as the answer (no forced tool use).
	srv := scriptedServer(t, []string{
		"FINAL\nSkipped tools.",
	})
	result, err := coder(srv, dir).Execute(context.Background(), "Do something.", config.PermissionYOLO, true)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Content != "Skipped tools." {
		t.Errorf("unexpected content: %q", result.Content)
	}
}

func TestCoder_PlainTextAfterToolIsFinalAnswer(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module m\n"), 0o644) //nolint:errcheck

	// With native tool calling, a response without tool calls IS the final answer.
	srv := scriptedServer(t, []string{
		`TOOL_CALL {"tool":"file-read","args":{"path":"go.mod"}}`,
		"Actual answer.",
	})
	result, err := coder(srv, dir).Execute(context.Background(), "Read go.mod.", config.PermissionYOLO, true)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Content != "Actual answer." {
		t.Errorf("unexpected content: %q", result.Content)
	}
}

// ── ToolCallback ───────────────────────────────────────────────────────────────

func TestCoder_ToolCallback(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module m\n"), 0o644) //nolint:errcheck

	srv := scriptedServer(t, []string{
		`TOOL_CALL {"tool":"file-read","args":{"path":"go.mod"}}`,
		"FINAL\nDone.",
	})
	var events []agent.ToolTrace
	c := coder(srv, dir)
	c.ToolCallback = func(tr agent.ToolTrace) { events = append(events, tr) }

	_, err := c.Execute(context.Background(), "Read go.mod.", config.PermissionYOLO, true)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	// Expect 2 events: "running" before + final status after.
	if len(events) != 2 {
		t.Errorf("expected 2 callback events, got %d: %+v", len(events), events)
	}
	if events[0].Status != "running" {
		t.Errorf("first event should be running, got %q", events[0].Status)
	}
	if events[1].Status != "success" {
		t.Errorf("second event should be success, got %q", events[1].Status)
	}
}

func TestCoder_ShellExec_RestrictedNeedsApproval(t *testing.T) {
	dir := t.TempDir()
	srv := scriptedServer(t, []string{
		`TOOL_CALL {"tool":"shell-exec","args":{"command":"echo hi"}}`,
		"FINAL\nDone.",
	})
	c := coder(srv, dir)
	c.ShellApproval = func(context.Context, agent.ShellApprovalRequest) (agent.ShellApprovalDecision, error) {
		return agent.ShellApprovalDeny, nil
	}
	result, err := c.Execute(context.Background(), "Run echo hi.", config.PermissionRestricted, true)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(result.Tools) == 0 || result.Tools[0].Status != "error" {
		t.Fatalf("expected denied shell-exec error trace, got: %+v", result.Tools)
	}
	if !strings.Contains(result.Tools[0].Output, "denied by user") {
		t.Fatalf("expected denied message, got: %q", result.Tools[0].Output)
	}
}

func TestCoder_ShellExec_RestrictedAllowAlwaysPersists(t *testing.T) {
	dir := t.TempDir()
	srv := scriptedServer(t, []string{
		`TOOL_CALL {"tool":"shell-exec","args":{"command":"echo hi"}}`,
		"FINAL\nDone.",
	})
	c := coder(srv, dir)
	c.ShellApproval = func(context.Context, agent.ShellApprovalRequest) (agent.ShellApprovalDecision, error) {
		return agent.ShellApprovalAllowAlways, nil
	}
	if _, err := c.Execute(context.Background(), "Run echo hi.", config.PermissionRestricted, true); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, ".spettro", "allowed_commands.json"))
	if err != nil {
		t.Fatalf("allowed_commands.json missing: %v", err)
	}
	if !strings.Contains(string(data), "echo hi") {
		t.Fatalf("expected persisted command, got: %s", string(data))
	}
}

func TestCoder_ShellExec_RestrictedAlwaysAllowedCommand(t *testing.T) {
	dir := t.TempDir()
	srv := scriptedServer(t, []string{
		`TOOL_CALL {"tool":"shell-exec","args":{"command":"pwd"}}`,
		"FINAL\nDone.",
	})
	c := coder(srv, dir)
	result, err := c.Execute(context.Background(), "Run pwd.", config.PermissionRestricted, true)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(result.Tools) == 0 || result.Tools[0].Status != "success" {
		t.Fatalf("expected successful shell-exec trace, got: %+v", result.Tools)
	}
}
