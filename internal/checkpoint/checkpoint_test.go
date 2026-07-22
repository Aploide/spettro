package checkpoint

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func newTestCheckpointer(t *testing.T) (*Checkpointer, string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	project := t.TempDir()
	c, err := Open(t.TempDir(), project)
	if err != nil {
		t.Fatal(err)
	}
	return c, project
}

func TestDirIsDeterministicAndCollisionFree(t *testing.T) {
	a := Dir("/g", "/home/u/proj")
	if a != Dir("/g", "/home/u/proj") {
		t.Error("Dir must be deterministic")
	}
	if a == Dir("/g", "/home/u/other") {
		t.Error("different projects must get different history dirs")
	}
	if filepath.Dir(a) != filepath.Join("/g", "history") {
		t.Errorf("history dir in wrong place: %q", a)
	}
}

func TestSnapshotListConversation(t *testing.T) {
	c, project := newTestCheckpointer(t)
	if err := os.WriteFile(filepath.Join(project, "a.txt"), []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}

	longPrompt := strings.Repeat("p", 250)
	cp, err := c.Snapshot("file-write", longPrompt, []byte(`{"messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	if cp.ID == "" || cp.Tool != "file-write" {
		t.Fatalf("bad checkpoint: %+v", cp)
	}
	if len(cp.Prompt) != 200+len("…") || !strings.HasSuffix(cp.Prompt, "…") {
		t.Errorf("prompt not truncated to 200: len=%d", len(cp.Prompt))
	}
	if cp.FilesChanged != 1 {
		t.Errorf("FilesChanged = %d, want 1", cp.FilesChanged)
	}

	list, err := c.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].ID != cp.ID {
		t.Fatalf("list = %+v", list)
	}

	conv, err := c.Conversation(cp.ID)
	if err != nil {
		t.Fatal(err)
	}
	if string(conv) != `{"messages":[]}` {
		t.Errorf("conversation blob = %q", conv)
	}
	if conv, err := c.Conversation("unknown-id"); err != nil || conv != nil {
		t.Errorf("missing conversation should be (nil, nil), got (%q, %v)", conv, err)
	}
}

func TestRestoreFiles(t *testing.T) {
	c, project := newTestCheckpointer(t)
	orig := filepath.Join(project, "a.txt")
	if err := os.WriteFile(orig, []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	cp, err := c.Snapshot("edit", "prompt", nil)
	if err != nil {
		t.Fatal(err)
	}

	// Mutate: change a.txt and add a new file.
	if err := os.WriteFile(orig, []byte("v2"), 0o644); err != nil {
		t.Fatal(err)
	}
	extra := filepath.Join(project, "new.txt")
	if err := os.WriteFile(extra, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := c.RestoreFiles(cp.ID); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(orig); string(data) != "v1" {
		t.Errorf("a.txt after restore = %q, want v1", data)
	}
	if _, err := os.Stat(extra); !os.IsNotExist(err) {
		t.Error("file created after checkpoint should be removed by restore")
	}
}

func TestGitignoreHonoured(t *testing.T) {
	c, project := newTestCheckpointer(t)
	if err := os.WriteFile(filepath.Join(project, ".gitignore"), []byte("secret.txt\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, "secret.txt"), []byte("keep out"), 0o644); err != nil {
		t.Fatal(err)
	}
	cp, err := c.Snapshot("edit", "p", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.RestoreFiles(cp.ID); err != nil {
		t.Fatal(err)
	}
	// Gitignored file must survive restore untouched (clean leaves it alone).
	if data, err := os.ReadFile(filepath.Join(project, "secret.txt")); err != nil || string(data) != "keep out" {
		t.Errorf("gitignored file harmed by restore: %q, %v", data, err)
	}
}

func TestSnapshotDisabled(t *testing.T) {
	c := &Checkpointer{disabled: true}
	if _, err := c.Snapshot("t", "p", nil); err == nil {
		t.Error("disabled checkpointer must refuse Snapshot")
	}
	if err := c.RestoreFiles("x"); err == nil {
		t.Error("disabled checkpointer must refuse RestoreFiles")
	}
}
