package testhome

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMain(m *testing.M) { os.Exit(Main(m)) }

// TestHomeIsIsolated is the guard every opted-in package carries.
func TestHomeIsIsolated(t *testing.T) { AssertIsolated(t) }

// TestIsolateMovesEveryHomeVariable checks that each per-user variable
// points into the temporary home and the toolchain caches do not.
func TestIsolateMovesEveryHomeVariable(t *testing.T) {
	home := os.Getenv("HOME")
	if home == "" || home == startHome {
		t.Fatalf("HOME not moved: %q", home)
	}
	for _, v := range homeVars {
		if got := os.Getenv(v); !isWithin(home, got) {
			t.Errorf("%s = %q, not inside the temporary home %q", v, os.Getenv(v), home)
		}
	}
	for _, v := range []string{"GOCACHE", "GOPATH", "GOPLSCACHE"} {
		if got := os.Getenv(v); got == "" || isWithin(home, got) {
			t.Errorf("%s = %q: the toolchain cache must stay out of the temporary home", v, got)
		}
	}
}

// recorder is a testing.TB that records a failure instead of stopping.
type recorder struct {
	testing.TB
	failed bool
}

func (r *recorder) Helper()               {}
func (r *recorder) Fatalf(string, ...any) { r.failed = true }

// TestAssertIsolatedFailsOnRealHome checks the guard itself: with HOME put
// back to the real one it must fail.
func TestAssertIsolatedFailsOnRealHome(t *testing.T) {
	if startHome == "" {
		t.Skip("the process started without HOME")
	}
	t.Setenv("HOME", startHome)
	rec := &recorder{TB: t}
	AssertIsolated(rec)
	if !rec.failed {
		t.Fatal("AssertIsolated passed with HOME set to the real home")
	}
}

// isWithin reports whether path is dir or below it.
func isWithin(dir, path string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
