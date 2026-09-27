//go:build !windows

package main

import (
	"os/exec"
	"syscall"
	"testing"
	"time"

	"spettro/internal/jobs"
)

// Every non-TUI mode leaves through exitSession (or a deferred
// releaseSessionResources). A background job the agent started runs in its
// own process group, so only an explicit kill stops it from outliving
// spettro: exitSession must do that before the process exits.
func TestExitSessionKillsBackgroundJobs(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	if _, err := jobs.Default().Start(cmd, "sleep 30"); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid

	saved := osExit
	t.Cleanup(func() { osExit = saved })
	code := -1
	osExit = func(c int) { code = c }

	exitSession(3)
	if code != 3 {
		t.Fatalf("exit code = %d, want 3", code)
	}
	deadline := time.Now().Add(3 * time.Second)
	for syscall.Kill(pid, 0) == nil {
		if time.Now().After(deadline) {
			t.Fatal("the background job survived exitSession")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
