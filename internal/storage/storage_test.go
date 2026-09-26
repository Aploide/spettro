package storage

import (
	"os"
	"path/filepath"
	"testing"

	"spettro/internal/fsperm"
)

func TestNewCreatesGlobalDirOnly(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cwd := t.TempDir()

	s, err := New(cwd)
	if err != nil {
		t.Fatal(err)
	}
	if s.ProjectDir != filepath.Join(cwd, ".spettro") {
		t.Errorf("ProjectDir = %q", s.ProjectDir)
	}
	if s.GlobalDir != filepath.Join(home, ".spettro") {
		t.Errorf("GlobalDir = %q", s.GlobalDir)
	}
	if info, err := os.Stat(s.GlobalDir); err != nil || !info.IsDir() {
		t.Errorf("global dir %q not created: %v", s.GlobalDir, err)
	}
	// The project dir is created on first write, not by New: an empty
	// .spettro/ in every work dir is clutter the model goes probing.
	if _, err := os.Stat(s.ProjectDir); !os.IsNotExist(err) {
		t.Errorf("project dir %q must not be created eagerly (stat err: %v)", s.ProjectDir, err)
	}
	// The global store holds credentials, so it must not be reachable by other
	// accounts. Asserted through fsperm rather than on mode bits, which
	// Windows does not implement.
	if ok, err := fsperm.IsOwnerOnly(s.GlobalDir); err != nil {
		t.Errorf("check global dir permissions: %v", err)
	} else if !ok {
		t.Error("global dir is accessible beyond its owner")
	}
}

func TestWriteAndAppendProjectFile(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	if err := s.WriteProjectFile("notes.md", "first"); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(s.ProjectDir, "notes.md")
	if data, _ := os.ReadFile(target); string(data) != "first" {
		t.Errorf("content = %q", data)
	}

	// write overwrites
	if err := s.WriteProjectFile("notes.md", "second"); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(target); string(data) != "second" {
		t.Errorf("content after overwrite = %q", data)
	}

	// append appends, and creates missing files
	if err := s.AppendProjectFile("notes.md", "+more"); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(target); string(data) != "second+more" {
		t.Errorf("content after append = %q", data)
	}
	if err := s.AppendProjectFile("new.log", "line"); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(filepath.Join(s.ProjectDir, "new.log")); string(data) != "line" {
		t.Errorf("appended new file = %q", data)
	}
}

// Appending first (no prior write) must also create the lazy project dir.
func TestAppendProjectFileCreatesProjectDir(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AppendProjectFile("log.txt", "x"); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(filepath.Join(s.ProjectDir, "log.txt")); string(data) != "x" {
		t.Errorf("content = %q", data)
	}
}
