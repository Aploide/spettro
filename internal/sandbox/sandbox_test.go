package sandbox_test

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"spettro/internal/sandbox"
)

func TestCommandUnwrappedWhenDisabled(t *testing.T) {
	cmd := sandbox.Command(context.Background(), sandbox.Policy{}, t.TempDir(), "bash", "-lc", "true")
	if filepath.Base(cmd.Path) == "sandbox-exec" {
		t.Fatalf("disabled sandbox must not wrap the command: %v", cmd.Args)
	}
}

func requireDarwinSandbox(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "darwin" || !sandbox.Available() {
		t.Skip("sandbox-exec confinement only validated on macOS")
	}
}

func TestDarwinSandboxConfinesWrites(t *testing.T) {
	requireDarwinSandbox(t)
	ctx := context.Background()
	ws := t.TempDir()
	pol := sandbox.Policy{FS: sandbox.FSWorkspaceWrite}

	// A write inside the workspace is allowed.
	inside := filepath.Join(ws, "inside.txt")
	cmd := sandbox.Command(ctx, pol, ws, "bash", "-lc", "echo ok > "+inside)
	if err := cmd.Run(); err != nil {
		t.Fatalf("write inside workspace should succeed: %v", err)
	}
	if _, err := os.Stat(inside); err != nil {
		t.Fatalf("inside file not written: %v", err)
	}

	// A write outside the workspace (and outside temp) is denied. The package
	// directory is a safe non-temp target; clean up in case confinement fails.
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(wd, "sandbox-escape-should-not-exist.txt")
	t.Cleanup(func() { _ = os.Remove(outside) })
	cmd2 := sandbox.Command(ctx, pol, ws, "bash", "-lc", "echo escaped > "+outside)
	_ = cmd2.Run() // expected to fail; we assert on the filesystem effect
	if _, err := os.Stat(outside); err == nil {
		t.Fatalf("write outside workspace should have been blocked, but file exists")
	}
}

// nonTempWorkspace returns a workspace outside the temp dirs that stay
// writable in every FS mode (t.TempDir() lives under /private/var/folders on
// macOS and /tmp on Linux, which would mask read-only denials).
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

func TestDarwinReadOnlyDeniesWorkspaceWrite(t *testing.T) {
	requireDarwinSandbox(t)
	ctx := context.Background()
	ws := nonTempWorkspace(t)
	pol := sandbox.Policy{FS: sandbox.FSReadOnly}

	inside := filepath.Join(ws, "inside.txt")
	cmd := sandbox.Command(ctx, pol, ws, "bash", "-lc", "echo ok > "+inside)
	_ = cmd.Run() // expected to fail
	if _, err := os.Stat(inside); err == nil {
		t.Fatalf("read-only mode must block writes inside the workspace too")
	}

	// Reads still work and /dev/null stays writable, or the mode is unusable.
	if err := sandbox.Command(ctx, pol, ws, "bash", "-lc", "cat /etc/hosts > /dev/null").Run(); err != nil {
		t.Fatalf("read + /dev/null write should succeed in read-only mode: %v", err)
	}
}

func TestDarwinReadConfinementBlocksHomeOutsideWorkspace(t *testing.T) {
	requireDarwinSandbox(t)
	ctx := context.Background()
	ws := nonTempWorkspace(t)
	secretDir := nonTempWorkspace(t) // sibling under the home tree, not the workspace
	secret := filepath.Join(secretDir, "secret.txt")
	if err := os.WriteFile(secret, []byte("topsecret"), 0o600); err != nil {
		t.Fatal(err)
	}
	wsFile := filepath.Join(ws, "in.txt")
	if err := os.WriteFile(wsFile, []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}

	pol := sandbox.Policy{FS: sandbox.FSWorkspaceWrite} // reads confined

	if err := sandbox.Command(ctx, pol, ws, "bash", "-lc", "cat "+wsFile+" > /dev/null").Run(); err != nil {
		t.Fatalf("reading inside the workspace must work: %v", err)
	}
	if err := sandbox.Command(ctx, pol, ws, "bash", "-lc", "cat /etc/hosts > /dev/null").Run(); err != nil {
		t.Fatalf("system reads must stay allowed: %v", err)
	}
	out, _ := sandbox.Command(ctx, pol, ws, "bash", "-lc", "cat "+secret).CombinedOutput()
	if strings.Contains(string(out), "topsecret") {
		t.Fatalf("read confinement must block files outside the workspace/home: got %q", out)
	}
	// An explicitly granted read dir is reachable again.
	pol2 := sandbox.Policy{FS: sandbox.FSWorkspaceWrite, ExtraReadable: []string{secretDir}}
	if err := sandbox.Command(ctx, pol2, ws, "bash", "-lc", "cat "+secret+" > /dev/null").Run(); err != nil {
		t.Fatalf("extra-readable dir should be readable: %v", err)
	}
}

