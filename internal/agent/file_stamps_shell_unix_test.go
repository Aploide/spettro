//go:build unix

package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"spettro/internal/config"
	"spettro/internal/provider"
)

// The shell re-stamp tests run POSIX shell commands, so they are unix-only;
// the rule itself is platform-neutral (Windows compares size and mtime).

func newStampShellRuntime(t *testing.T) (*toolRuntime, string) {
	t.Helper()
	dir := t.TempDir()
	return &toolRuntime{
		cwd:           dir,
		permission:    config.PermissionYOLO,
		readSet:       map[string]struct{}{},
		requiredReads: map[string]struct{}{},
		allowedShell:  map[string]struct{}{},
		toolPolicies:  map[string]config.ToolSpec{},
	}, dir
}

func stampShell(t *testing.T, r *toolRuntime, command string) string {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"command": command})
	out, err := r.runShellTool(context.Background(), "bash", raw, "bash")
	if err != nil {
		t.Fatalf("bash %q: %v (%s)", command, err, out)
	}
	return out
}

func stampRead(t *testing.T, r *toolRuntime, rel string) {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"path": rel})
	if _, err := r.runFileRead(context.Background(), raw); err != nil {
		t.Fatalf("file-read %s: %v", rel, err)
	}
}

func waitForFileContent(t *testing.T, path, want string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if got, err := os.ReadFile(path); err == nil && string(got) == want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%s never became %q", path, want)
}

func stampEdit(r *toolRuntime, rel, old, new string) error {
	_, err := r.runFileEdit(context.Background(), "file-edit", editArgs(rel, old, new))
	return err
}

// A file the agent's own command rewrites (a formatter, sed -i) stays
// editable, and the command's output says which read files it changed.
func TestShellRestampsFilesItChanged(t *testing.T) {
	r, dir := newStampShellRuntime(t)
	writeTestFile(t, dir, "s.go", "a := 1\nb := 2\n")
	writeTestFile(t, dir, "other.go", "untouched\n")
	stampRead(t, r, "s.go")
	stampRead(t, r, "other.go")

	out := stampShell(t, r, "printf 'a := 1\\nb := 2\\nc := 3\\n' > s.go")
	if !strings.Contains(out, "note: this command changed files you had read (s.go)") {
		t.Fatalf("output lacks the re-stamp note: %q", out)
	}
	if strings.Contains(out, "other.go") {
		t.Fatalf("note names a file the command did not change: %q", out)
	}
	if err := stampEdit(r, "s.go", "c := 3", "c := 30"); err != nil {
		t.Fatalf("edit after own shell change refused: %v", err)
	}
	// The model never saw the new lines: line numbers from its read no
	// longer count as current.
	raw, _ := os.ReadFile(filepath.Join(dir, "s.go"))
	if r.unchangedSinceRead("s.go", raw) {
		t.Fatal("read stamp moved with the shell re-stamp")
	}
}

// A formatter that writes a new file and renames it over the old one is the
// command's own change too.
func TestShellRestampsRenamedOverFile(t *testing.T) {
	r, dir := newStampShellRuntime(t)
	writeTestFile(t, dir, "f.txt", "one\n")
	stampRead(t, r, "f.txt")
	stampShell(t, r, "printf 'two\\n' > f.tmp && mv f.tmp f.txt")
	if err := stampEdit(r, "f.txt", "two", "three"); err != nil {
		t.Fatalf("edit after rename-over refused: %v", err)
	}
}

// A change from outside between calls is still caught, whether or not a
// later command of the agent touches the file as well.
func TestShellRestampKeepsOutsideChangesGuarded(t *testing.T) {
	r, dir := newStampShellRuntime(t)
	writeTestFile(t, dir, "s.go", "a := 1\n")
	writeTestFile(t, dir, "t.go", "x := 1\n")
	stampRead(t, r, "s.go")
	stampRead(t, r, "t.go")

	writeTestFile(t, dir, "s.go", "a := 1\nuser := true\n")
	writeTestFile(t, dir, "t.go", "x := 1\nuser := true\n")
	stampShell(t, r, "echo unrelated")
	stampShell(t, r, "printf 'x := 1\\nuser := true\\nagent := 1\\n' > t.go")

	for _, rel := range []string{"s.go", "t.go"} {
		err := stampEdit(r, rel, "user := true", "user := false")
		if err == nil || !strings.Contains(err.Error(), "modified on disk since you last read it") {
			t.Errorf("%s: outside change not caught: %v", rel, err)
		}
	}
}

