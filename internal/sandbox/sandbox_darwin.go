//go:build darwin

package sandbox

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

// parentSandboxMarker is set in the environment of the re-exec'd, confined
// parent so a second pass does not re-exec again.
const parentSandboxMarker = "SPETTRO_SANDBOX_PARENT"

func available() bool {
	_, err := exec.LookPath("sandbox-exec")
	return err == nil
}

func capabilities() Capabilities {
	if available() {
		return Capabilities{Mechanism: "seatbelt", FS: true, Net: true, Detail: "sandbox-exec (Seatbelt)"}
	}
	return Capabilities{Mechanism: "none", Detail: "sandbox-exec not found in PATH"}
}

// runChildIfRequested is a no-op on macOS: sandbox-exec confines the child
// directly, so there is no re-exec child to intercept.
func runChildIfRequested() {}

// confineParent re-execs the process under sandbox-exec with a write-only
// broker profile (reads/network open), so the parent and its in-process file
// tools cannot write outside the granted roots. macOS has no supported in-
// process self-sandbox API without cgo, so re-exec is the portable mechanism;
// a SPETTRO_SANDBOX_PARENT marker prevents an exec loop. On success this never
// returns.
func confineParent(writableRoots []string) error {
	if os.Getenv(parentSandboxMarker) == "1" {
		return nil // already running confined
	}
	sandboxExec, err := exec.LookPath("sandbox-exec")
	if err != nil {
		return fmt.Errorf("sandbox-exec not found: %w", err)
	}
	self, err := os.Executable()
	if err != nil || self == "" {
		return fmt.Errorf("cannot resolve own executable: %w", err)
	}
	profile := brokerSeatbeltProfile(parentWritableRoots(writableRoots))
	argv := append([]string{"sandbox-exec", "-p", profile, self}, os.Args[1:]...)
	env := append(os.Environ(), parentSandboxMarker+"=1")
	return syscall.Exec(sandboxExec, argv, env)
}

// darwinHomeRoots are the directory trees whose reads are blocked under read
// confinement: user data and secrets live here. System paths stay readable.
var darwinHomeRoots = []string{"/Users", "/home"}

func wrap(ctx context.Context, p Policy, workspaceDir, name string, args ...string) *exec.Cmd {
	profile := seatbeltProfile(p, workspaceDir, []string{
		os.TempDir(),
		"/private/tmp",
		"/private/var/folders",
		"/dev",
	}, darwinHomeRoots)
	full := append([]string{"-p", profile, name}, args...)
	return exec.CommandContext(ctx, "sandbox-exec", full...)
}
