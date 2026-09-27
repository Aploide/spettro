// Package testhome keeps a test binary away from the real user's home
// directory.
//
// Spettro keeps its configuration, API keys, sessions, checkpoint history,
// bot settings and downloaded tools under ~/.spettro, and much of the code
// under test finds that directory through $HOME (see homedir.Dir). A test
// that forgets t.Setenv("HOME", t.TempDir()) therefore reads and rewrites the
// developer's real files: one run rewrote a real config.json's
// last_agent_id and show_side_panel. A package whose tests can reach that
// code calls Main from its TestMain, so every test in the binary starts with
// HOME (and the XDG, Windows profile and sudo variables) pointing at a
// throwaway directory, and adds a test that calls Check:
//
//	func TestMain(m *testing.M) { os.Exit(testhome.Main(m)) }
//
//	func TestHomeIsIsolated(t *testing.T) { testhome.Check(t) }
//
// Tests that need a home of their own still call t.Setenv("HOME",
// t.TempDir()); that only moves them further from the real home. The package
// is imported only from _test.go files, so it never reaches the binary.
package testhome

import (
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"spettro/internal/homedir"
)

// startHome is $HOME as the process started. It is read during package
// initialisation, before any TestMain can change it, and never written again.
var startHome = os.Getenv("HOME")

// isolated is the throwaway home Isolate created; empty until Isolate runs.
// Written once by Isolate before m.Run, read-only afterwards.
var isolated string

// homeVars maps each variable that locates per-user files to where it points
// inside the throwaway home: HOME for homedir.Dir and os.UserHomeDir,
// USERPROFILE for os.UserHomeDir on Windows, the XDG base directories for
// os.UserConfigDir and os.UserCacheDir on Linux, and APPDATA and LOCALAPPDATA
// for their Windows counterparts. They are moved on every platform so a test
// that sets GOOS-specific paths by hand still lands in the throwaway home.
var homeVars = []struct{ name, rel string }{
	{"HOME", "."},
	{"USERPROFILE", "."},
	{"XDG_CONFIG_HOME", ".config"},
	{"XDG_CACHE_HOME", ".cache"},
	{"XDG_DATA_HOME", filepath.Join(".local", "share")},
	{"XDG_STATE_HOME", filepath.Join(".local", "state")},
	{"APPDATA", filepath.Join("AppData", "Roaming")},
	{"LOCALAPPDATA", filepath.Join("AppData", "Local")},
}

// Main isolates the home (see Isolate), runs each setup function with the
// throwaway home, runs the tests and removes the directory. It returns the
// exit code for os.Exit.
func Main(m *testing.M, setup ...func(home string) error) int {
	dir, err := Isolate()
	if err != nil {
		fmt.Fprintln(os.Stderr, "testhome:", err)
		return 1
	}
	defer os.RemoveAll(dir)
	for _, fn := range setup {
		if err := fn(dir); err != nil {
			fmt.Fprintln(os.Stderr, "testhome setup:", err)
			return 1
		}
	}
	return m.Run()
}

// Isolate points every per-user location at a fresh temporary directory for
// the rest of the process and returns that directory; the caller removes it.
//
// The Go toolchain's settings file, build cache and module directories, and
// gopls's cache, default to paths under the home, so they are pinned to where
// they are now first. Otherwise a test that runs the go command or starts
// gopls (the agent's edit tests do) rebuilds the standard library into empty
// caches under the throwaway home: slow, gigabytes of disk, and gopls
// processes left indexing in the background slowed a later CPU-bound agent
// test from 4.4 s to 13 s under -race.
func Isolate() (string, error) {
	pinGoToolDirs()
	dir, err := os.MkdirTemp("", "spettro-test-home-")
	if err != nil {
		return "", err
	}
	// On macOS the temp dir is reached through the /var -> /private/var
	// symlink; use the resolved path so tests comparing paths agree with
	// code that resolves the home's real path.
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	for _, v := range homeVars {
		if err := os.Setenv(v.name, filepath.Join(dir, v.rel)); err != nil {
			_ = os.RemoveAll(dir)
			return "", fmt.Errorf("set %s: %w", v.name, err)
		}
	}
	// Under sudo, key material follows SUDO_USER to that user's real home
	// (see config.secretsHome), so the variable is cleared too.
	if err := os.Unsetenv("SUDO_USER"); err != nil {
		_ = os.RemoveAll(dir)
		return "", fmt.Errorf("unset SUDO_USER: %w", err)
	}
	isolated = dir
	return dir, nil
}

