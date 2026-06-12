//go:build linux

package sandbox

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	llsys "github.com/landlock-lsm/go-landlock/landlock/syscall"
)

// TestMain lets a re-executed sandbox child apply Landlock and exec the real
// command before the test runner takes over. For a normal `go test` invocation
// (no child sentinel in os.Args) RunChildIfRequested is a no-op.
func TestMain(m *testing.M) {
	RunChildIfRequested()
	os.Exit(m.Run())
}

func requireLandlock(t *testing.T, minABI int) {
	t.Helper()
	abi, err := llsys.LandlockGetABIVersion()
	if err != nil || abi < minABI {
		t.Skipf("landlock ABI v%d unavailable (have v%d, err=%v)", minABI, abi, err)
	}
}

func TestLandlockConfinesWrites(t *testing.T) {
	requireLandlock(t, 1)
	ctx := context.Background()
	ws := t.TempDir()
	pol := Policy{FS: FSWorkspaceWrite}

	// A write inside the workspace is allowed.
	inside := filepath.Join(ws, "inside.txt")
	in := Command(ctx, pol, ws, "bash", "-lc", "echo ok > "+inside)
	in.Dir = ws
	if err := in.Run(); err != nil {
		t.Fatalf("write inside workspace should succeed: %v", err)
	}
	if _, err := os.Stat(inside); err != nil {
		t.Fatalf("inside file not written: %v", err)
	}

	// A write outside the workspace and outside temp is denied. The package
	// working directory is a safe non-temp target; clean up if confinement
	// somehow fails.
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(wd, "landlock-escape-should-not-exist.txt")
	t.Cleanup(func() { _ = os.Remove(outside) })
	out := Command(ctx, pol, ws, "bash", "-lc", "echo escaped > "+outside)
	out.Dir = ws
	_ = out.Run() // expected to fail under Landlock
	if _, err := os.Stat(outside); err == nil {
		t.Fatalf("write outside workspace should have been blocked by Landlock")
	}
}

// nonTempWorkspace returns a workspace outside /tmp (which stays writable in
// every FS mode and would mask read-only denials).
func nonTempWorkspace(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	ws, err := os.MkdirTemp(wd, "sandbox-ws-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(ws) })
	return ws
}

func TestLandlockReadOnlyDeniesWorkspaceWrite(t *testing.T) {
	requireLandlock(t, 1)
	ctx := context.Background()
	ws := nonTempWorkspace(t)
	pol := Policy{FS: FSReadOnly}

	inside := filepath.Join(ws, "inside.txt")
	in := Command(ctx, pol, ws, "bash", "-lc", "echo ok > "+inside)
	in.Dir = ws
	_ = in.Run() // expected to fail
	if _, err := os.Stat(inside); err == nil {
		t.Fatal("read-only mode must block writes inside the workspace too")
	}

	// Reads and /dev/null writes must keep working.
	ok := Command(ctx, pol, ws, "bash", "-lc", "cat /etc/hostname > /dev/null")
	ok.Dir = ws
	if err := ok.Run(); err != nil {
		t.Fatalf("read + /dev/null write should succeed in read-only mode: %v", err)
	}
}

func TestLandlockExtraWritableDir(t *testing.T) {
	requireLandlock(t, 1)
	ctx := context.Background()
	ws := t.TempDir()
	// The grant must live outside the always-writable temp dirs to prove
	// ExtraWritable itself takes effect.
	extra := nonTempWorkspace(t)
	pol := Policy{FS: FSReadOnly, ExtraWritable: []string{extra}}

	target := filepath.Join(extra, "granted.txt")
	cmd := Command(ctx, pol, ws, "bash", "-lc", "echo ok > "+target)
	cmd.Dir = ws
	if err := cmd.Run(); err != nil {
		t.Fatalf("write into extra writable dir should succeed: %v", err)
	}
}

func TestLandlockReadConfinementBlocksHomeOutsideWorkspace(t *testing.T) {
	requireLandlock(t, 1)
	ctx := context.Background()
	ws := nonTempWorkspace(t)
	secretDir := nonTempWorkspace(t) // sibling under the home tree, not the workspace
	secret := filepath.Join(secretDir, "secret.txt")
	if err := os.WriteFile(secret, []byte("topsecret"), 0o600); err != nil {
		t.Fatal(err)
	}

	pol := Policy{FS: FSWorkspaceWrite} // reads confined

	// System reads stay allowed.
	sys := Command(ctx, pol, ws, "bash", "-lc", "cat /etc/hostname > /dev/null")
	sys.Dir = ws
	if err := sys.Run(); err != nil {
		t.Fatalf("system reads must stay allowed: %v", err)
	}
	// A secret outside the workspace (under the home tree) is unreadable.
	blocked := Command(ctx, pol, ws, "bash", "-lc", "cat "+secret)
	blocked.Dir = ws
	out, _ := blocked.CombinedOutput()
	if strings.Contains(string(out), "topsecret") {
		t.Fatalf("read confinement must block files outside the workspace: %q", out)
	}
	// An explicitly granted read dir is reachable again.
	pol2 := Policy{FS: FSWorkspaceWrite, ExtraReadable: []string{secretDir}}
	okCmd := Command(ctx, pol2, ws, "bash", "-lc", "cat "+secret+" > /dev/null")
	okCmd.Dir = ws
	if err := okCmd.Run(); err != nil {
		t.Fatalf("extra-readable dir should be readable: %v", err)
	}
}

// confineParentHelper runs in a subprocess: it applies the parent broker
// confinement to itself, then verifies a write outside the granted roots is
// denied while a write inside succeeds and reads stay open.
func TestConfineParentHelper(t *testing.T) {
	roots := os.Getenv("SPETTRO_TEST_PARENT_ROOTS")
	if roots == "" {
		t.Skip("subprocess helper")
	}
	if err := confineParent(strings.Split(roots, string(os.PathListSeparator))); err != nil {
		fmt.Fprintln(os.Stderr, "confine:", err)
		os.Exit(3)
	}
	if err := os.WriteFile(os.Getenv("SPETTRO_TEST_DENIED"), []byte("x"), 0o644); err == nil {
		fmt.Fprintln(os.Stderr, "write outside roots should have been denied")
		os.Exit(4)
	}
	if err := os.WriteFile(os.Getenv("SPETTRO_TEST_ALLOWED"), []byte("x"), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "write inside roots failed:", err)
		os.Exit(5)
	}
	if _, err := os.ReadFile("/etc/hostname"); err != nil {
		fmt.Fprintln(os.Stderr, "reads must stay open:", err)
		os.Exit(6)
	}
	os.Exit(0)
}

