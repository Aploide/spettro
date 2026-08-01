package storage

import (
	"os"
	"path/filepath"
	"testing"

	"spettro/internal/fsperm"
)

func TestNewCreatesDirs(t *testing.T) {
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
	for _, dir := range []string{s.ProjectDir, s.GlobalDir} {
		info, err := os.Stat(dir)
		if err != nil || !info.IsDir() {
			t.Errorf("dir %q not created: %v", dir, err)
		}
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
