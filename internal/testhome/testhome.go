// Package testhome keeps test binaries away from the real user's home
// directory. Spettro reads and writes ~/.spettro (config.json, keys, the
// ripgrep download, lsp.json), so a test that forgets to redirect HOME edits
// the developer's live configuration: one run rewrote the real config.json's
// permission, last_agent_id and show_side_panel.
//
// A package opts in with a TestMain that calls Main, and one test that calls
// AssertIsolated so a missing or broken TestMain fails loudly:
//
//	func TestMain(m *testing.M) { os.Exit(testhome.Main(m)) }
//	func TestHomeIsIsolated(t *testing.T) { testhome.AssertIsolated(t) }
//
// It is imported only from _test.go files, so it never reaches the binary.
package testhome

import (
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"

	"spettro/internal/homedir"
)

// startHome is $HOME as the process started, read before any TestMain could
// change it (package initialisation runs first).
var startHome = os.Getenv("HOME")

// homeVars are the variables that locate per-user files: HOME for
// homedir.Dir and os.UserHomeDir, the XDG base directories for
// os.UserConfigDir / os.UserCacheDir on Linux, and their Windows
// counterparts.
var homeVars = []string{
	"HOME", "USERPROFILE",
	"XDG_CONFIG_HOME", "XDG_CACHE_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME",
	"APPDATA", "LOCALAPPDATA",
}

// Main points every per-user location at a fresh temporary directory, runs
// the tests and removes the directory. It returns m.Run's exit code.
func Main(m *testing.M) int {
	dir, err := Isolate()
	if err != nil {
		fmt.Fprintln(os.Stderr, "testhome:", err)
		return 1
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	return code
}

// Isolate sets the variables Main sets and returns the temporary home. The
// Go toolchain's own locations (GOCACHE, GOPATH, GOENV) are pinned to their
// real values first when they are not set explicitly: tests that run `go`
// (directly or through gopls) would otherwise rebuild the standard library
// into an empty cache under the temporary home, which is slow and costs
// gigabytes of disk.
func Isolate() (string, error) {
	pinGoToolchainDirs()
	dir, err := os.MkdirTemp("", "spettro-test-home-")
	if err != nil {
		return "", err
	}
	// On macOS the temp dir is reached through the /var -> /private/var
	// symlink; use the resolved path so tests comparing paths agree.
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	for _, v := range homeVars {
		target := dir
		if v != "HOME" && v != "USERPROFILE" {
			target = filepath.Join(dir, strings.ToLower(v))
		}
		if err := os.Setenv(v, target); err != nil {
			return "", err
		}
	}
	return dir, nil
}

// pinGoToolchainDirs sets GOCACHE, GOPATH and GOENV to the locations the
// real home gives them, unless they are already set.
func pinGoToolchainDirs() {
	if os.Getenv("GOCACHE") == "" {
		if cache, err := os.UserCacheDir(); err == nil {
			_ = os.Setenv("GOCACHE", filepath.Join(cache, "go-build"))
		}
	}
	if os.Getenv("GOENV") == "" {
		if cfg, err := os.UserConfigDir(); err == nil {
			_ = os.Setenv("GOENV", filepath.Join(cfg, "go", "env"))
		}
	}
	if os.Getenv("GOPATH") == "" {
		if home, err := os.UserHomeDir(); err == nil {
			_ = os.Setenv("GOPATH", filepath.Join(home, "go"))
		}
	}
}

// realHomes are the directories that count as the user's real home: $HOME
// as the process started, and the account's home directory from the user
// database (independent of the environment) when it can be read.
func realHomes() []string {
	var homes []string
	if startHome != "" {
		homes = append(homes, startHome)
	}
	if u, err := user.Current(); err == nil && u.HomeDir != "" {
		homes = append(homes, u.HomeDir)
	}
	return homes
}

// AssertIsolated fails the test when the home directory Spettro would use is
// the real user's home, i.e. when the package's TestMain does not call Main.
// A process started with HOME already pointing elsewhere (a CI sandbox)
// passes as long as the account's own home is not in use.
func AssertIsolated(t testing.TB) {
	t.Helper()
	home, err := homedir.Dir()
	if err != nil {
		t.Fatalf("resolve home: %v", err)
	}
	for _, real := range realHomes() {
		if samePath(home, real) && !strings.HasPrefix(filepath.Base(home), "spettro-test-home-") {
			t.Fatalf("tests run with the real home directory %s: call testhome.Main from this package's TestMain", home)
		}
	}
}

func samePath(a, b string) bool {
	if filepath.Clean(a) == filepath.Clean(b) {
		return true
	}
	ra, errA := filepath.EvalSymlinks(a)
	rb, errB := filepath.EvalSymlinks(b)
	return errA == nil && errB == nil && ra == rb
}
