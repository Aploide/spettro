package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"spettro/internal/memory"
)

func TestRunSaveMemoryUserScope(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	rt := &toolRuntime{cwd: t.TempDir()}

	out, err := rt.runSaveMemory([]byte(`{"fact":"prefers Italian variable names"}`))
	if err != nil {
		t.Fatalf("runSaveMemory: %v", err)
	}
	if !strings.Contains(out, "user memory") {
		t.Fatalf("unexpected output: %q", out)
	}
	data, err := os.ReadFile(filepath.Join(home, ".spettro", "memory.md"))
	if err != nil {
		t.Fatalf("user memory file missing: %v", err)
	}
	if !strings.Contains(string(data), "- prefers Italian variable names") {
		t.Fatalf("fact not persisted: %q", data)
	}
}

func TestRunSaveMemoryProjectScope(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cwd := t.TempDir()
	rt := &toolRuntime{cwd: cwd}

	if _, err := rt.runSaveMemory([]byte(`{"fact":"tests live under tests/","scope":"project"}`)); err != nil {
		t.Fatalf("runSaveMemory: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(cwd, ".spettro", "memory.md"))
	if err != nil {
		t.Fatalf("project memory file missing: %v", err)
	}
	if !strings.Contains(string(data), "- tests live under tests/") {
		t.Fatalf("fact not persisted: %q", data)
	}
}

func TestRunSaveMemoryRejectsBadArgs(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	rt := &toolRuntime{cwd: t.TempDir()}
	if _, err := rt.runSaveMemory([]byte(`{"fact":"x","scope":"global"}`)); err == nil {
		t.Fatal("invalid scope accepted")
	}
	if _, err := rt.runSaveMemory([]byte(`{"fact":""}`)); err == nil {
		t.Fatal("empty fact accepted")
	}
	if _, err := rt.runSaveMemory([]byte(`{"fact":"x","bogus":true}`)); err == nil {
		t.Fatal("unknown field accepted")
	}
}

// The saved memory must surface in the agent system context of a later
// session.
func TestMemorySurfacesInNextSessionContext(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	memory.ResetSessionCacheForTesting()
	t.Cleanup(memory.ResetSessionCacheForTesting)

	cwd := t.TempDir()
	rt := &toolRuntime{cwd: cwd}
	if _, err := rt.runSaveMemory([]byte(`{"fact":"answer in bullet points"}`)); err != nil {
		t.Fatal(err)
	}
	// Simulate a fresh session (new process → empty snapshot cache).
	memory.ResetSessionCacheForTesting()
	got := memory.SessionContext(cwd)
	if !strings.Contains(got, "# Memory") || !strings.Contains(got, "- answer in bullet points") {
		t.Fatalf("memory missing from session context: %q", got)
	}
}
