//go:build (!darwin && !linux) || ios

// iOS is included here even though it is a "darwin" GOOS: see sandbox_darwin.go.
// On iOS the whole process is already confined by the app sandbox, so reporting
// "no mechanism" and running unconfined is both accurate and harmless.

package sandbox

import (
	"context"
	"os/exec"
)

// available is false on platforms without a sandbox backend. Command therefore
// runs the program unconfined.
func available() bool { return false }

func capabilities() Capabilities {
	return Capabilities{Mechanism: "none", Detail: "unsupported platform"}
}

func wrap(ctx context.Context, _ Policy, _ string, name string, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, name, args...)
}

// runChildIfRequested is a no-op: only the Linux backend re-execs a child.
func runChildIfRequested() {}

// confineParent is a no-op on platforms without a sandbox backend.
func confineParent(_ []string) error { return nil }
