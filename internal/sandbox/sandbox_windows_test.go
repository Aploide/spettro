//go:build windows

package sandbox_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"spettro/internal/sandbox"
	"spettro/internal/shell"
	"spettro/internal/shell/shelltest"
)

// run executes cmdline under policy the way the shell tool does, and reports
// the combined output plus exit status.
func run(t *testing.T, p sandbox.Policy, workspace, cmdline string) (string, int) {
	t.Helper()
	name, args := shell.CommandLine(cmdline)
	cmd := sandbox.Command(context.Background(), p, workspace, name, args...)
	cmd.Dir = workspace
	out, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		if exitErr, ok := err.(interface{ ExitCode() int }); ok {
			code = exitErr.ExitCode()
		} else {
			t.Fatalf("run %q: %v", cmdline, err)
		}
	}
	return string(out), code
}

// writeTo is a command line that creates a file at path.
func writeTo(path string) string {
	if shell.Dialect() == shell.KindPowerShell {
		return "Set-Content -LiteralPath '" + path + "' -Value 'written'"
	}
	return "printf written > '" + path + "'"
}

// The whole point of the backend: a command the model runs must not be able to
// modify files outside the roots the policy allows. This is the test that
// distinguishes a real sandbox from a no-op.
func TestWorkspaceWriteBlocksWritesOutsideWorkspace(t *testing.T) {
	workspace := t.TempDir()
	outside := filepath.Join(t.TempDir(), "escaped.txt")
	policy := sandbox.Policy{FS: sandbox.FSWorkspaceWrite, Net: sandbox.NetAll}

	out, code := run(t, policy, workspace, writeTo(outside))
	if code == 0 {
		t.Errorf("write outside the workspace succeeded (exit 0), output %q", out)
	}
	if _, err := os.Stat(outside); err == nil {
		t.Fatalf("sandbox did not prevent the write: %s exists", outside)
	}
}

// Confinement is worthless if it also blocks the work: the agent has to be
// able to edit the project it was pointed at.
func TestWorkspaceWriteAllowsWritesInsideWorkspace(t *testing.T) {
	workspace := t.TempDir()
	inside := filepath.Join(workspace, "allowed.txt")
	policy := sandbox.Policy{FS: sandbox.FSWorkspaceWrite, Net: sandbox.NetAll}

	out, code := run(t, policy, workspace, writeTo(inside))
	if code != 0 {
		t.Fatalf("write inside the workspace failed (exit %d), output %q", code, out)
	}
	if _, err := os.Stat(inside); err != nil {
		t.Fatalf("file was not created inside the workspace: %v", err)
	}
}

// read-only means the workspace itself is off limits too.
func TestReadOnlyBlocksWritesInsideWorkspace(t *testing.T) {
	workspace := t.TempDir()
	inside := filepath.Join(workspace, "denied.txt")
	policy := sandbox.Policy{FS: sandbox.FSReadOnly, Net: sandbox.NetAll}

	out, code := run(t, policy, workspace, writeTo(inside))
	if code == 0 {
		t.Errorf("write to a read-only workspace succeeded, output %q", out)
	}
	if _, err := os.Stat(inside); err == nil {
		t.Fatalf("sandbox did not prevent the write: %s exists", inside)
	}
}

// Reads must keep working under confinement, or ordinary tooling breaks.
func TestConfinedCommandCanStillRead(t *testing.T) {
	workspace := t.TempDir()
	readable := filepath.Join(workspace, "input.txt")
	if err := os.WriteFile(readable, []byte("hello-from-disk"), 0o644); err != nil {
		t.Fatal(err)
	}
	policy := sandbox.Policy{FS: sandbox.FSReadOnly, Net: sandbox.NetAll}

	out, code := run(t, policy, workspace, "Get-Content -LiteralPath '"+readable+"'")
	if code != 0 {
		t.Fatalf("read failed under confinement (exit %d): %q", code, out)
	}
	if !strings.Contains(out, "hello-from-disk") {
		t.Errorf("output = %q, want the file contents", out)
	}
}

// Temp files are how compilers and package managers work; a sandbox that
// breaks them is unusable regardless of what it protects.
func TestConfinedCommandCanWriteToTemp(t *testing.T) {
	workspace := t.TempDir()
	policy := sandbox.Policy{FS: sandbox.FSReadOnly, Net: sandbox.NetAll}

	// The sandbox temp dir is shared across runs, so name the probe uniquely;
	// a fixed name collides with a file a previous run left behind.
	name := fmt.Sprintf("spettro-probe-%d-%d.txt", os.Getpid(), time.Now().UnixNano())
	out, code := run(t, policy, workspace,
		`$p = Join-Path $env:TEMP '`+name+`'; Set-Content -LiteralPath $p -Value ok; Get-Content -LiteralPath $p; Remove-Item -LiteralPath $p`)
	if code != 0 {
		t.Fatalf("temp write failed under confinement (exit %d): %q", code, out)
	}
	if !strings.Contains(out, "ok") {
		t.Errorf("output = %q, want the temp file contents", out)
	}
}

// An opt-in sandbox must never silently run unconfined. Network confinement
// cannot be enforced on this platform, so requesting it has to fail.
func TestNetworkPolicyFailsClosed(t *testing.T) {
	workspace := t.TempDir()
	for _, net := range []sandbox.NetPolicy{sandbox.NetNone, sandbox.NetLocalhost, sandbox.NetPorts} {
		policy := sandbox.Policy{FS: sandbox.FSWorkspaceWrite, Net: net}
		out, code := run(t, policy, workspace, shelltest.Echo("should not run"))
		if code == 0 {
			t.Errorf("net=%s ran unconfined (exit 0), output %q", net, out)
		}
		if strings.Contains(out, "should not run") {
			t.Errorf("net=%s executed the command anyway: %q", net, out)
		}
	}
}

// A disabled policy must not pay for any of this.
func TestDisabledPolicyRunsUnwrapped(t *testing.T) {
	workspace := t.TempDir()
	outside := filepath.Join(t.TempDir(), "unconfined.txt")

	out, code := run(t, sandbox.Policy{}, workspace, writeTo(outside))
	if code != 0 {
		t.Fatalf("unconfined write failed (exit %d): %q", code, out)
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatalf("unconfined write did not happen: %v", err)
	}
}

func TestPlatformCapabilitiesReportsNoNetworkConfinement(t *testing.T) {
	if !sandbox.Available() {
		t.Fatal("sandbox should be available on Windows")
	}
	caps := sandbox.PlatformCapabilities()
	if !caps.FS {
		t.Error("filesystem confinement should be reported as available")
	}
	if caps.Net {
		t.Error("network confinement is not implemented and must not be advertised")
	}
}
