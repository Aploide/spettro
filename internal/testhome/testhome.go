// Package testhome keeps a package's tests away from the real home
// directory.
//
// Spettro keeps the user's configuration, API keys and sessions under
// ~/.spettro, and much of its code finds that directory through $HOME. A test
// that forgets t.Setenv("HOME", t.TempDir()) therefore reads and rewrites the
// developer's real files; one such run changed a real config.json. A package
// whose code can reach ~/.spettro calls Main from its TestMain, so every test
// in the binary starts with HOME (and the XDG and Windows profile variables)
// pointing at a throwaway directory, and adds a test that calls Check.
//
//	func TestMain(m *testing.M) { os.Exit(testhome.Main(m)) }
//
//	func TestHomeIsIsolated(t *testing.T) { testhome.Check(t) }
package testhome

import (
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
)

// realHomes are the home directories the test process started with, recorded
// by Main before it replaces them. Written once by Main before m.Run, read by
// Check afterwards.
var realHomes []string

// isolatedHome is the throwaway home Main set up; empty until Main runs.
var isolatedHome string

// Main points HOME at a fresh temporary directory, runs the tests and
// removes the directory. It returns the exit code for os.Exit. Tests that set
// HOME themselves (t.Setenv) keep working: they only move further away from
// the real home.
func Main(m *testing.M) int {
	realHomes = currentHomes()
	pinGoToolDirs()

	dir, err := os.MkdirTemp("", "spettro-test-home-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "testhome: %v\n", err)
		return 1
	}
	isolatedHome = dir
	for name, value := range map[string]string{
		"HOME":            dir,
		"USERPROFILE":     dir,
		"XDG_CONFIG_HOME": filepath.Join(dir, ".config"),
		"XDG_DATA_HOME":   filepath.Join(dir, ".local", "share"),
		"XDG_STATE_HOME":  filepath.Join(dir, ".local", "state"),
		"XDG_CACHE_HOME":  filepath.Join(dir, ".cache"),
		"APPDATA":         filepath.Join(dir, "AppData", "Roaming"),
		"LOCALAPPDATA":    filepath.Join(dir, "AppData", "Local"),
		// Under sudo, key material follows SUDO_USER to that user's real
		// home (see config.secretsHome).
		"SUDO_USER": "",
	} {
		if err := os.Setenv(name, value); err != nil {
			fmt.Fprintf(os.Stderr, "testhome: set %s: %v\n", name, err)
			return 1
		}
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	return code
}

// Check fails the test unless Main has run and HOME is not one of the home
// directories the process started with.
func Check(t *testing.T) {
	t.Helper()
	if isolatedHome == "" {
		t.Fatal("testhome.Main did not run: add func TestMain(m *testing.M) { os.Exit(testhome.Main(m)) }")
	}
	home := os.Getenv("HOME")
	if home == "" {
		t.Fatal("HOME is empty")
	}
	for _, real := range realHomes {
		if sameDir(home, real) {
			t.Fatalf("HOME is the real home directory %s", real)
		}
	}
}

// currentHomes returns the home directory from $HOME and from the user
// database, which differ when the caller already moved HOME.
func currentHomes() []string {
	var homes []string
	if h, err := os.UserHomeDir(); err == nil && h != "" {
		homes = append(homes, h)
	}
	if h := os.Getenv("HOME"); h != "" {
		homes = append(homes, h)
	}
	if u, err := user.Current(); err == nil && u.HomeDir != "" {
		homes = append(homes, u.HomeDir)
	}
	return homes
}

// goToolDirs are the go command settings whose defaults derive from HOME.
var goToolDirs = []string{"GOENV", "GOCACHE", "GOPATH", "GOMODCACHE"}

// pinGoToolDirs fixes the go command's settings file, build cache and module
// directories to what they resolve to now. Their defaults derive from HOME,
// so a test that runs the go tool (go list, for example) would otherwise
// start with an empty build and module cache in the throwaway home: slow,
// gigabytes of disk, and module downloads. The values come from "go env"
// itself, so a GOPATH or GOCACHE set with "go env -w" is honoured. Without a
// go tool on PATH no test can run one, and nothing is pinned.
func pinGoToolDirs() {
	goTool, err := exec.LookPath("go")
	if err != nil {
		return
	}
	out, err := exec.Command(goTool, append([]string{"env"}, goToolDirs...)...).Output()
	if err != nil {
		return
	}
	values := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	for i, name := range goToolDirs {
		if i >= len(values) || os.Getenv(name) != "" {
			continue
		}
		if v := strings.TrimSpace(values[i]); v != "" {
			_ = os.Setenv(name, v)
		}
	}
}

// sameDir reports whether a and b name the same directory.
func sameDir(a, b string) bool {
	if filepath.Clean(a) == filepath.Clean(b) {
		return true
	}
	ai, errA := os.Stat(a)
	bi, errB := os.Stat(b)
	return errA == nil && errB == nil && os.SameFile(ai, bi)
}
