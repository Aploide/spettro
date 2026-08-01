package sandbox

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// FSPolicy selects how the filesystem is confined for sandboxed commands.
// The zero value ("") is treated as FSOff.
type FSPolicy string

const (
	// FSOff applies no filesystem confinement.
	FSOff FSPolicy = "off"
	// FSWorkspaceWrite confines writes to the workspace, temp dirs, /dev and
	// any ExtraWritable roots. Reads remain allowed everywhere.
	FSWorkspaceWrite FSPolicy = "workspace-write"
	// FSReadOnly denies writes to the workspace too: only temp dirs, /dev and
	// ExtraWritable roots stay writable. "Read-only" means the command cannot
	// modify project or user files, not that every write syscall fails.
	FSReadOnly FSPolicy = "read-only"
)

// NetPolicy selects how network access is confined for sandboxed commands.
// The zero value ("") is treated as NetAll. The spettro process itself (and
// therefore LLM API traffic) is never sandboxed, so NetNone still lets the
// agent reach its API server.
type NetPolicy string

const (
	// NetAll applies no network confinement.
	NetAll NetPolicy = "all"
	// NetLocalhost allows loopback traffic only. On Linux, Landlock cannot
	// scope rules to loopback, so this degrades to deny-all TCP (fail closed).
	NetLocalhost NetPolicy = "localhost"
	// NetPorts allows TCP traffic on AllowedPorts only (any host; both
	// mechanisms are host-blind at this granularity).
	NetPorts NetPolicy = "ports"
	// NetNone denies all network access the platform mechanism can govern.
	NetNone NetPolicy = "none"
)

// Policy is a capability-based description of the confinement applied to
// commands run through Command. The zero value is fully disabled.
type Policy struct {
	FS           FSPolicy  `json:"fs"`
	Net          NetPolicy `json:"net"`
	AllowedPorts []uint16  `json:"allowed_ports,omitempty"`
	// ExtraWritable roots are writable (and therefore readable). ExtraReadable
	// roots are readable only — used to grant toolchain caches that live
	// outside the workspace (e.g. ~/go/pkg/mod) when reads are confined.
	ExtraWritable []string `json:"extra_writable,omitempty"`
	ExtraReadable []string `json:"extra_readable,omitempty"`
}

// ReadConfined reports whether reads are restricted. Whenever the filesystem
// is confined at all, reads are limited to system locations, the workspace and
// the explicitly allowed roots — the rest of the home tree (other projects,
// ~/.ssh, credentials) is blocked.
func (p Policy) ReadConfined() bool { return p.fsRestricted() }

// ReadableRoots returns the non-system roots a confined command may read: the
// workspace, extra readable roots, and extra writable roots (writable implies
// readable). Platform system directories are added by the backends.
func (p Policy) ReadableRoots(workspace string) []string {
	roots := make([]string, 0, 1+len(p.ExtraReadable)+len(p.ExtraWritable))
	if workspace != "" {
		roots = append(roots, workspace)
	}
	roots = append(roots, p.ExtraReadable...)
	roots = append(roots, p.ExtraWritable...)
	return roots
}

func (p Policy) fsRestricted() bool  { return p.FS != "" && p.FS != FSOff }
func (p Policy) netRestricted() bool { return p.Net != "" && p.Net != NetAll }

// Enabled reports whether the policy restricts anything at all.
func (p Policy) Enabled() bool { return p.fsRestricted() || p.netRestricted() }

// FSEnforced reports whether the filesystem is confined under this policy.
func (p Policy) FSEnforced() bool { return p.fsRestricted() }

