// Package testhome keeps a test binary away from the real user's home
// directory.
//
// Spettro keeps its configuration, credentials, sessions and checkpoint
// history under ~/.spettro, and much of the code under test resolves that
// path from $HOME (internal/homedir). A test that forgets to point HOME
// somewhere else reads and overwrites the developer's real files: a TUI test
// once rewrote the real ~/.spettro/config.json. So a package whose tests can
// reach that code calls Isolate from TestMain, before any test runs, and
// adds a test that calls Check.
package testhome

import (
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// isolated is the home Isolate created, original the HOME it replaced.
// Both are written once, by Isolate, before any test runs.
var isolated, original string

// Isolate points HOME (USERPROFILE, APPDATA and LOCALAPPDATA too on
// Windows) and the XDG base directories at a fresh temporary directory for
// the rest of the process, so nothing a test runs can resolve the real
// home. The Go toolchain's build and module caches, which live under the
// home by default, and its settings file are pinned to where they were
// first, so a test that runs the go command still uses them instead of
// filling a new cache. It returns
// a function that removes the directory; call it after m.Run.
func Isolate() (cleanup func(), err error) {
	original = os.Getenv("HOME")
	if runtime.GOOS == "windows" && original == "" {
		original = os.Getenv("USERPROFILE")
	}
	if err := pinGoCaches(); err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp("", "spettro-test-home-*")
	if err != nil {
		return nil, fmt.Errorf("testhome: %w", err)
	}
	// Resolve symlinks (macOS's /var -> /private/var) so the path matches
	// what code that resolves the home's real path computes.
	if real, err := filepath.EvalSymlinks(dir); err == nil {
		dir = real
	}
	env := map[string]string{
		"HOME":            dir,
		"XDG_CONFIG_HOME": filepath.Join(dir, ".config"),
		"XDG_CACHE_HOME":  filepath.Join(dir, ".cache"),
		"XDG_DATA_HOME":   filepath.Join(dir, ".local", "share"),
		"XDG_STATE_HOME":  filepath.Join(dir, ".local", "state"),
	}
	if runtime.GOOS == "windows" {
		env["USERPROFILE"] = dir
		env["APPDATA"] = filepath.Join(dir, "AppData", "Roaming")
		env["LOCALAPPDATA"] = filepath.Join(dir, "AppData", "Local")
	}
	for key, value := range env {
		if err := os.Setenv(key, value); err != nil {
			os.RemoveAll(dir)
			return nil, fmt.Errorf("testhome: set %s: %w", key, err)
		}
	}
	isolated = dir
	return func() { os.RemoveAll(dir) }, nil
}

// pinGoCaches sets GOCACHE, GOMODCACHE, GOPATH and GOENV to their current
// defaults when they are unset, before the home they default into changes.
func pinGoCaches() error {
	if os.Getenv("GOENV") == "" {
		if cfg, err := os.UserConfigDir(); err == nil {
			if err := os.Setenv("GOENV", filepath.Join(cfg, "go", "env")); err != nil {
				return err
			}
		}
	}
	if os.Getenv("GOCACHE") == "" {
		if cache, err := os.UserCacheDir(); err == nil {
			if err := os.Setenv("GOCACHE", filepath.Join(cache, "go-build")); err != nil {
				return err
			}
		}
	}
	gopath := os.Getenv("GOPATH")
	if gopath == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil // no home to default into: nothing to pin
		}
		gopath = filepath.Join(home, "go")
		if err := os.Setenv("GOPATH", gopath); err != nil {
			return err
		}
	}
	if os.Getenv("GOMODCACHE") == "" {
		first, _, _ := strings.Cut(gopath, string(os.PathListSeparator))
		return os.Setenv("GOMODCACHE", filepath.Join(first, "pkg", "mod"))
	}
	return nil
}

// Main is TestMain for a package that needs nothing else: it isolates the
// home, runs the tests and exits with their status.
func Main(m *testing.M) {
	cleanup, err := Isolate()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	code := m.Run()
	cleanup()
	os.Exit(code)
}

// Check fails t unless the process's home is the one Isolate made (or one a
// test set on top of it): never unset, never the real user's home.
func Check(t testing.TB) {
	t.Helper()
	if isolated == "" {
		t.Fatal("testhome.Isolate was not called: add testhome.Main (or Isolate) to this package's TestMain")
	}
	home := os.Getenv("HOME")
	if home == "" {
		t.Fatal("HOME is unset")
	}
	real := []string{original}
	if u, err := user.Current(); err == nil {
		real = append(real, u.HomeDir)
	}
	for _, r := range real {
		if r != "" && samePath(home, r) {
			t.Fatalf("HOME is the real home %s: tests would read and write the user's ~/.spettro", r)
		}
	}
	if runtime.GOOS == "windows" {
		if profile := os.Getenv("USERPROFILE"); profile == "" || !samePath(profile, isolated) {
			t.Fatalf("USERPROFILE = %q, want the isolated home %s", profile, isolated)
		}
	}
}

// samePath reports whether a and b name the same directory.
func samePath(a, b string) bool {
	if filepath.Clean(a) == filepath.Clean(b) {
		return true
	}
	ra, errA := filepath.EvalSymlinks(a)
	rb, errB := filepath.EvalSymlinks(b)
	return errA == nil && errB == nil && ra == rb
}