func TestLandlockConfineParent(t *testing.T) {
	requireLandlock(t, 1)
	allowedDir := nonTempWorkspace(t) // a real granted root, not /tmp
	deniedDir := nonTempWorkspace(t)  // not granted

	cmd := exec.Command(os.Args[0], "-test.run=TestConfineParentHelper")
	cmd.Env = append(os.Environ(),
		"SPETTRO_TEST_PARENT_ROOTS="+allowedDir,
		"SPETTRO_TEST_ALLOWED="+filepath.Join(allowedDir, "ok.txt"),
		"SPETTRO_TEST_DENIED="+filepath.Join(deniedDir, "bad.txt"),
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("parent confinement helper failed: %v\n%s", err, out)
	}
}

// loopbackProbe starts a listener and returns its port plus a bash probe
// using /dev/tcp (no external tool dependency).
func loopbackProbe(t *testing.T) (probe string, cleanup func()) {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	return fmt.Sprintf("exec 3<>/dev/tcp/127.0.0.1/%d", port), func() { _ = ln.Close() }
}

func TestLandlockNetNoneBlocksTCP(t *testing.T) {
	requireLandlock(t, 4)
	ctx := context.Background()
	ws := t.TempDir()
	probe, cleanup := loopbackProbe(t)
	defer cleanup()

	// Positive control: without sandbox the probe must succeed.
	ctrl := Command(ctx, Policy{}, ws, "bash", "-lc", probe)
	ctrl.Dir = ws
	if err := ctrl.Run(); err != nil {
		t.Skipf("loopback probe unavailable on this host: %v", err)
	}
	den := Command(ctx, Policy{Net: NetNone}, ws, "bash", "-lc", probe)
	den.Dir = ws
	if err := den.Run(); err == nil {
		t.Fatal("net=none must block TCP connect")
	}
}

func TestLandlockNetPortsAllowsOnlyListedPort(t *testing.T) {
	requireLandlock(t, 4)
	ctx := context.Background()
	ws := t.TempDir()

	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := uint16(ln.Addr().(*net.TCPAddr).Port)
	probe := fmt.Sprintf("exec 3<>/dev/tcp/127.0.0.1/%d", port)

	ctrl := Command(ctx, Policy{}, ws, "bash", "-lc", probe)
	ctrl.Dir = ws
	if err := ctrl.Run(); err != nil {
		t.Skipf("loopback probe unavailable on this host: %v", err)
	}

	allowed := Command(ctx, Policy{Net: NetPorts, AllowedPorts: []uint16{port}}, ws, "bash", "-lc", probe)
	allowed.Dir = ws
	if err := allowed.Run(); err != nil {
		t.Fatalf("connect to allowed port must succeed: %v", err)
	}

	other := port + 1
	if other == 0 {
		other = 1
	}
	denied := Command(ctx, Policy{Net: NetPorts, AllowedPorts: []uint16{other}}, ws, "bash", "-lc", probe)
	denied.Dir = ws
	if err := denied.Run(); err == nil {
		t.Fatal("connect to non-allowed port must fail")
	}
}
