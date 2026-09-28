package testhome

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"spettro/internal/homedir"
)

func TestMain(m *testing.M) { os.Exit(Main(m)) }

// TestHomeIsIsolated is the guard every opted-in package carries; here it
// also checks that homedir resolves the isolated home.
func TestHomeIsIsolated(t *testing.T) {
	Check(t)
	got, err := homedir.Dir()
	if err != nil || !samePath(got, isolated) {
		t.Fatalf("homedir.Dir() = %q (%v), want the isolated home %s", got, err, isolated)
	}
}

// TestIsolateMovesEveryHomeVariable checks that each per-user variable
// points into the temporary home and the toolchain caches do not.
func TestIsolateMovesEveryHomeVariable(t *testing.T) {
	home := os.Getenv("HOME")
	if home == "" || home == startHome {
		t.Fatalf("HOME not moved: %q", home)
	}
	for _, v := range homeVars {
		if got := os.Getenv(v.name); !isWithin(home, got) {
			t.Errorf("%s = %q, not inside the temporary home %q", v.name, got, home)
		}
	}
	if cfg := os.Getenv("XDG_CONFIG_HOME"); cfg != filepath.Join(isolated, ".config") {
		t.Errorf("XDG_CONFIG_HOME = %q, want %s", cfg, filepath.Join(isolated, ".config"))
	}
	if got := os.Getenv("SUDO_USER"); got != "" {
		t.Errorf("SUDO_USER = %q, want it cleared", got)
	}
	for _, v := range []string{"GOCACHE", "GOPATH", "GOPLSCACHE"} {
		if got := os.Getenv(v); got == "" || isWithin(home, got) {
			t.Errorf("%s = %q: the toolchain cache must stay out of the temporary home", v, got)
		}
	}
}

// TestCheckRejectsRealHome checks the guard itself: with HOME put back to
// the real one it must fail.
func TestCheckRejectsRealHome(t *testing.T) {
	if startHome == "" {
		t.Skip("the test process started without a HOME")
	}
	t.Setenv("HOME", startHome)
	fake := &recorder{TB: t}
	func() {
		defer func() { _ = recover() }()
		Check(fake)
	}()
	if !fake.failed {
		t.Fatal("Check accepted the real home")
	}
}

func TestSamePath(t *testing.T) {
	dir := t.TempDir()
	if !samePath(dir, dir+string(os.PathSeparator)) {
		t.Fatal("a directory and its path with a trailing separator differ")
	}
	if samePath(dir, t.TempDir()) {
		t.Fatal("two temporary directories compare equal")
	}
}

// recorder is a testing.TB that records a Fatal and stops the checked
// function with a panic, which the caller recovers, instead of ending the
// test.
type recorder struct {
	testing.TB
	failed bool
}

func (r *recorder) Helper() {}

func (r *recorder) Fatal(args ...any) { r.failed = true; panic("fatal") }

func (r *recorder) Fatalf(format string, args ...any) { r.failed = true; panic("fatal") }

// isWithin reports whether path is dir or below it.
func isWithin(dir, path string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
