// Package sandbox provides opt-in, OS-native confinement for the shell commands
// spettro runs on behalf of an LLM agent. It is an experimental prototype: the
// goal is to bound filesystem writes to the workspace at the kernel level, as
// defense-in-depth behind the existing approval gates.
//
// Status by platform:
//   - macOS: sandbox-exec (Seatbelt) — writes confined to the workspace and
//     temp dirs; reads, exec and network remain allowed.
//   - Linux: Landlock (kernel >= 5.13) applied in a re-exec'd child before the
//     command runs. Unlike namespace tools (bwrap), Landlock needs no
//     privileges or user namespaces, so it works in locked-down containers.
//   - other: not implemented (Available reports false); commands run unconfined.
//
// Confinement is opt-in: callers pass enabled=true (today gated by the
// SPETTRO_SANDBOX=1 environment variable). When unsupported or disabled, the
// command runs normally, so enabling the flag never breaks unsupported hosts.
//
// On Linux the wrapper re-executes the spettro binary itself, which applies
// Landlock to its own process and then exec()s the real command (the
// restriction is preserved across execve). Programs that use this package MUST
// therefore call RunChildIfRequested() as the very first thing in main().
package sandbox

import (
	"context"
	"os/exec"
)

// Available reports whether OS-native sandboxing is implemented on this platform.
func Available() bool { return available() }

// RunChildIfRequested must be called at the very start of main(). On Linux,
// when the process was re-executed by Command as a sandbox child, it applies
// Landlock confinement and exec()s the real command (never returning). On every
// other platform, and for normal invocations, it is a no-op.
func RunChildIfRequested() { runChildIfRequested() }

// Command builds an exec.Cmd for name+args. When enabled and supported, the
// command is wrapped so filesystem writes are confined to workspaceDir (plus
// temp); otherwise it is returned unwrapped.
func Command(ctx context.Context, enabled bool, workspaceDir, name string, args ...string) *exec.Cmd {
	if enabled && available() {
		return wrap(ctx, workspaceDir, name, args...)
	}
	return exec.CommandContext(ctx, name, args...)
}
