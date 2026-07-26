package spettromobile

import (
	"os"
	"path/filepath"
	"testing"
)

// TestSetHomeRedirectsUserHomeDir pins the contract the iOS host depends on:
// after SetHome, os.UserHomeDir — which is how every storage and config path in
// the engine is resolved — reports the directory the host chose.
func TestSetHomeRedirectsUserHomeDir(t *testing.T) {
	original, hadOriginal := os.LookupEnv("HOME")
	t.Cleanup(func() {
		if hadOriginal {
			_ = os.Setenv("HOME", original)
		} else {
			_ = os.Unsetenv("HOME")
		}
	})

	dir := t.TempDir()
	if err := SetHome(dir); err != nil {
		t.Fatalf("SetHome: %v", err)
	}
	got, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("UserHomeDir: %v", err)
	}
	if got != dir {
		t.Errorf("UserHomeDir = %q, want %q", got, dir)
	}

	// Idempotent, as the host calls it before every engine start.
	if err := SetHome(dir); err != nil {
		t.Fatalf("second SetHome: %v", err)
	}
}

func TestSetHomeRejectsUnusableDirectories(t *testing.T) {
	if err := SetHome(""); err == nil {
		t.Error("empty home was accepted")
	}
	if err := SetHome(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("nonexistent home was accepted")
	}
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := SetHome(file); err == nil {
		t.Error("a regular file was accepted as home")
	}
}
