//go:build unix

package shell

import (
	"context"
	"errors"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// processGone polls until pid no longer exists (its orphaned parent was killed,
// so init reaps it shortly after the kill lands).
func processGone(pid int) bool {
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

// The hang this guards against: a timed-out command whose grandchild inherited
// the output pipe. Killing only the interpreter left the grandchild holding the
// pipe and CombinedOutput blocked until it exited on its own.
func TestConfigureProcessTreeKillsWholeGroupOnCancel(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", "sleep 30 & echo $!; wait")
	ConfigureProcessTree(cmd)
	start := time.Now()
	out, err := cmd.CombinedOutput()
	if elapsed := time.Since(start); elapsed > DefaultWaitDelay+2*time.Second {
		t.Fatalf("CombinedOutput blocked for %s after the deadline", elapsed)
	}
	if err == nil {
		t.Fatal("cancelled command reported success")
	}
	pid, convErr := strconv.Atoi(strings.TrimSpace(string(out)))
	if convErr != nil {
		t.Fatalf("partial output lost: %q", out)
	}
	if !processGone(pid) {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		t.Fatalf("grandchild %d survived the group kill", pid)
	}
}

// A command that exits normally while a background child still holds its
// output must not block Wait: WaitDelay caps the wait and the exit status is
// still success.
func TestConfigureProcessTreeBoundsLingeringPipeHolders(t *testing.T) {
	cmd := exec.CommandContext(context.Background(), "sh", "-c", "echo done; sleep 30 &")
	cmd.WaitDelay = 200 * time.Millisecond
	ConfigureProcessTree(cmd)
	if cmd.WaitDelay != 200*time.Millisecond {
		t.Fatalf("explicit WaitDelay overridden: %s", cmd.WaitDelay)
	}
	start := time.Now()
	out, err := cmd.CombinedOutput()
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("CombinedOutput blocked for %s", elapsed)
	}
	if err != nil && !errors.Is(err, exec.ErrWaitDelay) {
		t.Fatalf("unexpected error: %v", err)
	}
	if cmd.ProcessState == nil || !cmd.ProcessState.Success() {
		t.Fatalf("exit status lost: %v", cmd.ProcessState)
	}
	if !strings.Contains(string(out), "done") {
		t.Fatalf("output lost: %q", out)
	}
	_ = KillProcessTree(cmd)
}

func TestKillProcessTreeNotStarted(t *testing.T) {
	if err := KillProcessTree(exec.Command("true")); err != nil {
		t.Fatalf("kill of unstarted command: %v", err)
	}
	if err := KillProcessTree(nil); err != nil {
		t.Fatalf("kill of nil command: %v", err)
	}
}