// A stamp restored from an earlier turn is trusted by the shell re-stamp
// only once its content is confirmed to match.
func TestShellRestampOfRestoredStamps(t *testing.T) {
	first, dir := newStampShellRuntime(t)
	writeTestFile(t, dir, "same.go", "v := 1\n")
	writeTestFile(t, dir, "moved.go", "w := 1\n")
	stampRead(t, first, "same.go")
	stampRead(t, first, "moved.go")
	carried := []provider.Message{{Role: provider.RoleUser, FileStamps: first.takeStampDelta()}}

	// Between turns the user edits moved.go.
	writeTestFile(t, dir, "moved.go", "w := 1\nuser := 1\n")

	next, _ := newStampShellRuntime(t)
	next.cwd = dir
	next.restoreStamps(carried)
	stampShell(t, next, "printf 'v := 2\\n' > same.go && printf 'w := 2\\nuser := 1\\n' > moved.go")

	if err := stampEdit(next, "same.go", "v := 2", "v := 3"); err != nil {
		t.Errorf("restored stamp, own change: edit refused: %v", err)
	}
	if err := stampEdit(next, "moved.go", "w := 2", "w := 3"); err == nil || !strings.Contains(err.Error(), "modified on disk") {
		t.Errorf("restored stamp, user change between turns: not caught: %v", err)
	}
}

// A background job outlives its call: what it changes stays guarded.
func TestShellRestampSkipsBackgroundJobs(t *testing.T) {
	r, dir := newStampShellRuntime(t)
	writeTestFile(t, dir, "b.txt", "one\n")
	stampRead(t, r, "b.txt")
	snap := r.snapshotStampsForShell()
	if len(snap) != 1 {
		t.Fatalf("snapshot = %d files, want 1", len(snap))
	}
	// Nothing changed during the "command": nothing is re-stamped.
	if changed := r.restampAfterShell(snap); len(changed) != 0 {
		t.Fatalf("re-stamped unchanged files: %v", changed)
	}
	raw, _ := json.Marshal(map[string]any{"command": "sleep 0.2; printf 'two\\n' > b.txt", "run_in_background": true})
	if _, err := r.runShellTool(context.Background(), "bash", raw, "bash"); err != nil {
		t.Fatal(err)
	}
	waitForFileContent(t, filepath.Join(dir, "b.txt"), "two\n")
	if err := stampEdit(r, "b.txt", "two", "three"); err == nil || !strings.Contains(err.Error(), "modified on disk") {
		t.Fatalf("background job change not guarded: %v", err)
	}
}

func stampWrite(r *toolRuntime, rel, content string, appendMode bool) error {
	raw, _ := json.Marshal(map[string]any{"path": rel, "content": content, "append": appendMode})
	_, err := r.execute(context.Background(), toolCall{Tool: "file-write", Args: raw}, map[string]struct{}{"file-write": {}})
	return err
}

