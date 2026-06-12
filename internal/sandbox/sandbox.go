// Package sandbox provides opt-in, OS-native confinement for the shell
// commands spettro runs on behalf of an LLM agent, as defense-in-depth behind
// the existing approval gates.
//
// Confinement is described by a capability-based Policy: a filesystem mode
// (off, workspace-write, read-only, plus extra writable/readable roots)
// combined with a network mode (all, localhost, allowed TCP ports, none). The
// effective policy is resolved once at startup by ResolvePolicy with precedence
//
//	CLI flags > project manifest > disabled
//
// and is immutable thereafter. It is set by the operator alone: the model
// cannot observe the policy (there is no sandbox tool and no prompt hint) nor
// request changes to it — blocked operations surface as ordinary failures.
//
// Mechanisms by platform:
//   - macOS: sandbox-exec (Seatbelt) with a generated SBPL profile. Filesystem
//     and network filters are enforced in the child directly; network filters
//     are ip/port based (not hostname). "(deny network*)" also covers unix
//     sockets, so under NetNone DNS via mDNSResponder is blocked too.
//   - Linux: Landlock, applied in a re-exec'd child before the command runs
//     (the restriction survives execve). Filesystem rules use the strictest
//     ABI the kernel offers (v1+, kernel 5.13+); network rules need ABI v4
//     (kernel 6.7+) and govern TCP connect/bind by port only — UDP, MPTCP and
//     unix sockets are not covered, and NetLocalhost degrades to deny-all TCP
//     because Landlock cannot scope rules to loopback. Unlike namespace tools
//     (bwrap), Landlock needs no privileges or user namespaces, so it works in
//     locked-down containers.
//   - other platforms: not implemented (Available reports false); commands run
//     unconfined.
//
// Coverage:
//   - Shell commands (Command) are confined at the kernel level.
//   - The in-process file tools (file-write/file-edit) honor the same FS write
//     scope via an application-level check, so read-only cannot be bypassed by
//     writing through a file tool instead of a shell redirect.
//   - The spettro process itself is write-confined as defense-in-depth
//     (ConfineParent): Landlock in-process on Linux, a one-time sandbox-exec
//     re-exec on macOS. Its reads stay open (the in-process git committer and
//     skill discovery legitimately read $HOME); the model's read surface is
//     confined at the shell and file-tool layers instead.
//
// Caveats:
//   - LLM API traffic flows through the never-network-confined parent, so
//     NetNone still lets the agent reach its API server.
//   - Reads are confined to system locations plus the workspace and allowed
//     roots; toolchain caches under $HOME (e.g. ~/go/pkg/mod) are blocked
//     unless granted with an extra readable root.
//   - In FSReadOnly, temp dirs and /dev stay writable so ordinary commands
//     (">/dev/null", compiler scratch files) keep working; the guarantee is
//     "cannot modify project or user files".
//   - Failure is closed: on Linux the child exits 126 if the kernel cannot
//     enforce the requested policy, so an opt-in sandbox never silently runs
//     unconfined.
//
// On Linux the wrapper re-executes the spettro binary itself, which applies
// Landlock to its own process and then exec()s the real command. Programs that
// use this package MUST therefore call RunChildIfRequested() as the very first
// thing in main().
package sandbox

import (
	"context"
	"os"
	"os/exec"
)

// Available reports whether OS-native sandboxing is implemented on this platform.
func Available() bool { return available() }

// Capabilities describes what the platform's sandbox mechanism can enforce.
type Capabilities struct {
	Mechanism string `json:"mechanism"` // "seatbelt", "landlock" or "none"
	FS        bool   `json:"fs"`        // filesystem confinement enforceable
	Net       bool   `json:"net"`       // network confinement enforceable
	Detail    string `json:"detail"`
}

// PlatformCapabilities reports the sandbox capabilities of this host.
func PlatformCapabilities() Capabilities { return capabilities() }

// RunChildIfRequested must be called at the very start of main(). On Linux,
// when the process was re-executed by Command as a sandbox child, it applies
// Landlock confinement and exec()s the real command (never returning). On every
// other platform, and for normal invocations, it is a no-op.
func RunChildIfRequested() { runChildIfRequested() }

// Command builds an exec.Cmd for name+args. When the policy restricts anything
// and the platform supports sandboxing, the command is wrapped so the policy is
// enforced at the kernel level; otherwise it is returned unwrapped.
func Command(ctx context.Context, p Policy, workspaceDir, name string, args ...string) *exec.Cmd {
	if p.Enabled() && available() {
		return wrap(ctx, p, workspaceDir, name, args...)
	}
	return exec.CommandContext(ctx, name, args...)
}

// ConfineParent applies a write-confinement backstop to the spettro process
// itself: the parent (and the in-process file tools it runs) may write only
// under writableRoots plus the system temp dirs; reads and network stay open.
// On Linux it is applied in-process via Landlock; on macOS the process re-execs
// itself under sandbox-exec once (guarded against re-entry). It is a defense-
// in-depth layer — the model's surface is already confined at the shell and
// file-tool layers — so callers should treat a returned error as a warning,
// not fatal. Call only when a sandbox policy is enabled.
func ConfineParent(writableRoots []string) error {
	return confineParent(writableRoots)
}

// parentWritableRoots merges the caller's roots with the system temp dirs,
// cleaned and deduplicated.
func parentWritableRoots(writableRoots []string) []string {
	var out []string
	seen := map[string]struct{}{}
	add := func(d string) {
		if d == "" {
			return
		}
		if _, ok := seen[d]; ok {
			return
		}
		seen[d] = struct{}{}
		out = append(out, d)
	}
	for _, d := range writableRoots {
		add(d)
	}
	add(os.TempDir())
	add("/tmp")
	return out
}
