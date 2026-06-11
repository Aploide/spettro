package agent_test

import (
	"os"
	"path/filepath"
	"testing"

	"spettro/internal/agent"
)

func TestNormalizeCommand(t *testing.T) {
	got := agent.NormalizeCommandForTesting("   git    status   --short  ")
	if got != "git status --short" {
		t.Fatalf("unexpected normalized command: %q", got)
	}
}

func TestIsAlwaysAllowedCommand(t *testing.T) {
	if !agent.IsAlwaysAllowedCommandForTesting("pwd") {
		t.Fatal("expected pwd to be always allowed")
	}
	if !agent.IsAlwaysAllowedCommandForTesting("git diff --staged") {
		t.Fatal("expected git diff prefix to be always allowed")
	}
	if agent.IsAlwaysAllowedCommandForTesting("ls $(rm -rf /)") {
		t.Fatal("expected command substitution to require approval")
	}
	if agent.IsAlwaysAllowedCommandForTesting("cat `rm -rf /`") {
		t.Fatal("expected backticks to require approval")
	}
	if agent.IsAlwaysAllowedCommandForTesting("grep foo file > out.txt") {
		t.Fatal("expected redirection to require approval")
	}
	if agent.IsAlwaysAllowedCommandForTesting("ls & echo hi") {
		t.Fatal("expected backgrounding to require approval")
	}
	if agent.IsAlwaysAllowedCommandForTesting("npm publish") {
		t.Fatal("npm publish should not be always allowed")
	}
}

func TestAllowedCommandSetRoundTrip(t *testing.T) {
	cwd := t.TempDir()
	set := map[string]struct{}{
		agent.NormalizeCommandForTesting("echo  hi"): {},
		"git status": {},
	}
	if err := agent.SaveAllowedCommandSetForTesting(cwd, set); err != nil {
		t.Fatalf("saveAllowedCommandSet: %v", err)
	}

	loaded, err := agent.LoadAllowedCommandSetForTesting(cwd)
	if err != nil {
		t.Fatalf("loadAllowedCommandSet: %v", err)
	}
	if _, ok := loaded["echo hi"]; !ok {
		t.Fatalf("expected normalized command in loaded set: %+v", loaded)
	}
	if _, ok := loaded["git status"]; !ok {
		t.Fatalf("expected git status in loaded set: %+v", loaded)
	}

	path := agent.AllowedCommandsPathForTesting(cwd)
	if filepath.Base(path) != "allowed_commands.json" {
		t.Fatalf("unexpected file path: %q", path)
	}

	// The approved-command list is owner-only, consistent with the other
	// ~/.spettro stores; it must not be world-readable.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat allowed commands: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("allowed_commands.json perm = %o, want 600", perm)
	}
	dirInfo, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatalf("stat .spettro dir: %v", err)
	}
	if perm := dirInfo.Mode().Perm(); perm != 0o700 {
		t.Fatalf(".spettro dir perm = %o, want 700", perm)
	}
}

func TestSplitShellCommandSegments(t *testing.T) {
	parts := agent.SplitShellCommandSegmentsForTesting(`cd src && git status | rg foo; echo "a && b"`)
	if len(parts) != 4 {
		t.Fatalf("expected 4 command segments, got %d: %#v", len(parts), parts)
	}
	want := []string{"cd src", "git status", "rg foo", `echo "a && b"`}
	for i := range want {
		if parts[i] != want[i] {
			t.Fatalf("segment %d mismatch: want %q, got %q", i, want[i], parts[i])
		}
	}
}

func TestSplitShellCommandSegments_RespectsQuotedAndSubshellOperators(t *testing.T) {
	parts := agent.SplitShellCommandSegmentsForTesting("echo \"$(a && b)\" && printf \"x|y\"\ncat file")
	want := []string{`echo "$(a && b)"`, `printf "x|y"`, "cat file"}
	if len(parts) != len(want) {
		t.Fatalf("expected %d segments, got %d: %#v", len(want), len(parts), parts)
	}
	for i := range want {
		if parts[i] != want[i] {
			t.Fatalf("segment %d mismatch: want %q, got %q", i, want[i], parts[i])
		}
	}
}

func TestSplitShellCommandSegments_RespectsBackticks(t *testing.T) {
	parts := agent.SplitShellCommandSegmentsForTesting("echo `a; b` && pwd")
	want := []string{"echo `a; b`", "pwd"}
	if len(parts) != len(want) {
		t.Fatalf("expected %d segments, got %d: %#v", len(want), len(parts), parts)
	}
	for i := range want {
		if parts[i] != want[i] {
			t.Fatalf("segment %d mismatch: want %q, got %q", i, want[i], parts[i])
		}
	}
}

func TestIsBlockedCommand(t *testing.T) {
	if !agent.IsBlockedCommandForTesting("rm -fr /") {
		t.Fatal("expected rm -fr / to be blocked")
	}
	if agent.IsBlockedCommandForTesting("rm -rf /tmp") {
		t.Fatal("expected rm -rf /tmp to be allowed by blocklist")
	}
}

func TestIsBlockedCommand_NoPreserveRoot(t *testing.T) {
	blocked := []string{
		"rm -rf / --no-preserve-root",
		"rm --no-preserve-root -rf /home/user",
		"sudo rm -rf --no-preserve-root .",
	}
	for _, cmd := range blocked {
		if !agent.IsBlockedCommandForTesting(cmd) {
			t.Errorf("expected %q to be blocked (--no-preserve-root)", cmd)
		}
	}
	// A normal recursive delete of a subdirectory is NOT hard-blocked — it
	// still goes through the approval prompt rather than being refused outright.
	for _, cmd := range []string{"rm -rf ./build", "rm -rf dist", "rm -rf node_modules"} {
		if agent.IsBlockedCommandForTesting(cmd) {
			t.Errorf("expected %q to be allowed by the blocklist (approval-gated, not hard-blocked)", cmd)
		}
	}
}