// A file the agent's own command changed was never shown to the model, so
// overwriting it whole from the model's last read is refused until it
// reads the file again: the overwrite would revert the command's output,
// and any change another process made while the command ran. Appending and
// file-edit still work, and keep the mark.
func TestShellRestampedFileNeedsReadBeforeOverwrite(t *testing.T) {
	r, dir := newStampShellRuntime(t)
	writeTestFile(t, dir, "f.go", "a := 1\n")
	stampRead(t, r, "f.go")
	stampShell(t, r, "printf 'a := 1\\nuser := 1\\n' > f.go")

	if err := stampWrite(r, "f.go", "a := 2\n", false); err == nil || !strings.Contains(err.Error(), "changed by one of your bash commands") {
		t.Fatalf("blind overwrite after own shell change: %v", err)
	}
	if err := stampWrite(r, "f.go", "tail := 1\n", true); err != nil {
		t.Fatalf("append after own shell change refused: %v", err)
	}
	if err := stampEdit(r, "f.go", "user := 1", "user := 2"); err != nil {
		t.Fatalf("edit after own shell change refused: %v", err)
	}
	if err := stampWrite(r, "f.go", "a := 2\n", false); err == nil {
		t.Fatal("an append or edit cleared the mark: the model still has not seen the command's lines")
	}
	stampRead(t, r, "f.go")
	if err := stampWrite(r, "f.go", "a := 2\n", false); err != nil {
		t.Fatalf("overwrite after re-read refused: %v", err)
	}
	// A full write leaves nothing unseen: writing again needs no read.
	if err := stampWrite(r, "f.go", "a := 3\n", false); err != nil {
		t.Fatalf("second overwrite refused: %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "f.go")); string(got) != "a := 3\n" {
		t.Fatalf("content = %q", got)
	}
}

// The mark rides on the conversation's stamp records like the stamps do,
// and a later record without it (a re-read) clears it.
func TestShellStampedMarkIsCarried(t *testing.T) {
	first, dir := newStampShellRuntime(t)
	writeTestFile(t, dir, "f.go", "a := 1\n")
	stampRead(t, first, "f.go")
	stampShell(t, first, "printf 'a := 9\\n' > f.go")
	carried := []provider.Message{{Role: provider.RoleUser, FileStamps: first.takeStampDelta()}}

	next, _ := newStampShellRuntime(t)
	next.cwd = dir
	next.restoreStamps(carried)
	if err := stampWrite(next, "f.go", "a := 2\n", false); err == nil || !strings.Contains(err.Error(), "changed by one of your bash commands") {
		t.Fatalf("restored mark not enforced: %v", err)
	}

	stampRead(t, first, "f.go")
	carried = append(carried, provider.Message{Role: provider.RoleUser, FileStamps: first.takeStampDelta()})
	last, _ := newStampShellRuntime(t)
	last.cwd = dir
	last.restoreStamps(carried)
	if err := stampWrite(last, "f.go", "a := 2\n", false); err != nil {
		t.Fatalf("re-read in a carried record did not clear the mark: %v", err)
	}
}

// rewriteKeepingMtime changes a file's content in place to same-size text
// and puts its mtime back, as touch -r, rsync -t --inplace or tar -x do:
// size, mtime and inode all stay the same.
func rewriteKeepingMtime(t *testing.T, path, content string) {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(content)) != fi.Size() {
		t.Fatalf("rewrite must keep the size: %d != %d", len(content), fi.Size())
	}
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte(content), 0); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, fi.ModTime(), fi.ModTime()); err != nil {
		t.Fatal(err)
	}
}

// An outside edit that keeps size, mtime and inode is still caught when a
// later command of the agent also writes the file. Here the file was read
// moments ago, so the snapshot hashes it (the racy window).
func TestShellRestampCatchesMtimePreservingEdit(t *testing.T) {
	r, dir := newStampShellRuntime(t)
	path := writeTestFile(t, dir, "g.go", "x := 1\n")
	stampRead(t, r, "g.go")
	rewriteKeepingMtime(t, path, "x := 9\n")
	stampShell(t, r, "printf 'y := 1\\n' >> g.go")
	if err := stampEdit(r, "g.go", "y := 1", "y := 2"); err == nil || !strings.Contains(err.Error(), "modified on disk") {
		t.Fatalf("mtime-preserving outside edit absorbed: %v", err)
	}
}

// The same edit on a file whose identity is trusted (verified long after
// its last change) is caught by the ctime, which no rewrite can set back.
func TestShellSnapshotComparesCtime(t *testing.T) {
	r, dir := newStampShellRuntime(t)
	path := writeTestFile(t, dir, "g.go", "x := 1\n")
	stampRead(t, r, "g.go")
	key := r.stampKey("g.go")
	r.mu.Lock()
	si := r.stampIDs[key]
	si.verifiedAt = max(si.id.mtime, si.id.ctime) + stampRacyWindow
	r.stampIDs[key] = si
	r.mu.Unlock()
	if si.id.ctime == 0 {
		t.Skip("no ctime read on this platform: every snapshot hashes")
	}
	if !identityTrusted(si) {
		t.Fatalf("identity %+v not trusted", si)
	}
	if snap := r.snapshotStampsForShell(); len(snap) != 1 {
		t.Fatalf("trusted, unchanged file not eligible: %d", len(snap))
	}

	time.Sleep(10 * time.Millisecond) // let the ctime move on filesystems with coarse clocks
	rewriteKeepingMtime(t, path, "x := 9\n")
	if snap := r.snapshotStampsForShell(); len(snap) != 0 {
		t.Fatalf("mtime-preserving outside edit left the file eligible: %+v", snap)
	}
}
