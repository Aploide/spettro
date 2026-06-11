//go:build !darwin && !linux

package sandbox

import (
	"context"
	"os/exec"
)

// available is false on platforms without a sandbox backend. Command therefore
// runs the program unconfined.
func available() bool { return false }

func wrap(ctx context.Context, _ string, name string, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, name, args...)
}

// runChildIfRequested is a no-op: only the Linux backend re-execs a child.
func runChildIfRequested() {}