// goToolDirs are the go command settings whose defaults derive from HOME.
var goToolDirs = []string{"GOENV", "GOCACHE", "GOPATH", "GOMODCACHE"}

// pinGoToolDirs fixes the go command's settings file, build cache and module
// directories, and gopls's cache, to what they resolve to now, unless they
// are already set. The go values come from "go env" itself, so a GOPATH or
// GOCACHE set with "go env -w" is honoured; without a go tool on PATH no test
// can run one, and those are left alone.
func pinGoToolDirs() {
	if os.Getenv("GOPLSCACHE") == "" {
		if cache, err := os.UserCacheDir(); err == nil {
			_ = os.Setenv("GOPLSCACHE", filepath.Join(cache, "gopls"))
		}
	}
	goTool, err := exec.LookPath("go")
	if err != nil {
		return
	}
	out, err := exec.Command(goTool, append([]string{"env"}, goToolDirs...)...).Output()
	if err != nil {
		return
	}
	values := strings.Split(strings.TrimRight(string(out), "\r\n"), "\n")
	for i, name := range goToolDirs {
		if i >= len(values) || os.Getenv(name) != "" {
			continue
		}
		if v := strings.TrimSpace(values[i]); v != "" {
			_ = os.Setenv(name, v)
		}
	}
}

// Check fails t unless the process runs with the home Isolate made (or one a
// test set on top of it): Isolate must have run, and neither the home Spettro
// resolves nor, on Windows, USERPROFILE may be the real user's home.
func Check(t testing.TB) {
	t.Helper()
	if isolated == "" {
		t.Fatal("testhome.Main did not run: add func TestMain(m *testing.M) { os.Exit(testhome.Main(m)) }")
	}
	home, err := homedir.Dir()
	if err != nil {
		t.Fatalf("resolve the test home: %v", err)
	}
	for _, real := range realHomes() {
		if samePath(home, real) {
			t.Fatalf("tests run with the real home %s: they could overwrite the user's ~/.spettro", real)
		}
	}
	if runtime.GOOS == "windows" {
		if profile := os.Getenv("USERPROFILE"); profile == "" || !samePath(profile, isolated) {
			t.Fatalf("USERPROFILE = %q, want the isolated home %s", profile, isolated)
		}
	}
}

// realHomes returns the directories that count as the user's real home:
// $HOME as the process started, and the account's home from the user
// database, which does not depend on the environment. A process started with
// HOME already inside another test's throwaway home (a test binary run by a
// test) does not count that home as real.
func realHomes() []string {
	var homes []string
	if startHome != "" && !strings.HasPrefix(filepath.Base(startHome), "spettro-test-home-") {
		homes = append(homes, startHome)
	}
	if u, err := user.Current(); err == nil && u.HomeDir != "" {
		homes = append(homes, u.HomeDir)
	}
	return homes
}

// samePath reports whether a and b name the same directory, following
// symbolic links where they exist (/tmp and /private/tmp on macOS).
func samePath(a, b string) bool {
	if filepath.Clean(a) == filepath.Clean(b) {
		return true
	}
	ra, errA := filepath.EvalSymlinks(a)
	rb, errB := filepath.EvalSymlinks(b)
	return errA == nil && errB == nil && ra == rb
}
