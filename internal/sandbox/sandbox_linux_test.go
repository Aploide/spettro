//go:build linux

package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestMain lets a re-executed sandbox child apply Landlock and exec the real
// command before the test runner takes over. For a normal `go test` invocation
// (no child sentinel in os.Args) RunChildIfRequested is a no-op.
func TestMain(m *testing.M) {
	RunChildIfRequested()
	os.Exit(m.Run())
}

func TestLandlockConfinesWrites(t *testing.T) {
	if !Available() {
		t.Skip("landlock unavailable")
	}
	ctx := context.Background()
	ws := t.TempDir()

	// A write inside the workspace is allowed.
	inside := filepath.Join(ws, "inside.txt")
	in := Command(ctx, true, ws, "bash", "-lc", "echo ok > "+inside)
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
	out := Command(ctx, true, ws, "bash", "-lc", "echo escaped > "+outside)
	out.Dir = ws
	_ = out.Run() // expected to fail under Landlock
	if _, err := os.Stat(outside); err == nil {
		t.Fatalf("write outside workspace should have been blocked by Landlock")
	}
}
