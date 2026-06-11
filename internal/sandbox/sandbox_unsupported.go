//go:build !darwin

package sandbox

import (
	"context"
	"os/exec"
)

// available is false on platforms without a sandbox backend yet (e.g. Linux,
// where Landlock integration is a future step). Command therefore runs the
// program unconfined.
func available() bool { return false }

func wrap(ctx context.Context, _ string, name string, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, name, args...)
}
