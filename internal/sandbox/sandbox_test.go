package sandbox_test

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"spettro/internal/sandbox"
)

func TestCommandUnwrappedWhenDisabled(t *testing.T) {
	cmd := sandbox.Command(context.Background(), false, t.TempDir(), "bash", "-lc", "true")
	if filepath.Base(cmd.Path) == "sandbox-exec" {
		t.Fatalf("disabled sandbox must not wrap the command: %v", cmd.Args)
	}
}

func TestDarwinSandboxConfinesWrites(t *testing.T) {
	if runtime.GOOS != "darwin" || !sandbox.Available() {
		t.Skip("sandbox-exec confinement only validated on macOS")
	}
	ctx := context.Background()
	ws := t.TempDir()

	// A write inside the workspace is allowed.
	inside := filepath.Join(ws, "inside.txt")
	cmd := sandbox.Command(ctx, true, ws, "bash", "-lc", "echo ok > "+inside)
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
	cmd2 := sandbox.Command(ctx, true, ws, "bash", "-lc", "echo escaped > "+outside)
	_ = cmd2.Run() // expected to fail; we assert on the filesystem effect
	if _, err := os.Stat(outside); err == nil {
		t.Fatalf("write outside workspace should have been blocked, but file exists")
	}
}