// WritablePath reports whether absPath may be written under this policy. It is
// the in-process counterpart to the kernel rules applied to shell children, so
// the file tools enforce exactly the same FS scope: writes are allowed under
// the temp dirs, any extra writable roots, and — only in workspace-write mode —
// the workspace itself. When the FS is unconfined every path is writable.
func (p Policy) WritablePath(absPath, workspace string, tempDirs []string) bool {
	if !p.fsRestricted() {
		return true
	}
	roots := make([]string, 0, len(tempDirs)+len(p.ExtraWritable)+1)
	roots = append(roots, tempDirs...)
	roots = append(roots, p.ExtraWritable...)
	if p.FS == FSWorkspaceWrite {
		roots = append(roots, workspace)
	}
	return pathUnderAny(absPath, roots)
}

// pathUnderAny reports whether absPath equals or is contained by any root.
func pathUnderAny(absPath string, roots []string) bool {
	target := filepath.Clean(absPath)
	for _, root := range roots {
		root = strings.TrimSpace(root)
		if root == "" {
			continue
		}
		root = filepath.Clean(root)
		if target == root {
			return true
		}
		rel, err := filepath.Rel(root, target)
		if err != nil {
			continue
		}
		if rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// Summary renders the policy for prompts, status output and failure hints,
// e.g. "fs=read-only net=ports:443,8080 extra-writable=[/data]".
func (p Policy) Summary() string {
	if !p.Enabled() {
		return "disabled"
	}
	fs := p.FS
	if fs == "" {
		fs = FSOff
	}
	s := "fs=" + string(fs)
	switch {
	case p.Net == NetPorts:
		s += " net=ports:" + portList(p.AllowedPorts)
	case p.Net == "":
		s += " net=" + string(NetAll)
	default:
		s += " net=" + string(p.Net)
	}
	if len(p.ExtraWritable) > 0 {
		s += " extra-writable=[" + strings.Join(p.ExtraWritable, " ") + "]"
	}
	return s
}

// Short renders a compact tag for the TUI header, e.g. "ws", "ro+net:none".
func (p Policy) Short() string {
	if !p.Enabled() {
		return "off"
	}
	var parts []string
	switch p.FS {
	case FSWorkspaceWrite:
		parts = append(parts, "ws")
	case FSReadOnly:
		parts = append(parts, "ro")
	}
	switch p.Net {
	case NetNone:
		parts = append(parts, "net:none")
	case NetLocalhost:
		parts = append(parts, "net:lo")
	case NetPorts:
		parts = append(parts, "net:"+portList(p.AllowedPorts))
	}
	return strings.Join(parts, "+")
}

func portList(ports []uint16) string {
	out := make([]string, len(ports))
	for i, p := range ports {
		out[i] = strconv.Itoa(int(p))
	}
	return strings.Join(out, ",")
}

// ParseMode parses a filesystem sandbox mode. "full-access" is accepted as an
// alias of "off" (it is the canonical disabled value persisted in manifests).
func ParseMode(s string) (FSPolicy, error) {
	switch strings.TrimSpace(strings.ToLower(s)) {
	case "off", "full-access":
		return FSOff, nil
	case "workspace-write":
		return FSWorkspaceWrite, nil
	case "read-only":
		return FSReadOnly, nil
	default:
		return "", fmt.Errorf("invalid sandbox mode %q (want off|read-only|workspace-write)", s)
	}
}

// ParseNetSpec parses a network policy spec: "all", "localhost", "none" or
// "ports:443,8080". Ports are deduplicated and sorted.
func ParseNetSpec(s string) (NetPolicy, []uint16, error) {
	t := strings.TrimSpace(strings.ToLower(s))
	switch t {
	case string(NetAll):
		return NetAll, nil, nil
	case string(NetLocalhost):
		return NetLocalhost, nil, nil
	case string(NetNone):
		return NetNone, nil, nil
	}
	if rest, ok := strings.CutPrefix(t, "ports:"); ok {
		var ports []uint16
		for f := range strings.SplitSeq(rest, ",") {
			f = strings.TrimSpace(f)
			if f == "" {
				continue
			}
			n, err := strconv.ParseUint(f, 10, 16)
			if err != nil || n == 0 {
				return "", nil, fmt.Errorf("invalid port %q in net spec %q", f, s)
			}
			if !slices.Contains(ports, uint16(n)) {
				ports = append(ports, uint16(n))
			}
		}
		if len(ports) == 0 {
			return "", nil, fmt.Errorf("net spec %q lists no ports", s)
		}
		slices.Sort(ports)
		return NetPorts, ports, nil
	}
	return "", nil, fmt.Errorf("invalid net policy %q (want all|localhost|none|ports:443,8080)", s)
}

// Overrides are the CLI-level sandbox settings; empty fields mean unset.
type Overrides struct {
	Mode      string
	Net       string
	AllowDirs []string
	ReadDirs  []string
}

// ManifestPolicy mirrors the project manifest's sandbox settings as plain
// strings so this package does not import internal/config.
type ManifestPolicy struct {
	Mode      string
	Net       string
	AllowDirs []string
	ReadDirs  []string
}

// resolveDirs absolutizes and validates a union of directory paths, deduping.
func resolveDirs(label string, groups ...[]string) ([]string, error) {
	var out []string
	for _, g := range groups {
		for _, d := range g {
			d = strings.TrimSpace(d)
			if d == "" {
				continue
			}
			abs, err := filepath.Abs(d)
			if err != nil {
				return nil, fmt.Errorf("%s %q: %w", label, d, err)
			}
			st, err := os.Stat(abs)
			if err != nil {
				return nil, fmt.Errorf("%s %q: %w", label, d, err)
			}
			if !st.IsDir() {
				return nil, fmt.Errorf("%s %q: not a directory", label, d)
			}
			if !slices.Contains(out, abs) {
				out = append(out, abs)
			}
		}
	}
	return out, nil
}

// ResolvePolicy merges sandbox settings into an effective Policy.
// Precedence: CLI flags > manifest > disabled. An explicit CLI "off" wins
// over the manifest. Extra writable/readable dirs are the union of CLI and
// manifest entries, absolutized and validated to exist.
func ResolvePolicy(o Overrides, m ManifestPolicy) (Policy, error) {
	var p Policy

	switch {
	case strings.TrimSpace(o.Mode) != "":
		fs, err := ParseMode(o.Mode)
		if err != nil {
			return Policy{}, fmt.Errorf("--sandbox: %w", err)
		}
		p.FS = fs
	case strings.TrimSpace(m.Mode) != "":
		fs, err := ParseMode(m.Mode)
		if err != nil {
			return Policy{}, fmt.Errorf("manifest runtime.sandbox_mode: %w", err)
		}
		p.FS = fs
	}
	if p.FS == "" {
		p.FS = FSOff
	}

	netSpec, netSource := "", ""
	if strings.TrimSpace(o.Net) != "" {
		netSpec, netSource = o.Net, "--sandbox-net"
	} else if strings.TrimSpace(m.Net) != "" {
		netSpec, netSource = m.Net, "manifest runtime.sandbox_net"
	}
	if netSpec != "" {
		n, ports, err := ParseNetSpec(netSpec)
		if err != nil {
			return Policy{}, fmt.Errorf("%s: %w", netSource, err)
		}
		p.Net, p.AllowedPorts = n, ports
	} else {
		p.Net = NetAll
	}

	writable, err := resolveDirs("sandbox allow dir", o.AllowDirs, m.AllowDirs)
	if err != nil {
		return Policy{}, err
	}
	p.ExtraWritable = writable

	readable, err := resolveDirs("sandbox allow read dir", o.ReadDirs, m.ReadDirs)
	if err != nil {
		return Policy{}, err
	}
	// A path already writable is implicitly readable; don't list it twice.
	for _, d := range readable {
		if !slices.Contains(p.ExtraWritable, d) {
			p.ExtraReadable = append(p.ExtraReadable, d)
		}
	}
	return p, nil
}