func TestDarwinExtraWritableDir(t *testing.T) {
	requireDarwinSandbox(t)
	ctx := context.Background()
	ws := t.TempDir()
	// The grant must live outside the always-writable temp dirs to prove
	// ExtraWritable itself takes effect.
	extra := nonTempWorkspace(t)
	pol := sandbox.Policy{FS: sandbox.FSReadOnly, ExtraWritable: []string{extra}}

	target := filepath.Join(extra, "granted.txt")
	if err := sandbox.Command(ctx, pol, ws, "bash", "-lc", "echo ok > "+target).Run(); err != nil {
		t.Fatalf("write into extra writable dir should succeed: %v", err)
	}
}

// loopbackTarget starts a listener and returns a bash command probing it.
func loopbackTarget(t *testing.T) (probe string, cleanup func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	return fmt.Sprintf("nc -z 127.0.0.1 %d", port), func() { _ = ln.Close() }
}

func TestDarwinNetNoneBlocksLoopback(t *testing.T) {
	requireDarwinSandbox(t)
	ctx := context.Background()
	ws := t.TempDir()
	probe, cleanup := loopbackTarget(t)
	defer cleanup()

	// Positive control: without sandbox the probe must succeed.
	if err := sandbox.Command(ctx, sandbox.Policy{}, ws, "bash", "-lc", probe).Run(); err != nil {
		t.Skipf("loopback probe unavailable on this host: %v", err)
	}
	if err := sandbox.Command(ctx, sandbox.Policy{Net: sandbox.NetNone}, ws, "bash", "-lc", probe).Run(); err == nil {
		t.Fatal("net=none must block loopback connections")
	}
}

func TestDarwinNetLocalhostAllowsLoopback(t *testing.T) {
	requireDarwinSandbox(t)
	ctx := context.Background()
	ws := t.TempDir()
	probe, cleanup := loopbackTarget(t)
	defer cleanup()

	if err := sandbox.Command(ctx, sandbox.Policy{}, ws, "bash", "-lc", probe).Run(); err != nil {
		t.Skipf("loopback probe unavailable on this host: %v", err)
	}
	if err := sandbox.Command(ctx, sandbox.Policy{Net: sandbox.NetLocalhost}, ws, "bash", "-lc", probe).Run(); err != nil {
		t.Fatalf("net=localhost must allow loopback connections: %v", err)
	}
}

// TestDarwinProfileSmoke proves the kernel accepts every composed profile
// variant — golden tests alone only prove composition.
func TestDarwinProfileSmoke(t *testing.T) {
	requireDarwinSandbox(t)
	ctx := context.Background()
	ws := t.TempDir()
	extra := t.TempDir()
	policies := []sandbox.Policy{
		{FS: sandbox.FSWorkspaceWrite},
		{FS: sandbox.FSReadOnly},
		{FS: sandbox.FSWorkspaceWrite, ExtraWritable: []string{extra}},
		{Net: sandbox.NetNone},
		{Net: sandbox.NetLocalhost},
		{Net: sandbox.NetPorts, AllowedPorts: []uint16{443, 8080}},
		{FS: sandbox.FSReadOnly, Net: sandbox.NetNone},
	}
	for _, pol := range policies {
		if err := sandbox.Command(ctx, pol, ws, "/usr/bin/true").Run(); err != nil {
			t.Errorf("kernel rejected profile for %s: %v", pol.Summary(), err)
		}
	}
}
