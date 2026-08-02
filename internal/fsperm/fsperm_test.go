package fsperm_test

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"spettro/internal/fsperm"
)

func TestRestrictToOwnerOnFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys.enc")
	// 0666 on purpose: the point is that RestrictToOwner tightens whatever it
	// finds, including a file created wide open.
	if err := os.WriteFile(path, []byte("secret"), 0o666); err != nil {
		t.Fatal(err)
	}
	if err := fsperm.RestrictToOwner(path); err != nil {
		t.Fatalf("RestrictToOwner: %v", err)
	}
	ok, err := fsperm.IsOwnerOnly(path)
	if err != nil {
		t.Fatalf("IsOwnerOnly: %v", err)
	}
	if !ok {
		t.Error("file is still accessible beyond its owner")
	}
	// Restricting must not cost the owner their own access.
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("owner can no longer read the file: %v", err)
	}
	if string(got) != "secret" {
		t.Errorf("content = %q, want %q", got, "secret")
	}
}

func TestRestrictToOwnerOnDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), ".spettro")
	if err := os.MkdirAll(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := fsperm.RestrictToOwner(dir); err != nil {
		t.Fatalf("RestrictToOwner: %v", err)
	}
	ok, err := fsperm.IsOwnerOnly(dir)
	if err != nil {
		t.Fatalf("IsOwnerOnly: %v", err)
	}
	if !ok {
		t.Error("directory is still accessible beyond its owner")
	}
	// The directory must stay usable: restriction is about other accounts.
	child := filepath.Join(dir, "child.json")
	if err := os.WriteFile(child, []byte("{}"), 0o600); err != nil {
		t.Fatalf("owner can no longer write inside the directory: %v", err)
	}
}

func TestWideOpenPathIsNotOwnerOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "public.txt")
	if err := os.WriteFile(path, []byte("hello"), 0o666); err != nil {
		t.Fatal(err)
	}
	// Chmod rather than rely on the creation mode: a restrictive umask would
	// strip the group and other bits and leave nothing for IsOwnerOnly to
	// report on, making this test pass or fail on the ambient umask instead of
	// on the behaviour it is meant to pin down.
	if err := os.Chmod(path, 0o666); err != nil {
		t.Fatal(err)
	}
	ok, err := fsperm.IsOwnerOnly(path)
	if err != nil {
		t.Fatalf("IsOwnerOnly: %v", err)
	}
	if ok {
		t.Error("a world-readable path reported as owner-only")
	}
}

// Securing the directory rather than every individual write is what keeps the
// secrets stores tractable, but the two platforms buy that with different
// mechanisms and only one of them touches the file itself. On Windows the
// directory's DACL is inheritable, so a file created inside starts out
// owner-only on its own. Unix has no such inheritance — a new file's mode comes
// from its creation mode and the process umask — so what protects a secret
// there is that no other account can traverse a 0700 directory to reach it;
// the write sites additionally pass 0600 themselves. Assert the guarantee the
// host actually makes, since asserting the Windows one everywhere just made
// the result depend on the umask the suite happened to run under.
func TestSecureDirProtectsFilesCreatedInside(t *testing.T) {
	dir := filepath.Join(t.TempDir(), ".spettro")
	if err := fsperm.SecureMkdirAll(dir); err != nil {
		t.Fatalf("SecureMkdirAll: %v", err)
	}
	// 0666 on purpose: no per-file restriction is applied here.
	secret := filepath.Join(dir, "keys.enc")
	if err := os.WriteFile(secret, []byte("token"), 0o666); err != nil {
		t.Fatal(err)
	}
	guarded, what := dir, "the directory holding a secret is reachable by others"
	if runtime.GOOS == "windows" {
		guarded, what = secret, "a file created inside a secured directory is readable by others"
	}
	ok, err := fsperm.IsOwnerOnly(guarded)
	if err != nil {
		t.Fatalf("IsOwnerOnly: %v", err)
	}
	if !ok {
		t.Error(what)
	}
}

// SecureMkdirAll must tighten a directory that already exists, so stores
// created before this protection existed are fixed on the next run.
func TestSecureMkdirAllTightensExistingDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), ".spettro")
	if err := os.MkdirAll(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := fsperm.SecureMkdirAll(dir); err != nil {
		t.Fatalf("SecureMkdirAll: %v", err)
	}
	ok, err := fsperm.IsOwnerOnly(dir)
	if err != nil {
		t.Fatalf("IsOwnerOnly: %v", err)
	}
	if !ok {
		t.Error("pre-existing directory was not tightened")
	}
}

func TestRestrictToOwnerMissingPath(t *testing.T) {
	if err := fsperm.RestrictToOwner(filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Error("expected an error for a path that does not exist")
	}
}
