//go:build linux

package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"

	"github.com/landlock-lsm/go-landlock/landlock"
	llsys "github.com/landlock-lsm/go-landlock/landlock/syscall"
)

// childSentinel marks a re-executed sandbox child. Command re-execs the spettro
// binary as `<self> <childSentinel> <childSpecJSON> -- <cmd> <args...>`; the
// child's RunChildIfRequested applies Landlock and exec()s the real command.
const childSentinel = "__spettro_sandbox_child__"

// childSpec carries the policy from the parent to the re-exec'd child in a
// single argv slot: argv passes verbatim through exec (no shell quoting) and,
// unlike an env var, never leaks into the confined command's environment.
type childSpec struct {
	V         int    `json:"v"`
	Workspace string `json:"workspace"`
	Policy    Policy `json:"policy"`
}

// linuxSystemReadDirs are the OS directory trees a confined command may read so
// that programs, libraries, certificates and DNS config keep working. The home
// tree is deliberately excluded: it is granted per-policy (workspace + allowed
// roots) so other projects and credentials stay unreadable. Missing entries are
// ignored at restrict time.
var linuxSystemReadDirs = []string{
	"/usr", "/bin", "/sbin", "/lib", "/lib32", "/lib64", "/libx32",
	"/etc", "/opt", "/proc", "/sys", "/dev", "/run", "/var",
}

func available() bool { return true }

func capabilities() Capabilities {
	abi, err := llsys.LandlockGetABIVersion()
	if err != nil || abi < 1 {
		return Capabilities{Mechanism: "landlock", Detail: "Landlock unavailable (kernel < 5.13 or disabled); sandboxed commands fail closed"}
	}
	c := Capabilities{Mechanism: "landlock", FS: true, Net: abi >= 4, Detail: fmt.Sprintf("Landlock ABI v%d", abi)}
	if !c.Net {
		c.Detail += " (network confinement needs ABI v4, Linux 6.7+)"
	}
	return c
}

func wrap(ctx context.Context, p Policy, workspaceDir, name string, args ...string) *exec.Cmd {
	self, err := os.Executable()
	if err != nil || self == "" {
		self = "/proc/self/exe"
	}
	spec, _ := json.Marshal(childSpec{V: 1, Workspace: workspaceDir, Policy: p})
	full := append([]string{childSentinel, string(spec), "--", name}, args...)
	return exec.CommandContext(ctx, self, full...)
}

// confineParent applies a write-confinement Landlock layer to the current
// process: reads stay open, writes are limited to the given roots. Children
// re-exec'd later add their own stricter layer on top (Landlock intersects).
func confineParent(writableRoots []string) error {
	abi, err := llsys.LandlockGetABIVersion()
	if err != nil || abi < 1 {
		return errors.New("kernel lacks Landlock (Linux 5.13+) for parent confinement")
	}
	cfg, _ := fsConfigForABI(abi)
	return cfg.RestrictPaths(
		landlock.RODirs("/"),
		landlock.RWDirs(parentWritableRoots(writableRoots)...).IgnoreIfMissing(),
	)
}

func runChildIfRequested() {
	if len(os.Args) < 5 || os.Args[1] != childSentinel || os.Args[3] != "--" {
		return
	}
	var spec childSpec
	if err := json.Unmarshal([]byte(os.Args[2]), &spec); err != nil || spec.V != 1 {
		os.Stderr.WriteString("spettro sandbox: invalid child spec\n")
		os.Exit(126)
	}
	cmd := os.Args[4:]
	p := spec.Policy

	abi, err := llsys.LandlockGetABIVersion()
	if err != nil {
		abi = 0
	}

	// Filesystem: system paths stay readable so programs load, but the home
	// tree (other projects, ~/.ssh, ~/.spettro keys) is blocked except the
	// workspace and any explicitly allowed roots. Writes go only to the granted
	// roots. The strictest config the kernel offers is used (never best-effort):
	// V3 also governs truncate, which V1 cannot. Any failure exits 126 so an
	// opt-in sandbox never silently runs unconfined.
	if p.fsRestricted() {
		cfg, ok := fsConfigForABI(abi)
		if !ok {
			os.Stderr.WriteString("spettro sandbox: kernel lacks Landlock (Linux 5.13+ required for filesystem sandboxing)\n")
			os.Exit(126)
		}
		rw := []string{"/tmp", "/dev"}
		if td := os.TempDir(); td != "" {
			rw = append(rw, td)
		}
		if p.FS == FSWorkspaceWrite && spec.Workspace != "" {
			rw = append(rw, spec.Workspace)
		}
		rw = append(rw, p.ExtraWritable...)
		// Readable: the system allowlist (filtered to existing paths) plus the
		// workspace and extra readable/writable roots. Anything not granted —
		// notably the rest of the home directory — is denied.
		ro := append([]string{}, linuxSystemReadDirs...)
		ro = append(ro, p.ReadableRoots(spec.Workspace)...)
		if err := cfg.RestrictPaths(
			landlock.RODirs(ro...).IgnoreIfMissing(),
			landlock.RWDirs(rw...).IgnoreIfMissing(),
		); err != nil {
			os.Stderr.WriteString("spettro sandbox: landlock restrict failed: " + err.Error() + "\n")
			os.Exit(126)
		}
	}

	// Network: Landlock governs TCP connect/bind by port (ABI v4+). NetNone
	// and NetLocalhost both apply zero rules — deny all TCP — because Landlock
	// cannot scope rules to loopback. UDP, MPTCP and unix sockets are not
	// covered by the kernel API.
	if p.netRestricted() {
		if abi < 4 {
			os.Stderr.WriteString("spettro sandbox: kernel lacks Landlock ABI v4 (Linux 6.7+) required for network sandboxing\n")
			os.Exit(126)
		}
		var rules []landlock.Rule
		if p.Net == NetPorts {
			for _, port := range p.AllowedPorts {
				rules = append(rules, landlock.ConnectTCP(port), landlock.BindTCP(port))
			}
		}
		if err := landlock.V4.RestrictNet(rules...); err != nil {
			os.Stderr.WriteString("spettro sandbox: landlock net restrict failed: " + err.Error() + "\n")
			os.Exit(126)
		}
	}

	path, err := exec.LookPath(cmd[0])
	if err != nil {
		os.Stderr.WriteString("spettro sandbox: " + err.Error() + "\n")
		os.Exit(127)
	}
	if err := syscall.Exec(path, cmd, os.Environ()); err != nil {
		os.Stderr.WriteString("spettro sandbox: exec failed: " + err.Error() + "\n")
		os.Exit(126)
	}
}

func fsConfigForABI(abi int) (landlock.Config, bool) {
	switch {
	case abi >= 3:
		return landlock.V3, true
	case abi == 2:
		return landlock.V2, true
	case abi == 1:
		return landlock.V1, true
	default:
		return landlock.Config{}, false
	}
}
