package checkpoint_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

func gitProject(t *testing.T, project string) {
	t.Helper()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = project
		cmd.Env = append(os.Environ(),
			"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	run("init", "--quiet")
	run("add", "-A")
	run("-c", "commit.gpgsign=false", "commit", "--quiet", "-m", "init")
}

func TestAlternatesBorrowProjectObjects(t *testing.T) {
	global := t.TempDir()
	project := t.TempDir()
	// Content already committed in the project repo must be borrowed via
	// alternates, not copied into the shadow store.
	big := make([]byte, 512*1024)
	for i := range big {
		big[i] = byte(i)
	}
	if err := os.WriteFile(filepath.Join(project, "big.bin"), big, 0o644); err != nil {
		t.Fatal(err)
	}
	gitProject(t, project)

	cp, err := checkpoint.Open(global, project)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	alt := filepath.Join(checkpoint.Dir(global, project), "repo.git", "objects", "info", "alternates")
	if _, err := os.Stat(alt); err != nil {
		t.Fatalf("alternates file not written: %v", err)
	}
	c1, err := cp.Snapshot("file-write", "p", nil)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	// The shadow store must stay far below the size of the borrowed blob.
	if size := cp.Size(); size > 256*1024 {
		t.Fatalf("shadow store %d bytes; blob was not borrowed via alternates", size)
	}
	if err := os.WriteFile(filepath.Join(project, "big.bin"), []byte("gone"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := cp.RestoreFiles(c1.ID); err != nil {
		t.Fatalf("restore: %v", err)
	}
	got, _ := os.ReadFile(filepath.Join(project, "big.bin"))
	if len(got) != len(big) {
		t.Fatalf("big.bin not restored (got %d bytes)", len(got))
	}
}

func TestPrunedAlternatesDegradesCleanly(t *testing.T) {
	global := t.TempDir()
	project := t.TempDir()
	if err := os.WriteFile(filepath.Join(project, "a.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitProject(t, project)
	cp, err := checkpoint.Open(global, project)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	c1, err := cp.Snapshot("file-write", "p", nil)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	// Simulate the user pruning their repo: borrowed objects vanish.
	if err := os.RemoveAll(filepath.Join(project, ".git", "objects")); err != nil {
		t.Fatal(err)
	}
	err = cp.RestoreFiles(c1.ID)
	if err == nil || !strings.Contains(err.Error(), "no longer restorable") {
		t.Fatalf("want clear degradation error, got %v", err)
	}
}

func TestNoChangeSnapshotReusesCommit(t *testing.T) {
	global := t.TempDir()
	project := t.TempDir()
	if err := os.WriteFile(filepath.Join(project, "a.txt"), []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	cp, err := checkpoint.Open(global, project)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	c1, err := cp.Snapshot("file-write", "p1", []byte("conv1"))
	if err != nil {
		t.Fatalf("snapshot 1: %v", err)
	}
	c2, err := cp.Snapshot("file-edit", "p2", []byte("conv2"))
	if err != nil {
		t.Fatalf("snapshot 2: %v", err)
	}
	if c2.ID != c1.ID {
		t.Fatalf("unchanged tree minted a new commit: %s vs %s", c2.ID, c1.ID)
	}
	if c2.ConvKey() == c1.ConvKey() {
		t.Fatalf("dedup entries must keep distinct conversation keys")
	}
	b1, _ := cp.Conversation(c1.ConvKey())
	b2, _ := cp.Conversation(c2.ConvKey())
	if string(b1) != "conv1" || string(b2) != "conv2" {
		t.Fatalf("conversations = %q, %q", b1, b2)
	}
	list, _ := cp.List()
	if len(list) != 2 {
		t.Fatalf("want 2 list entries, got %d", len(list))
	}
}

func TestOversizedFileSkipped(t *testing.T) {
	global := t.TempDir()
	project := t.TempDir()
	if err := os.WriteFile(filepath.Join(project, "small.txt"), []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}
	huge := make([]byte, 2<<20)
	if err := os.WriteFile(filepath.Join(project, "huge.bin"), huge, 0o644); err != nil {
		t.Fatal(err)
	}
	cp, err := checkpoint.OpenWith(global, project, checkpoint.Options{MaxFileMB: 1})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	c1, err := cp.Snapshot("file-write", "p", nil)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if len(c1.SkippedLarge) != 1 || c1.SkippedLarge[0] != "huge.bin" {
		t.Fatalf("SkippedLarge = %v, want [huge.bin]", c1.SkippedLarge)
	}
	// The oversized file is neither restored nor deleted on rewind.
	if err := os.WriteFile(filepath.Join(project, "small.txt"), []byte("changed"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := cp.RestoreFiles(c1.ID); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if _, err := os.Stat(filepath.Join(project, "huge.bin")); err != nil {
		t.Fatalf("huge.bin should survive restore untouched: %v", err)
	}
	got, _ := os.ReadFile(filepath.Join(project, "small.txt"))
	if string(got) != "ok" {
		t.Fatalf("small.txt = %q, want ok", got)
	}
}

func TestRetentionPrunesOldCheckpoints(t *testing.T) {
	global := t.TempDir()
	project := t.TempDir()
	if err := os.WriteFile(filepath.Join(project, "a.txt"), []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	cp, err := checkpoint.Open(global, project)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	old, err := cp.Snapshot("file-write", "old", []byte("oldconv"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, "a.txt"), []byte("v2"), 0o644); err != nil {
		t.Fatal(err)
	}
	recent, err := cp.Snapshot("file-write", "recent", []byte("recentconv"))
	if err != nil {
		t.Fatal(err)
	}
	// Backdate the first checkpoint beyond the retention horizon.
	dir := checkpoint.Dir(global, project)
	raw, err := os.ReadFile(filepath.Join(dir, "checkpoints.json"))
	if err != nil {
		t.Fatal(err)
	}
	var file struct {
		ProjectPath string                  `json:"project_path"`
		Checkpoints []checkpoint.Checkpoint `json:"checkpoints"`
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatal(err)
	}
	file.Checkpoints[0].At = time.Now().AddDate(0, 0, -30)
	raw, _ = json.Marshal(file)
	if err := os.WriteFile(filepath.Join(dir, "checkpoints.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}

	cp2, err := checkpoint.OpenWith(global, project, checkpoint.Options{RetentionDays: 14})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	got, err := cp2.List()
	if err != nil || len(got) != 1 || got[0].ID != recent.ID {
		t.Fatalf("retention list = %v (err %v), want only recent", got, err)
	}
	if blob, _ := cp2.Conversation(old.ConvKey()); blob != nil {
		t.Fatalf("old conversation blob should be deleted")
	}
	if blob, _ := cp2.Conversation(recent.ConvKey()); string(blob) != "recentconv" {
		t.Fatalf("recent conversation lost: %q", blob)
	}
	// The pruned commit's objects must actually be gone from the store.
	if err := cp2.RestoreFiles(old.ID); err == nil {
		t.Fatalf("pruned checkpoint should not be restorable")
	}
	if err := cp2.RestoreFiles(recent.ID); err != nil {
		t.Fatalf("recent checkpoint must survive retention: %v", err)
	}
}
