package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"spettro/internal/config"
	"spettro/internal/sandbox"
)

func TestSandboxStateNilIsDisabled(t *testing.T) {
	var s *SandboxState
	if s.Policy().Enabled() {
		t.Fatal("nil sandbox state must report a disabled policy")
	}
	r := &toolRuntime{}
	if r.sandboxPolicy().Enabled() {
		t.Fatal("runtime without sandbox state must be unconfined")
	}
}

func TestSandboxStateCarriesPolicy(t *testing.T) {
	p := sandbox.Policy{FS: sandbox.FSReadOnly, Net: sandbox.NetNone}
	r := &toolRuntime{sandboxState: NewSandboxState(p)}
	got := r.sandboxPolicy()
	if got.FS != sandbox.FSReadOnly || got.Net != sandbox.NetNone {
		t.Fatalf("policy not threaded through: %+v", got)
	}
}

// TestFileWriteHonorsReadOnlySandbox proves the in-process file tools cannot
// bypass a read-only sandbox the way the original implementation could (it only
// gated on approval, never on the FS policy).
func TestFileWriteHonorsReadOnlySandbox(t *testing.T) {
	ctx := context.Background()
	// A synthetic workspace outside $TMPDIR; authorizeWriteAccess never touches
	// the filesystem, and a real temp dir would be writable in every mode.
	ws := filepath.Clean("/work/repo")

	// Read-only: a workspace write through file-write must be denied, even in
	// YOLO mode — the sandbox is an operator setting, not a permission prompt.
	ro := &toolRuntime{
		cwd:          ws,
		permission:   config.PermissionYOLO,
		sandboxState: NewSandboxState(sandbox.Policy{FS: sandbox.FSReadOnly}),
		toolPolicies: map[string]config.ToolSpec{"file-write": {RequiresApproval: true}},
	}
	if err := ro.authorizeWriteAccess(ctx, "file-write", "src/main.go"); err == nil {
		t.Fatal("read-only sandbox must deny workspace writes via file-write, even under YOLO")
	}

	// workspace-write: the same write is allowed.
	ws2 := &toolRuntime{
		cwd:          ws,
		permission:   config.PermissionYOLO,
		sandboxState: NewSandboxState(sandbox.Policy{FS: sandbox.FSWorkspaceWrite}),
		toolPolicies: map[string]config.ToolSpec{"file-write": {RequiresApproval: true}},
	}
	if err := ws2.authorizeWriteAccess(ctx, "file-write", "src/main.go"); err != nil {
		t.Fatalf("workspace-write sandbox must allow workspace writes: %v", err)
	}

	// No sandbox: unchanged behavior.
	off := &toolRuntime{cwd: ws, permission: config.PermissionYOLO, toolPolicies: map[string]config.ToolSpec{}}
	if err := off.authorizeWriteAccess(ctx, "file-write", "src/main.go"); err != nil {
		t.Fatalf("no sandbox must not block writes: %v", err)
	}
}

// TestResolvePathBlocksSymlinkEscapeUnderSandbox proves an agent cannot read a
// secret outside the workspace by symlinking it in and reading via the
// in-process file tools (which run in the read-open parent).
func TestResolvePathBlocksSymlinkEscapeUnderSandbox(t *testing.T) {
	ws := t.TempDir()
	outside := t.TempDir()
	secret := filepath.Join(outside, "id_rsa")
	if err := os.WriteFile(secret, []byte("PRIVATE"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(ws, "link")); err != nil {
		t.Fatal(err)
	}

	// With the sandbox active, resolving the symlinked path is rejected.
	confined := &toolRuntime{cwd: ws, sandboxState: NewSandboxState(sandbox.Policy{FS: sandbox.FSWorkspaceWrite})}
	if _, _, err := confined.resolvePath("link"); err == nil {
		t.Fatal("sandbox must reject a workspace symlink pointing outside the workspace")
	}
	// A real file inside the workspace still resolves.
	if err := os.WriteFile(filepath.Join(ws, "real.txt"), []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := confined.resolvePath("real.txt"); err != nil {
		t.Fatalf("in-workspace file must resolve under sandbox: %v", err)
	}

	// Without a sandbox, behavior is unchanged (symlink followed as before).
	open := &toolRuntime{cwd: ws}
	if _, _, err := open.resolvePath("link"); err != nil {
		t.Fatalf("no sandbox must not change symlink behavior: %v", err)
	}
}

// TestRunShellToolEnforcesSandboxEndToEnd drives the real chain
// runShellTool -> sandbox.Command -> kernel and checks the denial. The failure
// is opaque to the model: a generic command error, no sandbox hint.
func TestRunShellToolEnforcesSandboxEndToEnd(t *testing.T) {
	if runtime.GOOS != "darwin" || !sandbox.Available() {
		t.Skip("kernel enforcement validated on macOS hosts")
	}
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	ws, err := os.MkdirTemp(wd, "sandbox-e2e-") // outside temp so read-only denies it
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(ws) })

	r := &toolRuntime{
		cwd:          ws,
		permission:   config.PermissionYOLO, // bypass approval; we test confinement
		sandboxState: NewSandboxState(sandbox.Policy{FS: sandbox.FSReadOnly}),
		allowedShell: map[string]struct{}{},
	}
	args, _ := json.Marshal(map[string]string{"command": "echo blocked > inside.txt"})
	out, err := r.runShellTool(context.Background(), "shell-exec", args, "shell-exec")
	if err == nil {
		t.Fatal("write inside workspace must fail under read-only sandbox")
	}
	if _, statErr := os.Stat(ws + "/inside.txt"); statErr == nil {
		t.Fatal("kernel did not block the write")
	}
	if strings.Contains(out, "[sandbox]") {
		t.Fatalf("failure output must not reveal the sandbox to the model: %q", out)
	}

	args, _ = json.Marshal(map[string]string{"command": "cat /etc/hosts > /dev/null && echo ok"})
	out, err = r.runShellTool(context.Background(), "shell-exec", args, "shell-exec")
	if err != nil || !strings.Contains(out, "ok") {
		t.Fatalf("read-only command should still succeed: %v / %q", err, out)
	}
}
