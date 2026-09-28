//go:build unix

package agent

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"spettro/internal/shell"
)

// A login profile that hangs must not leave anything behind when the probe
// times out: the probe's whole process group is killed, not just the shell.
// The fake shell here stands in for such a profile: it starts a long sleep,
// records its pid, and waits on it.
func TestProbeCommandsKillsProcessGroupOnTimeout(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "child.pid")
	fakeShell := filepath.Join(dir, "fakesh")
	script := "#!/bin/sh\nsleep 30 &\necho $! > '" + pidFile + "'\nwait\n"
	if err := os.WriteFile(fakeShell, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(shell.EnvOverride, fakeShell)
	saved := interpreterProbeTimeout
	interpreterProbeTimeout = 300 * time.Millisecond
	t.Cleanup(func() { interpreterProbeTimeout = saved })

	if got := probeCommands([]string{"sh"}); len(got) != 0 {
		t.Fatalf("probe = %v, want nothing from a timed-out shell", got)
	}
	data, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatalf("pid file = %q", data)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
	t.Fatalf("the profile's child %d outlived the probe timeout", pid)
}
