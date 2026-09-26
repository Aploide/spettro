//go:build unix

package shell

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
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

// waitForPIDFile polls until path holds a pid.
func waitForPIDFile(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if raw, err := os.ReadFile(path); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(raw))); err == nil {
				return pid
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("no pid written to %s", path)
	return 0
}

// KillAllProcessTrees reaches a command CombinedOutput is still running, and
// the command's output and status come back as for exec's CombinedOutput.
func TestKillAllProcessTreesKillsRunningCommand(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pid")
	cmd := exec.CommandContext(context.Background(), "sh", "-c", "echo started; sleep 30 & echo $! > "+pidFile+"; wait")
	ConfigureProcessTree(cmd)
	type result struct {
		out []byte
		err error
	}
	done := make(chan result, 1)
	go func() {
		out, err := CombinedOutput(cmd)
		done <- result{out, err}
	}()
	pid := waitForPIDFile(t, pidFile)
	KillAllProcessTrees()
	select {
	case res := <-done:
		if res.err == nil {
			t.Fatal("killed command reported success")
		}
		if !strings.Contains(string(res.out), "started") {
			t.Fatalf("output lost: %q", res.out)
		}
	case <-time.After(DefaultWaitDelay + 3*time.Second):
		t.Fatal("CombinedOutput did not return after KillAllProcessTrees")
	}
	if !processGone(pid) {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		t.Fatalf("grandchild %d survived KillAllProcessTrees", pid)
	}
}

// hangupHelperEnv makes the test binary act as a spettro stand-in: it installs
// the hangup handler and runs a long command in its own process group.
const hangupHelperEnv = "SPETTRO_PROCTREE_HANGUP_HELPER"

func TestMain(m *testing.M) {
	if pidFile := os.Getenv(hangupHelperEnv); pidFile != "" {
		KillProcessTreesOnHangup()
		cmd := exec.CommandContext(context.Background(), "sh", "-c", "sleep 30 & echo $! > "+pidFile+"; wait")
		ConfigureProcessTree(cmd)
		_, _ = CombinedOutput(cmd)
		os.Exit(3) // only reached if the hangup did not end the process
	}
	os.Exit(m.Run())
}

// Closing the terminal sends SIGHUP to spettro's process group only. The
// command in its own group must still die, and spettro must still terminate.
func TestHangupKillsCommandTrees(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pid")
	helper := exec.Command(os.Args[0], "-test.run=^$")
	helper.Env = append(os.Environ(), hangupHelperEnv+"="+pidFile)
	if err := helper.Start(); err != nil {
		t.Fatal(err)
	}
	pid := waitForPIDFile(t, pidFile)
	if err := helper.Process.Signal(syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	waited := make(chan error, 1)
	go func() { waited <- helper.Wait() }()
	select {
	case err := <-waited:
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Fatalf("helper exit = %v, want death by SIGHUP", err)
		}
		if ws, ok := exitErr.Sys().(syscall.WaitStatus); !ok || !ws.Signaled() || ws.Signal() != syscall.SIGHUP {
			t.Fatalf("helper exit = %v, want death by SIGHUP", err)
		}
	case <-time.After(10 * time.Second):
		_ = helper.Process.Kill()
		t.Fatal("helper did not exit on SIGHUP")
	}
	if !processGone(pid) {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		t.Fatalf("command %d survived the hangup", pid)
	}
}
