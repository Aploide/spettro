//go:build darwin

package sandbox

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

func available() bool {
	_, err := exec.LookPath("sandbox-exec")
	return err == nil
}

// runChildIfRequested is a no-op on macOS: sandbox-exec confines the child
// directly, so there is no re-exec child to intercept.
func runChildIfRequested() {}

func wrap(ctx context.Context, workspaceDir, name string, args ...string) *exec.Cmd {
	profile := seatbeltProfile(workspaceDir)
	full := append([]string{"-p", profile, name}, args...)
	return exec.CommandContext(ctx, "sandbox-exec", full...)
}

// seatbeltProfile allows everything by default, then denies all filesystem
// writes except under the workspace, the system temp dirs, and /dev. Reads,
// process execution and network remain allowed — the goal is to stop the agent
// from modifying files outside the project, not to fully isolate it.
func seatbeltProfile(workspaceDir string) string {
	writable := []string{
		workspaceDir,
		os.TempDir(),
		"/private/tmp",
		"/private/var/folders",
		"/dev",
	}
	var b strings.Builder
	b.WriteString("(version 1)\n")
	b.WriteString("(allow default)\n")
	b.WriteString("(deny file-write*)\n")
	for _, p := range writable {
		if strings.TrimSpace(p) == "" {
			continue
		}
		fmt.Fprintf(&b, "(allow file-write* (subpath %q))\n", p)
	}
	return b.String()
}
