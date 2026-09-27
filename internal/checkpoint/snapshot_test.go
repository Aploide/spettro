package checkpoint

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// editScript is a scripted sequence of working-tree edits: step i applies
// its edits to a project, then a checkpoint is taken.
var editScript = []map[string]string{
	{"a.txt": "a1", "dir/b.txt": "b1"},
	{"a.txt": "a2"},
	{},                               // nothing changed
	{"c.txt": "c1", "dir/b.txt": ""}, // "" deletes
	{"dir/d/e.txt": "e1"},
	{},
	{"a.txt": "a3", "c.txt": "c2"},
}

func applyEdits(t *testing.T, project string, edits map[string]string) {
	t.Helper()
	for rel, content := range edits {
		path := filepath.Join(project, filepath.FromSlash(rel))
		if content == "" {
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// refCount returns how many refs/checkpoints/* refs the shadow repo holds.
func refCount(t *testing.T, c *Checkpointer) int {
	t.Helper()
	out, err := c.git("for-each-ref", "--format=%(refname)", "refs/checkpoints/")
	if err != nil {
		t.Fatal(err)
	}
	if out == "" {
		return 0
	}
	return len(strings.Split(out, "\n"))
}

// Prepare+Commit records exactly what Snapshot records, over a scripted
// edit sequence with changed and unchanged steps (and the conversation
// changing midway).
func TestPreparedCommitMatchesSnapshot(t *testing.T) {
	syncCP, syncProject := newTestCheckpointer(t)
	specCP, specProject := newTestCheckpointer(t)
	for i, edits := range editScript {
		applyEdits(t, syncProject, edits)
		applyEdits(t, specProject, edits)
		conv := []byte(fmt.Sprintf(`{"run":%d}`, i/3))
		want, err := syncCP.Snapshot("file-edit", "p", conv)
		if err != nil {
			t.Fatal(err)
		}
		p, err := specCP.Prepare("step")
		if err != nil {
			t.Fatal(err)
		}
		got, err := specCP.Commit(p, "file-edit", "p", conv)
		if err != nil {
			t.Fatalf("step %d: Commit: %v", i, err)
		}
		if got.Tree != want.Tree || got.FilesChanged != want.FilesChanged || got.Tool != want.Tool || (got.Conv == "") != (want.Conv == "") {
			t.Fatalf("step %d: prepared checkpoint %+v, synchronous %+v", i, got, want)
		}
	}
	syncList, _ := syncCP.List()
	specList, _ := specCP.List()
	if len(specList) != len(syncList) {
		t.Fatalf("prepared path recorded %d checkpoints, synchronous %d", len(specList), len(syncList))
	}
	if n := refCount(t, specCP); n != refCount(t, syncCP) {
		t.Fatalf("prepared path pins %d refs, synchronous %d", n, refCount(t, syncCP))
	}
}

// Commit runs no git process: the claim on a mutating call's path is only
// the list entry and the conversation blob.
func TestCommitRunsNoGit(t *testing.T) {
	c, project := newTestCheckpointer(t)
	applyEdits(t, project, map[string]string{"a.txt": "1"})
	p, err := c.Prepare("step")
	if err != nil {
		t.Fatal(err)
	}
	before := c.gitRuns
	if _, err := c.Commit(p, "file-write", "p", []byte("{}")); err != nil {
		t.Fatal(err)
	}
	if runs := c.gitRuns - before; runs != 0 {
		t.Fatalf("Commit ran %d git processes, want 0", runs)
	}
}

// A Prepared snapshot is stale once anything else was staged, recorded or
// restored.
func TestCommitRejectsStalePrepared(t *testing.T) {
	c, project := newTestCheckpointer(t)
	applyEdits(t, project, map[string]string{"a.txt": "1"})
	first, err := c.Snapshot("file-write", "p", nil)
	if err != nil {
		t.Fatal(err)
	}
	for name, intervene := range map[string]func(){
		"snapshot": func() { _, _ = c.Snapshot("bash", "p", nil) },
		"prepare":  func() { _, _ = c.Prepare("step") },
		"restore":  func() { _ = c.RestoreFiles(first.ID) },
	} {
		applyEdits(t, project, map[string]string{"a.txt": name})
		p, err := c.Prepare("step")
		if err != nil {
			t.Fatal(err)
		}
		intervene()
		if _, err := c.Commit(p, "file-edit", "p", nil); !errors.Is(err, ErrStalePrepared) {
			t.Fatalf("Commit after a %s = %v, want ErrStalePrepared", name, err)
		}
		if _, err := c.CommitTracked(p, "file-edit", "p", nil); !errors.Is(err, ErrStalePrepared) {
			t.Fatalf("CommitTracked after a %s = %v, want ErrStalePrepared", name, err)
		}
	}
}

// Another process appending to the list makes a Prepared stale too.
func TestCommitRejectsPreparedAfterExternalCheckpoint(t *testing.T) {
	c, project := newTestCheckpointer(t)
	other, err := Open(filepath.Dir(filepath.Dir(c.dir)), project)
	if err != nil {
		t.Fatal(err)
	}
	applyEdits(t, project, map[string]string{"a.txt": "1"})
	p, err := c.Prepare("step")
	if err != nil {
		t.Fatal(err)
	}
	applyEdits(t, project, map[string]string{"a.txt": "2"})
	if _, err := other.Snapshot("bash", "p", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Commit(p, "file-edit", "p", nil); !errors.Is(err, ErrStalePrepared) {
		t.Fatalf("Commit after another process's checkpoint = %v, want ErrStalePrepared", err)
	}
	list, _ := c.List()
	if len(list) != 1 {
		t.Fatalf("list as seen by the first process has %d entries, want the other process's 1", len(list))
	}
}

// CommitTracked picks up a change to a tracked file made after Prepare, and
// rewinding to it restores that content.
func TestCommitTrackedPicksUpLaterEdit(t *testing.T) {
	c, project := newTestCheckpointer(t)
	applyEdits(t, project, map[string]string{"a.txt": "v1", "b.txt": "b"})
	if _, err := c.Snapshot("file-write", "p", nil); err != nil {
		t.Fatal(err)
	}
	applyEdits(t, project, map[string]string{"a.txt": "v2"})
	p, err := c.Prepare("step")
	if err != nil {
		t.Fatal(err)
	}
	applyEdits(t, project, map[string]string{"a.txt": "v3 (user edit during generation)"})
	cp, err := c.CommitTracked(p, "file-edit", "p", nil)
	if err != nil {
		t.Fatal(err)
	}
	applyEdits(t, project, map[string]string{"a.txt": "v4 (agent edit)"})
	if err := c.RestoreFiles(cp.ID); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(filepath.Join(project, "a.txt")); string(data) != "v3 (user edit during generation)" {
		t.Fatalf("a.txt after rewind = %q, want the edit made before the tool call", data)
	}
}

// Rewind restores every step of a sequence recorded through Prepare+Commit.
func TestRewindRestoresEveryPreparedStep(t *testing.T) {
	c, project := newTestCheckpointer(t)
	var cps []Checkpoint
	var states []map[string]string
	for _, edits := range editScript {
		applyEdits(t, project, edits)
		p, err := c.Prepare("step")
		if err != nil {
			t.Fatal(err)
		}
		cp, err := c.Commit(p, "file-edit", "p", nil)
		if err != nil {
			t.Fatal(err)
		}
		cps = append(cps, cp)
		states = append(states, readTree(t, project))
	}
	for i := len(cps) - 1; i >= 0; i-- {
		if err := c.RestoreFiles(cps[i].ID); err != nil {
			t.Fatal(err)
		}
		if got := readTree(t, project); fmt.Sprint(got) != fmt.Sprint(states[i]) {
			t.Fatalf("rewind to step %d: tree %v, want %v", i, got, states[i])
		}
	}
}

func readTree(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := os.ReadFile(path)
		rel, _ := filepath.Rel(root, path)
		out[filepath.ToSlash(rel)] = string(data)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// An unclaimed prepared commit is unpinned by the next Prepare that does not
// reuse it, and by the next Open after an exit.
func TestUnclaimedPreparedCommitIsUnpinned(t *testing.T) {
	c, project := newTestCheckpointer(t)
	applyEdits(t, project, map[string]string{"a.txt": "1"})
	if _, err := c.Snapshot("file-write", "p", nil); err != nil {
		t.Fatal(err)
	}
	applyEdits(t, project, map[string]string{"a.txt": "2"})
	if _, err := c.Prepare("step"); err != nil {
		t.Fatal(err)
	}
	if n := refCount(t, c); n != 2 {
		t.Fatalf("refs after an unclaimed Prepare = %d, want 2 (checkpoint + prepared)", n)
	}
	applyEdits(t, project, map[string]string{"a.txt": "3"})
	if _, err := c.Prepare("step"); err != nil {
		t.Fatal(err)
	}
	if n := refCount(t, c); n != 2 {
		t.Fatalf("refs after a second unclaimed Prepare = %d, want 2 (the first one unpinned)", n)
	}
	// The process "exits" with the prepared commit unclaimed; Open drops it.
	reopened, err := Open(filepath.Dir(filepath.Dir(c.dir)), project)
	if err != nil {
		t.Fatal(err)
	}
	if n := refCount(t, reopened); n != 1 {
		t.Fatalf("refs after reopening = %d, want only the checkpoint's", n)
	}
	if _, err := os.Stat(reopened.unclaimedMarkerPath()); !os.IsNotExist(err) {
		t.Fatalf("unclaimed marker after reopening: %v, want it removed", err)
	}
	// A Prepare that finds the tree unchanged also unpins a pending one.
	applyEdits(t, project, map[string]string{"a.txt": "4"})
	if _, err := reopened.Prepare("step"); err != nil {
		t.Fatal(err)
	}
	applyEdits(t, project, map[string]string{"a.txt": "1"})
	if _, err := reopened.Prepare("step"); err != nil {
		t.Fatal(err)
	}
	if n := refCount(t, reopened); n != 1 {
		t.Fatalf("refs after an unchanged Prepare = %d, want 1", n)
	}
}

// The cached list follows writes by another process.
func TestListCacheSeesOtherProcessWrites(t *testing.T) {
	c, project := newTestCheckpointer(t)
	applyEdits(t, project, map[string]string{"a.txt": "1"})
	if _, err := c.Snapshot("file-write", "p", nil); err != nil {
		t.Fatal(err)
	}
	other, err := Open(filepath.Dir(filepath.Dir(c.dir)), project)
	if err != nil {
		t.Fatal(err)
	}
	applyEdits(t, project, map[string]string{"a.txt": "2"})
	if _, err := other.Snapshot("bash", "p", nil); err != nil {
		t.Fatal(err)
	}
	list, err := c.List()
	if err != nil || len(list) != 2 {
		t.Fatalf("List after another process's snapshot = %d entries (%v), want 2", len(list), err)
	}
}

func TestIsCommitHash(t *testing.T) {
	for s, want := range map[string]bool{
		strings.Repeat("a", 40): true, strings.Repeat("0", 64): true,
		strings.Repeat("a", 39): false, strings.Repeat("A", 40): false, "HEAD": false, "../x": false,
	} {
		if got := isCommitHash(s); got != want {
			t.Errorf("isCommitHash(%q) = %v, want %v", s, got, want)
		}
	}
}
