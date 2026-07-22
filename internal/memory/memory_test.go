package memory

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testStore(t *testing.T) Store {
	t.Helper()
	dir := t.TempDir()
	return Store{
		UserFile:    filepath.Join(dir, "user", "memory.md"),
		ProjectFile: filepath.Join(dir, "project", ".spettro", "memory.md"),
	}
}

func TestSaveCreatesFileWithHeaderAndAppends(t *testing.T) {
	s := testStore(t)
	if _, err := s.Save(ScopeUser, "prefers tabs over spaces"); err != nil {
		t.Fatalf("save: %v", err)
	}
	if _, err := s.Save(ScopeUser, "likes short commit messages"); err != nil {
		t.Fatalf("save: %v", err)
	}
	data, err := os.ReadFile(s.UserFile)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	got := string(data)
	if !strings.HasPrefix(got, "# Spettro memory (user)\n") {
		t.Fatalf("missing header: %q", got)
	}
	if !strings.Contains(got, "- prefers tabs over spaces\n- likes short commit messages\n") {
		t.Fatalf("entries not appended in order: %q", got)
	}
}

func TestSaveProjectScope(t *testing.T) {
	s := testStore(t)
	path, err := s.Save(ScopeProject, "run make lint before tests")
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if path != s.ProjectFile {
		t.Fatalf("path = %q, want %q", path, s.ProjectFile)
	}
	if _, err := os.Stat(s.ProjectFile); err != nil {
		t.Fatalf("project file missing: %v", err)
	}
}

func TestSaveValidation(t *testing.T) {
	s := testStore(t)
	if _, err := s.Save(ScopeUser, "   "); err == nil {
		t.Fatal("empty fact accepted")
	}
	if _, err := s.Save(ScopeUser, "a\nb"); err == nil {
		t.Fatal("multi-line fact accepted")
	}
	if _, err := s.Save(ScopeUser, strings.Repeat("x", maxFactLen+1)); err == nil {
		t.Fatal("oversized fact accepted")
	}
}

func TestLoadCombinesScopesAndEmpty(t *testing.T) {
	s := testStore(t)
	if s.Load() != "" {
		t.Fatal("empty store should load empty context")
	}
	if _, err := s.Save(ScopeUser, "user fact"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Save(ScopeProject, "project fact"); err != nil {
		t.Fatal(err)
	}
	got := s.Load()
	if !strings.Contains(got, "# Memory") || !strings.Contains(got, "- user fact") || !strings.Contains(got, "- project fact") {
		t.Fatalf("combined load missing content: %q", got)
	}
	if strings.Index(got, "- user fact") > strings.Index(got, "- project fact") {
		t.Fatal("user memory should come before project memory")
	}
}

func TestLoadTailTrimsOversizedFile(t *testing.T) {
	s := testStore(t)
	var sb strings.Builder
	for range 2000 {
		sb.WriteString("- some remembered fact line padded out for size\n")
	}
	sb.WriteString("- newest fact\n")
	if err := os.MkdirAll(filepath.Dir(s.UserFile), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.UserFile, []byte(sb.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	got := s.Load()
	if len(got) > maxFileBytes+1024 {
		t.Fatalf("loaded content not capped: %d bytes", len(got))
	}
	if !strings.Contains(got, "- newest fact") {
		t.Fatal("tail trim dropped the newest entries")
	}
}

func TestClear(t *testing.T) {
	s := testStore(t)
	if err := s.Clear(ScopeUser); err != nil {
		t.Fatalf("clear on missing file: %v", err)
	}
	if _, err := s.Save(ScopeUser, "fact"); err != nil {
		t.Fatal(err)
	}
	if err := s.Clear(ScopeUser); err != nil {
		t.Fatalf("clear: %v", err)
	}
	data, err := os.ReadFile(s.UserFile)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(data) != 0 {
		t.Fatalf("file not truncated: %q", data)
	}
}

func TestSessionContextIsFrozenPerProcess(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	ResetSessionCacheForTesting()
	t.Cleanup(ResetSessionCacheForTesting)

	cwd := t.TempDir()
	if got := SessionContext(cwd); got != "" {
		t.Fatalf("expected empty first snapshot, got %q", got)
	}
	// A fact saved after the snapshot must NOT change the session context —
	// the system prompt has to stay byte-stable for prompt caching.
	if _, err := DefaultStore(cwd).Save(ScopeUser, "saved mid-session"); err != nil {
		t.Fatal(err)
	}
	if got := SessionContext(cwd); got != "" {
		t.Fatalf("session snapshot changed mid-session: %q", got)
	}
	// A fresh process (simulated by resetting the cache) sees the fact.
	ResetSessionCacheForTesting()
	if got := SessionContext(cwd); !strings.Contains(got, "saved mid-session") {
		t.Fatalf("new session missing saved fact: %q", got)
	}
}
