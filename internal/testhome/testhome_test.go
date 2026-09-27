package testhome

import (
	"os"
	"path/filepath"
	"testing"

	"spettro/internal/homedir"
)

func TestMain(m *testing.M) { Main(m) }

// The isolated home is what homedir resolves, and not the real one.
func TestHomeIsIsolated(t *testing.T) {
	Check(t)
	got, err := homedir.Dir()
	if err != nil || !samePath(got, isolated) {
		t.Fatalf("homedir.Dir() = %q (%v), want the isolated home %s", got, err, isolated)
	}
	if cfg := os.Getenv("XDG_CONFIG_HOME"); cfg != filepath.Join(isolated, ".config") {
		t.Fatalf("XDG_CONFIG_HOME = %q, want it under the isolated home", cfg)
	}
}

// Check catches a test that points HOME back at the real home.
func TestCheckRejectsRealHome(t *testing.T) {
	if original == "" {
		t.Skip("the test process started without a HOME")
	}
	t.Setenv("HOME", original)
	fake := &recorder{TB: t}
	func() {
		defer func() { _ = recover() }()
		Check(fake)
	}()
	if !fake.failed {
		t.Fatal("Check accepted the real home")
	}
}

// recorder records a Fatal instead of stopping the test.
type recorder struct {
	testing.TB
	failed bool
}

func (r *recorder) Helper() {}

func (r *recorder) Fatal(args ...any) { r.failed = true; panic("fatal") }

func (r *recorder) Fatalf(format string, args ...any) { r.failed = true; panic("fatal") }
