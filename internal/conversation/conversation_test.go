package conversation

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSaveLoadRoundTrip(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "convs")
	conv := Conversation{
		ID:        "20260722-101500",
		StartedAt: time.Date(2026, 7, 22, 10, 15, 0, 0, time.UTC),
		Messages: []Message{
			{Role: "user", Content: "hello", At: time.Date(2026, 7, 22, 10, 15, 1, 0, time.UTC)},
			{Role: "assistant", Content: "hi", Thinking: "greeting", Meta: "m", At: time.Date(2026, 7, 22, 10, 15, 2, 0, time.UTC)},
		},
	}
	if err := Save(dir, conv); err != nil {
		t.Fatal(err)
	}
	got, err := Load(filepath.Join(dir, conv.ID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != conv.ID || !got.StartedAt.Equal(conv.StartedAt) || len(got.Messages) != 2 {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
	if got.Messages[1].Thinking != "greeting" || got.Messages[1].Meta != "m" {
		t.Errorf("message fields lost: %+v", got.Messages[1])
	}
}

func TestLoadErrors(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "missing.json")); err == nil {
		t.Error("expected error for missing file")
	}
	bad := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(bad, []byte("{nope"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(bad); err == nil {
		t.Error("expected error for invalid JSON")
	}
}

func TestListSortsAndPreviews(t *testing.T) {
	dir := t.TempDir()
	older := Conversation{
		ID:        "older",
		StartedAt: time.Date(2026, 7, 20, 9, 0, 0, 0, time.UTC),
		Messages: []Message{
			{Role: "assistant", Content: "skip me"},
			{Role: "user", Content: strings.Repeat("x", 100)},
		},
	}
	newer := Conversation{
		ID:        "newer",
		StartedAt: time.Date(2026, 7, 21, 9, 0, 0, 0, time.UTC),
		Messages:  []Message{{Role: "user", Content: "short question"}},
	}
	for _, c := range []Conversation{older, newer} {
		if err := Save(dir, c); err != nil {
			t.Fatal(err)
		}
	}
	// non-conversation files are ignored
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "corrupt.json"), []byte("{bad"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "sub.json"), 0o755); err != nil {
		t.Fatal(err)
	}

	out, err := List(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 {
		t.Fatalf("got %d summaries, want 2: %+v", len(out), out)
	}
	if out[0].ID != "newer" || out[1].ID != "older" {
		t.Errorf("wrong sort order: %s, %s", out[0].ID, out[1].ID)
	}
	if out[0].Preview != "short question" {
		t.Errorf("preview = %q", out[0].Preview)
	}
	if out[1].Preview != strings.Repeat("x", 60)+"…" {
		t.Errorf("long preview not truncated: %q", out[1].Preview)
	}
	if out[0].Path != filepath.Join(dir, "newer.json") {
		t.Errorf("path = %q", out[0].Path)
	}
}

func TestListMissingDir(t *testing.T) {
	out, err := List(filepath.Join(t.TempDir(), "nope"))
	if err != nil {
		t.Fatal(err)
	}
	if out != nil {
		t.Errorf("expected nil, got %+v", out)
	}
}

func TestProjectDir(t *testing.T) {
	a := ProjectDir("/global", "/home/u/projects/app")
	b := ProjectDir("/global", "/home/u/other/app")
	if a == b {
		t.Error("same-name projects in different paths must not collide")
	}
	if a != ProjectDir("/global", "/home/u/projects/app") {
		t.Error("ProjectDir must be deterministic")
	}
	base := filepath.Base(a)
	if !strings.HasPrefix(base, "app-") || len(base) != len("app-")+8 {
		t.Errorf("slug format wrong: %q", base)
	}
	if filepath.Dir(filepath.Dir(a)) != "/global" {
		t.Errorf("not under /global/conversations: %q", a)
	}
}
