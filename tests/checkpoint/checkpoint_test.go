package checkpoint_test

import (
	"os"
	"path/filepath"
	"testing"

	"spettro/internal/checkpoint"
)

func TestSnapshotAndRestore(t *testing.T) {
	global := t.TempDir()
	project := t.TempDir()

	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(project, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("main.txt", "v1")
	write(".gitignore", "ignored.txt\n")
	write("ignored.txt", "secret")

	cp, err := checkpoint.Open(global, project)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	c1, err := cp.Snapshot("file-write", "first edit", []byte(`{"marker":1}`))
	if err != nil {
		t.Fatalf("snapshot 1: %v", err)
	}
	if c1.FilesChanged == 0 {
		t.Fatalf("expected files in first checkpoint, got %d", c1.FilesChanged)
	}

	// Mutate: change a file, add a file, remove nothing.
	write("main.txt", "v2")
	write("new.txt", "created later")
	if _, err := cp.Snapshot("file-edit", "second edit", []byte(`{"marker":2}`)); err != nil {
		t.Fatalf("snapshot 2: %v", err)
	}

	list, err := cp.List()
	if err != nil || len(list) != 2 {
		t.Fatalf("list: %v (len %d)", err, len(list))
	}

	if err := cp.RestoreFiles(c1.ID); err != nil {
		t.Fatalf("restore: %v", err)
	}
	got, _ := os.ReadFile(filepath.Join(project, "main.txt"))
	if string(got) != "v1" {
		t.Fatalf("main.txt = %q, want v1", got)
	}
	if _, err := os.Stat(filepath.Join(project, "new.txt")); !os.IsNotExist(err) {
		t.Fatalf("new.txt should be removed by restore")
	}
	// Gitignored files are neither snapshotted nor deleted on restore.
	if ig, err := os.ReadFile(filepath.Join(project, "ignored.txt")); err != nil || string(ig) != "secret" {
		t.Fatalf("ignored.txt should be untouched, got %q err %v", ig, err)
	}

	blob, err := cp.Conversation(c1.ID)
	if err != nil || string(blob) != `{"marker":1}` {
		t.Fatalf("conversation blob = %q err %v", blob, err)
	}
}
