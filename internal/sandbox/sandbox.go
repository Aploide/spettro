// Package sandbox provides opt-in, OS-native confinement for the shell commands
// spettro runs on behalf of an LLM agent. It is an experimental prototype: the
// goal is to bound filesystem writes to the workspace at the kernel level, as
// defense-in-depth behind the existing approval gates.
//
// Status by platform:
//   - macOS: implemented via sandbox-exec (Seatbelt) — writes confined to the
//     workspace and temp dirs; reads, exec and network remain allowed.
//   - Linux/other: not yet implemented (Available reports false); commands run
//     unconfined. A future revision can add Landlock here.
//
// Confinement is opt-in: callers pass enabled=true (today gated by the
// SPETTRO_SANDBOX=1 environment variable). When unsupported or disabled, the
// command runs normally, so enabling the flag never breaks unsupported hosts.
package sandbox

import (
	"context"
	"os/exec"
)

// Available reports whether OS-native sandboxing is implemented on this platform.
func Available() bool { return available() }

// Command builds an exec.Cmd for name+args. When enabled and supported, the
// command is wrapped so filesystem writes are confined to workspaceDir (plus
// temp); otherwise it is returned unwrapped.
func Command(ctx context.Context, enabled bool, workspaceDir, name string, args ...string) *exec.Cmd {
	if enabled && available() {
		return wrap(ctx, workspaceDir, name, args...)
	}
	return exec.CommandContext(ctx, name, args...)
}
