package sandbox

import (
	"fmt"
	"strings"
)

// brokerSeatbeltProfile is the profile the spettro process applies to itself
// when the sandbox is enabled. It is a pure write-confinement backstop: writes
// are limited to the given roots (config, workspace, temp) while reads and
// network stay open, because the parent legitimately reads dotfiles (git
// config), discovers skills across the home tree, and talks to the LLM API.
// The model's own read/network surface is confined separately at the shell and
// file-tool layers.
func brokerSeatbeltProfile(writableRoots []string) string {
	var b strings.Builder
	b.WriteString("(version 1)\n")
	b.WriteString("(allow default)\n")
	b.WriteString("(deny file-write*)\n")
	for _, path := range writableRoots {
		if strings.TrimSpace(path) == "" {
			continue
		}
		fmt.Fprintf(&b, "(allow file-write* (subpath %q))\n", path)
	}
	return b.String()
}

// seatbeltProfile composes a Seatbelt (SBPL) profile for the given policy.
// It is pure — tempDirs and homeRoots are injected by the darwin backend — so
// profile composition is golden-testable on every GOOS. SBPL applies later
// rules over earlier ones, so each section starts from a broad deny and carves
// out allowances.
func seatbeltProfile(p Policy, workspaceDir string, tempDirs, homeRoots []string) string {
	var b strings.Builder
	b.WriteString("(version 1)\n")
	b.WriteString("(allow default)\n")

	if p.fsRestricted() {
		b.WriteString("(deny file-write*)\n")
		var writable []string
		if p.FS == FSWorkspaceWrite {
			writable = append(writable, workspaceDir)
		}
		writable = append(writable, tempDirs...)
		writable = append(writable, p.ExtraWritable...)
		for _, path := range writable {
			if strings.TrimSpace(path) == "" {
				continue
			}
			fmt.Fprintf(&b, "(allow file-write* (subpath %q))\n", path)
		}
	}

	if p.ReadConfined() {
		// System paths stay readable so programs load; the home tree is blocked
		// (secrets, other projects, ~/.spettro keys) except the workspace and
		// any explicitly allowed roots.
		for _, root := range homeRoots {
			if strings.TrimSpace(root) == "" {
				continue
			}
			fmt.Fprintf(&b, "(deny file-read* (subpath %q))\n", root)
		}
		for _, path := range p.ReadableRoots(workspaceDir) {
			if strings.TrimSpace(path) == "" {
				continue
			}
			fmt.Fprintf(&b, "(allow file-read* (subpath %q))\n", path)
		}
	}

	switch p.Net {
	case NetNone:
		// No unix-socket exception: that would re-open DNS via mDNSResponder
		// and local daemon sockets (e.g. docker.sock).
		b.WriteString("(deny network*)\n")
	case NetLocalhost:
		b.WriteString("(deny network*)\n")
		b.WriteString("(allow network* (local ip \"localhost:*\"))\n")
		b.WriteString("(allow network* (remote ip \"localhost:*\"))\n")
		b.WriteString("(allow network-outbound (literal \"/private/var/run/mDNSResponder\"))\n")
	case NetPorts:
		b.WriteString("(deny network*)\n")
		for _, port := range p.AllowedPorts {
			fmt.Fprintf(&b, "(allow network-outbound (remote ip \"*:%d\"))\n", port)
			fmt.Fprintf(&b, "(allow network-bind (local ip \"*:%d\"))\n", port)
			fmt.Fprintf(&b, "(allow network-inbound (local ip \"*:%d\"))\n", port)
		}
		// DNS resolves through mDNSResponder's unix socket on macOS; without
		// it a port allowance would be unusable by hostname.
		b.WriteString("(allow network-outbound (literal \"/private/var/run/mDNSResponder\"))\n")
	}
	return b.String()
}
