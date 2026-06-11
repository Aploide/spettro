//go:build linux

package sandbox

import (
	"context"
	"os"
	"os/exec"
	"syscall"

	"github.com/landlock-lsm/go-landlock/landlock"
)

// childSentinel marks a re-executed sandbox child. Command re-execs the spettro
// binary as `<self> <childSentinel> <workspace> -- <cmd> <args...>`; the child's
// RunChildIfRequested applies Landlock and exec()s the real command.
const childSentinel = "__spettro_sandbox_child__"

func available() bool { return true }

func wrap(ctx context.Context, workspaceDir, name string, args ...string) *exec.Cmd {
	self, err := os.Executable()
	if err != nil || self == "" {
		self = "/proc/self/exe"
	}
	full := append([]string{childSentinel, workspaceDir, "--", name}, args...)
	return exec.CommandContext(ctx, self, full...)
}

func runChildIfRequested() {
	if len(os.Args) < 5 || os.Args[1] != childSentinel || os.Args[3] != "--" {
		return
	}
	workspace := os.Args[2]
	cmd := os.Args[4:]

	// Read everything, but write only within the workspace and the usual
	// scratch locations. Landlock denies any filesystem access it handles that
	// is not granted here, so this confines writes at the kernel level.
	rw := []string{workspace, "/tmp", "/dev"}
	if td := os.TempDir(); td != "" {
		rw = append(rw, td)
	}
	// V1 (strict, not best-effort) fails closed if the kernel lacks Landlock,
	// so an opt-in sandbox never silently runs unconfined.
	if err := landlock.V1.RestrictPaths(
		landlock.RODirs("/"),
		landlock.RWDirs(rw...),
	); err != nil {
		os.Stderr.WriteString("spettro sandbox: landlock restrict failed: " + err.Error() + "\n")
		os.Exit(126)
	}

	path, err := exec.LookPath(cmd[0])
	if err != nil {
		os.Stderr.WriteString("spettro sandbox: " + err.Error() + "\n")
		os.Exit(127)
	}
	if err := syscall.Exec(path, cmd, os.Environ()); err != nil {
		os.Stderr.WriteString("spettro sandbox: exec failed: " + err.Error() + "\n")
		os.Exit(126)
	}
}
