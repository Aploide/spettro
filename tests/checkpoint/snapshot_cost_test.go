package checkpoint_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"spettro/internal/checkpoint"
)

// An unchanged tree with an unchanged conversation is indistinguishable from
// the previous checkpoint: it is returned as is, with no new list entry.
func TestIdenticalSnapshotAddsNoEntry(t *testing.T) {
	global := t.TempDir()
	project := t.TempDir()
	if err := os.WriteFile(filepath.Join(project, "a.txt"), []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	cp, err := checkpoint.Open(global, project)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	c1, err := cp.Snapshot("file-edit", "p", []byte("conv"))
	if err != nil {
		t.Fatal(err)
	}
	c2, err := cp.Snapshot("bash", "p", []byte("conv"))
	if err != nil {
		t.Fatal(err)
	}
	if c2.ID != c1.ID || c2.ConvKey() != c1.ConvKey() || !c2.At.Equal(c1.At) {
		t.Fatalf("identical snapshot = %+v, want the previous checkpoint %+v", c2, c1)
	}
	if list, _ := cp.List(); len(list) != 1 {
		t.Fatalf("list has %d entries, want 1", len(list))
	}
	// Without a conversation the same holds.
	c3, err := checkpoint.Open(t.TempDir(), project)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c3.Snapshot("bash", "p", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := c3.Snapshot("bash", "p", nil); err != nil {
		t.Fatal(err)
	}
	if list, _ := c3.List(); len(list) != 1 {
		t.Fatalf("nil-conversation list has %d entries, want 1", len(list))
	}
}

// A run hands every snapshot the same run-start conversation: it is written
// once and shared, every checkpoint still resolves it, and retention does not
// delete it while a kept checkpoint uses it.
func TestRunConversationStoredOnce(t *testing.T) {
	global := t.TempDir()
	project := t.TempDir()
	cp, err := checkpoint.Open(global, project)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	conv := []byte(`{"messages":["` + strings.Repeat("x", 4096) + `"]}`)
	var cps []checkpoint.Checkpoint
	for i := range 3 {
		if err := os.WriteFile(filepath.Join(project, "a.txt"), []byte{byte('a' + i)}, 0o644); err != nil {
			t.Fatal(err)
		}
		c, err := cp.Snapshot("file-edit", "p", conv)
		if err != nil {
			t.Fatal(err)
		}
		cps = append(cps, c)
	}
	if cps[0].ID == cps[1].ID || cps[1].ID == cps[2].ID {
		t.Fatalf("changed trees must mint distinct commits: %v", cps)
	}
	dir := checkpoint.Dir(global, project)
	blobs, err := os.ReadDir(filepath.Join(dir, "conv"))
	if err != nil {
		t.Fatal(err)
	}
	if len(blobs) != 1 {
		t.Fatalf("%d conversation blobs stored for one run, want 1", len(blobs))
	}
	for i, c := range cps {
		if got, _ := cp.Conversation(c.ConvKey()); string(got) != string(conv) {
			t.Fatalf("checkpoint %d lost its conversation", i)
		}
	}

	// Backdate the first checkpoint past retention: its entry goes, but the
	// blob stays because the kept checkpoints share it.
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
		t.Fatal(err)
	}
	list, _ := cp2.List()
	if len(list) != 2 {
		t.Fatalf("retention kept %d checkpoints, want 2", len(list))
	}
	for _, c := range list {
		if got, _ := cp2.Conversation(c.ConvKey()); string(got) != string(conv) {
			t.Fatalf("retention deleted a conversation blob still in use")
		}
	}
	if err := cp2.RestoreFiles(list[0].ID); err != nil {
		t.Fatalf("restore after retention: %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(project, "a.txt")); string(got) != "b" {
		t.Fatalf("a.txt = %q, want b", got)
	}
}

// A file that grows past the size cap after an earlier checkpoint is dropped
// from the new snapshot, and that drop counts as a change from the previous
// one.
func TestFileGrowingPastCapIsDropped(t *testing.T) {
	global := t.TempDir()
	project := t.TempDir()
	data := filepath.Join(project, "data.bin")
	if err := os.WriteFile(data, []byte("small"), 0o644); err != nil {
		t.Fatal(err)
	}
	cp, err := checkpoint.OpenWith(global, project, checkpoint.Options{MaxFileMB: 1})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	c1, err := cp.Snapshot("file-write", "p", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(data, make([]byte, 2<<20), 0o644); err != nil {
		t.Fatal(err)
	}
	c2, err := cp.Snapshot("bash", "p", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(c2.SkippedLarge) != 1 || c2.SkippedLarge[0] != "data.bin" {
		t.Fatalf("SkippedLarge = %v, want [data.bin]", c2.SkippedLarge)
	}
	if c2.ID == c1.ID || c2.FilesChanged != 1 {
		t.Fatalf("drop of the grown file not recorded: id reused=%v FilesChanged=%d", c2.ID == c1.ID, c2.FilesChanged)
	}
}

// FilesChanged counts additions, modifications and deletions since the
// previous checkpoint, and restore undoes all three.
func TestFilesChangedCountsDelta(t *testing.T) {
	global := t.TempDir()
	project := t.TempDir()
	for _, name := range []string{"keep.txt", "edit.txt", "gone.txt"} {
		if err := os.WriteFile(filepath.Join(project, name), []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cp, err := checkpoint.Open(global, project)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	c1, err := cp.Snapshot("file-write", "p", nil)
	if err != nil {
		t.Fatal(err)
	}
	if c1.FilesChanged != 3 {
		t.Fatalf("first checkpoint FilesChanged = %d, want 3", c1.FilesChanged)
	}
	if err := os.WriteFile(filepath.Join(project, "edit.txt"), []byte("changed"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(project, "gone.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, "new.txt"), []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	c2, err := cp.Snapshot("bash", "p", nil)
	if err != nil {
		t.Fatal(err)
	}
	if c2.FilesChanged != 3 {
		t.Fatalf("FilesChanged = %d, want 3 (one edit, one delete, one add)", c2.FilesChanged)
	}
	if err := cp.RestoreFiles(c1.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(project, "gone.txt")); string(got) != "gone.txt" {
		t.Fatalf("deleted file not restored: %q", got)
	}
	if got, _ := os.ReadFile(filepath.Join(project, "edit.txt")); string(got) != "edit.txt" {
		t.Fatalf("edited file not restored: %q", got)
	}
	if _, err := os.Stat(filepath.Join(project, "new.txt")); !os.IsNotExist(err) {
		t.Fatalf("file added after the checkpoint survived restore")
	}
}

// Two sessions on one project share the checkpoint list but each remembers
// its own last conversation. When a session returns to a conversation it
// stored earlier on an unchanged tree, the new entry must name a blob that
// exists, not a fresh key that was never written.
func TestSharedConversationKeyExistsAcrossSessions(t *testing.T) {
	global := t.TempDir()
	project := t.TempDir()
	if err := os.WriteFile(filepath.Join(project, "a.txt"), []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	s1, err := checkpoint.Open(global, project)
	if err != nil {
		t.Fatalf("open s1: %v", err)
	}
	s2, err := checkpoint.Open(global, project)
	if err != nil {
		t.Fatalf("open s2: %v", err)
	}
	conv1, conv2 := []byte("conversation one"), []byte("conversation two")
	steps := []struct {
		cp   *checkpoint.Checkpointer
		conv []byte
	}{{s1, conv1}, {s2, conv2}, {s1, conv1}, {s2, conv2}}
	for i, step := range steps {
		got, err := step.cp.Snapshot("bash", "p", step.conv)
		if err != nil {
			t.Fatalf("snapshot %d: %v", i, err)
		}
		data, err := step.cp.Conversation(got.ConvKey())
		if err != nil || string(data) != string(step.conv) {
			t.Fatalf("snapshot %d: conversation %q = %q, %v; want %q", i, got.ConvKey(), data, err, step.conv)
		}
	}
	list, err := s1.List()
	if err != nil {
		t.Fatal(err)
	}
	for i, entry := range list {
		if data, err := s1.Conversation(entry.ConvKey()); err != nil || data == nil {
			t.Errorf("list entry %d names conversation %q with no blob (%v)", i, entry.ConvKey(), err)
		}
	}
}
