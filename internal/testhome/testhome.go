// Package testhome keeps test processes away from the real user's home.
//
// Spettro stores its config, keys, sessions and bot settings under
// ~/.spettro (see homedir.Dir), and many code paths a test exercises save
// there: toggling the side panel or the permission mode, switching agents,
// pairing a Telegram bot. A test binary that runs with the developer's HOME
// therefore rewrites their real settings. A package whose tests can reach
// such a path calls Main from its TestMain, which points HOME (and the XDG
// base directories) at a fresh temporary directory for the whole binary,
// and adds a test calling Check, which fails if that isolation is missing.
//
// Tests that need a home of their own still call t.Setenv("HOME",
// t.TempDir()); that only narrows the isolation further.
package testhome

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"spettro/internal/homedir"
)

// realHome is the home the test process started with, recorded by Main
// before it is replaced. Empty until Main runs. Written once by Main before
// any test starts, read-only afterwards.
var realHome string

// homeVars are the variables that decide where a process looks for its
// home or per-user directories: HOME everywhere (homedir.Dir honours it on
// Windows too), USERPROFILE for os.UserHomeDir on Windows, and the XDG base
// directories for libraries that follow them.
var homeVars = []string{
	"HOME", "USERPROFILE",
	"XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_CACHE_HOME", "XDG_STATE_HOME",
}

// Main runs the package's tests with HOME and the XDG base directories set
// to a new temporary directory, removes it afterwards and returns m.Run's
// exit code. Call it from TestMain:
//
//	func TestMain(m *testing.M) { os.Exit(testhome.Main(m)) }
//
// The Go build and module caches are pinned to their current locations
// first, so a test that runs the go command does not rebuild everything
// into the temporary home.
func Main(m *testing.M) int {
	home, err := homedir.Dir()
	if err != nil {
		fmt.Fprintln(os.Stderr, "testhome: resolve the real home:", err)
		return 1
	}
	realHome = home
	dir, err := os.MkdirTemp("", "spettro-test-home-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "testhome: create the test home:", err)
		return 1
	}
	defer os.RemoveAll(dir)
	pinGoCaches(home)
	for _, name := range homeVars {
		value := dir
		if name != "HOME" && name != "USERPROFILE" {
			value = filepath.Join(dir, "xdg", name)
		}
		if err := os.Setenv(name, value); err != nil {
			fmt.Fprintln(os.Stderr, "testhome: set", name+":", err)
			return 1
		}
	}
	return m.Run()
}

// pinGoCaches sets GOENV, GOCACHE, GOMODCACHE and GOPATH to where the go
// command finds them under the real home, unless they are already set:
// their defaults are derived from HOME, which Main is about to replace.
func pinGoCaches(home string) {
	if os.Getenv("GOENV") == "" {
		if dir, err := os.UserConfigDir(); err == nil {
			_ = os.Setenv("GOENV", filepath.Join(dir, "go", "env"))
		}
	}
	if os.Getenv("GOCACHE") == "" {
		if dir, err := os.UserCacheDir(); err == nil {
			_ = os.Setenv("GOCACHE", filepath.Join(dir, "go-build"))
		}
	}
	if os.Getenv("GOPATH") == "" {
		_ = os.Setenv("GOPATH", filepath.Join(home, "go"))
	}
	if os.Getenv("GOMODCACHE") == "" {
		_ = os.Setenv("GOMODCACHE", filepath.Join(os.Getenv("GOPATH"), "pkg", "mod"))
	}
}

// Check fails t unless the test process runs with an isolated home: Main
// must have run, and the home the process now resolves must not be the
// one it started with.
func Check(t testing.TB) {
	t.Helper()
	if realHome == "" {
		t.Fatal("the package's TestMain does not call testhome.Main: its tests would run in the real home")
	}
	current, err := homedir.Dir()
	if err != nil {
		t.Fatalf("resolve the test home: %v", err)
	}
	if sameDir(current, realHome) {
		t.Fatalf("tests run with the real home %s: they could overwrite the user's ~/.spettro", realHome)
	}
}

// sameDir reports whether a and b name the same directory, following
// symbolic links where they exist (/tmp and /private/tmp on macOS).
func sameDir(a, b string) bool {
	if ra, err := filepath.EvalSymlinks(a); err == nil {
		a = ra
	}
	if rb, err := filepath.EvalSymlinks(b); err == nil {
		b = rb
	}
	return filepath.Clean(a) == filepath.Clean(b)
}
