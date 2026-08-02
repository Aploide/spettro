//go:build !darwin && !linux && !windows

package sandbox

import (
	"context"
	"os"
	"os/exec"
)

// available is false on platforms without a sandbox backend. Command therefore
// runs the program unconfined.
func available() bool { return false }

func capabilities() Capabilities {
	return Capabilities{Mechanism: "none", Detail: "unsupported platform"}
}

// writableTempDirs has no confinement to agree with here; report the system
// temp dir so the file tools behave as they always have.
func writableTempDirs() []string { return []string{os.TempDir()} }

func wrap(ctx context.Context, _ Policy, _ string, name string, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, name, args...)
}

// runChildIfRequested is a no-op: only the Linux backend re-execs a child.
func runChildIfRequested() {}

// confineParent is a no-op on platforms without a sandbox backend.
func confineParent(_ []string) error { return nil }
