package storage

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeFile creates a file with parents.
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// writeHistory creates a fake history dir with a checkpoints.json recording
// projectPath ("" writes the legacy bare-array format).
func writeHistory(t *testing.T, globalDir, hash, projectPath string) string {
	t.Helper()
	dir := filepath.Join(globalDir, "history", hash)
	var raw []byte
	if projectPath == "" {
		raw = []byte(`[]`)
	} else {
		raw, _ = json.Marshal(map[string]any{"project_path": projectPath, "checkpoints": []any{}})
	}
	writeFile(t, filepath.Join(dir, "checkpoints.json"), string(raw))
	writeFile(t, filepath.Join(dir, "repo.git", "HEAD"), "ref: refs/heads/main\n")
	return dir
}

// writeSession creates a session dir with metadata.
func writeSession(t *testing.T, globalDir, id, projectHash string, updated time.Time) string {
	t.Helper()
	dir := filepath.Join(globalDir, "sessions", id)
	meta, _ := json.Marshal(map[string]any{
		"id": id, "project_hash": projectHash, "updated_at": updated,
	})
	writeFile(t, filepath.Join(dir, "session.json"), string(meta))
	writeFile(t, filepath.Join(dir, "messages.json"), "[]")
	return dir
}

// TestMain sandboxes the spool scan: without this, tests running Clean on a
// real machine could delete genuine spettro-spool-* dirs under /tmp.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "spettro-registry-test-spool-*")
	if err == nil {
		spoolRoot = dir
	}
	code := m.Run()
	if dir != "" {
		os.RemoveAll(dir)
	}
	os.Exit(code)
}

func TestDeadSpoolDetection(t *testing.T) {
	dead := filepath.Join(spoolRoot, "spettro-spool-dead")
	writeFile(t, filepath.Join(dead, "1.txt"), "old output")
	past := time.Now().Add(-72 * time.Hour)
	if err := os.Chtimes(dead, past, past); err != nil {
		t.Fatal(err)
	}
	fresh := filepath.Join(spoolRoot, "spettro-spool-fresh")
	writeFile(t, filepath.Join(fresh, "1.txt"), "live output")
	defer os.RemoveAll(fresh)

	r := Inventory(t.TempDir(), filepath.Join(t.TempDir(), ".spettro"), CleanOptions{})
	spool := findClass(t, r, "spool")
	if spool.Count != 1 || len(spool.Items) != 1 || spool.Items[0].Path != dead {
		t.Fatalf("spool items = %+v, want only the dead dir", spool.Items)
	}
	if _, err := Clean(r.PreselectedItems()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dead); !os.IsNotExist(err) {
		t.Fatal("dead spool dir should have been cleaned")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatal("recently-touched spool dir must survive (may belong to a live session)")
	}
}

func findClass(t *testing.T, r Report, name string) ClassReport {
	t.Helper()
	for _, c := range r.Classes {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("class %q not in report", name)
	return ClassReport{}
}

func TestOrphanDetection(t *testing.T) {
	global := t.TempDir()
	liveProject := t.TempDir()
	writeHistory(t, global, "aaaa", liveProject)                     // live
	orphan := writeHistory(t, global, "bbbb", "/nonexistent/gone-1") // orphan
	writeHistory(t, global, "cccc", "")                              // legacy, unknown project

	r := Inventory(global, filepath.Join(liveProject, ".spettro"), CleanOptions{})
	hist := findClass(t, r, "history")
	if hist.Count != 3 {
		t.Fatalf("history count = %d, want 3", hist.Count)
	}
	var preselected []string
	for _, it := range hist.Items {
		if it.Preselected {
			preselected = append(preselected, it.Path)
		}
	}
	if len(preselected) != 1 || preselected[0] != orphan {
		t.Fatalf("preselected = %v, want only the orphan %s", preselected, orphan)
	}
}

func TestSessionSurvivors(t *testing.T) {
	global := t.TempDir()
	project := t.TempDir()
	old := time.Now().AddDate(0, 0, -90)
	// 4 old sessions of one project + the active session (also old).
	for _, id := range []string{"session-old-1", "session-old-2", "session-old-3"} {
		writeSession(t, global, id, "proj1", old)
	}
	active := writeSession(t, global, "session-active", "proj1", old)
	recent := writeSession(t, global, "session-recent", "proj1", time.Now())

	r := Inventory(global, filepath.Join(project, ".spettro"), CleanOptions{
		SessionAgeDays:  30,
		KeepSessions:    2,
		ActiveSessionID: "session-active",
	})
	sess := findClass(t, r, "sessions")
	for _, it := range sess.Items {
		if filepath.Base(it.Path) == "session-active" {
			t.Fatal("active session must not be listed at all")
		}
	}
	// keep-K = 2: the 2 most recent (recent + active or old-N by mtime) are
	// protected. Preselected must be old sessions beyond rank 2 only.
	for _, it := range r.PreselectedItems() {
		if it.ClassName != "sessions" {
			continue
		}
		base := filepath.Base(it.Path)
		if base == "session-active" || base == "session-recent" {
			t.Fatalf("%s must survive", base)
		}
	}
	if _, err := os.Stat(active); err != nil {
		t.Fatal("active session dir missing before clean")
	}
	if freed, err := Clean(r.PreselectedItems()); err != nil || freed == 0 {
		t.Fatalf("clean: freed=%d err=%v", freed, err)
	}
	for _, path := range []string{active, recent} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("survivor %s was deleted", path)
		}
	}
}

func TestSecretsNeverListedOrDeleted(t *testing.T) {
	global := t.TempDir()
	project := t.TempDir()
	secrets := []string{"config.json", "keys.enc", "master.key", "trusted.json", "memory.md"}
	for _, name := range secrets {
		writeFile(t, filepath.Join(global, name), "precious")
	}
	writeFile(t, filepath.Join(global, "catalog.json"), "{}")
	writeHistory(t, global, "dddd", "/nonexistent/gone-2")

	r := Inventory(global, filepath.Join(project, ".spettro"), CleanOptions{})
	for _, c := range r.Classes {
		if !c.Class.Reclaimable() && len(c.Items) > 0 {
			t.Fatalf("class %s (%s) must never emit deletable items", c.Name, c.Class)
		}
	}
	if _, err := Clean(r.PreselectedItems()); err != nil {
		t.Fatal(err)
	}
	for _, name := range secrets {
		if _, err := os.Stat(filepath.Join(global, name)); err != nil {
			t.Fatalf("secret %s was deleted", name)
		}
	}
	// The cache and the orphan should be gone.
	if _, err := os.Stat(filepath.Join(global, "catalog.json")); !os.IsNotExist(err) {
		t.Fatal("catalog.json should have been cleaned")
	}
	if _, err := os.Stat(filepath.Join(global, "history", "dddd")); !os.IsNotExist(err) {
		t.Fatal("orphaned history should have been cleaned")
	}
}

func TestUnknownEntriesReportedNeverDeleted(t *testing.T) {
	global := t.TempDir()
	project := t.TempDir()
	writeFile(t, filepath.Join(global, "mystery.dat"), "???")

	r := Inventory(global, filepath.Join(project, ".spettro"), CleanOptions{})
	if len(r.Unknown) != 1 || r.Unknown[0] != "mystery.dat" {
		t.Fatalf("unknown = %v, want [mystery.dat]", r.Unknown)
	}
	if _, err := Clean(r.PreselectedItems()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(global, "mystery.dat")); err != nil {
		t.Fatal("unknown entry was deleted")
	}
}

func TestDeleterPathValidation(t *testing.T) {
	root := t.TempDir()
	victim := t.TempDir()
	writeFile(t, filepath.Join(victim, "file"), "keep me")

	it := Item{Path: victim, root: filepath.Join(root, "history")}
	if err := it.Delete(); err == nil {
		t.Fatal("delete outside root must fail")
	}
	if _, err := os.Stat(filepath.Join(victim, "file")); err != nil {
		t.Fatal("victim was deleted despite validation")
	}

	inside := filepath.Join(root, "history", "entry")
	writeFile(t, filepath.Join(inside, "file"), "x")
	it = Item{
		Path:  inside,
		root:  filepath.Join(root, "history"),
		match: func(string) bool { return false },
	}
	if err := it.Delete(); err == nil {
		t.Fatal("delete failing the class pattern must fail")
	}
	// Root itself must be refused too.
	it = Item{Path: filepath.Join(root, "history"), root: filepath.Join(root, "history")}
	if err := it.Delete(); err == nil {
		t.Fatal("deleting the class root itself must fail")
	}
}

func TestProjectCacheCleaned(t *testing.T) {
	global := t.TempDir()
	project := t.TempDir()
	projectDir := filepath.Join(project, ".spettro")
	writeFile(t, filepath.Join(projectDir, "cache", "symbols.json"), "{}")
	writeFile(t, filepath.Join(projectDir, "memory.md"), "project memory")

	r := Inventory(global, projectDir, CleanOptions{})
	if _, err := Clean(r.PreselectedItems()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(projectDir, "cache")); !os.IsNotExist(err) {
		t.Fatal("project cache should have been cleaned")
	}
	if _, err := os.Stat(filepath.Join(projectDir, "memory.md")); err != nil {
		t.Fatal("project memory.md was deleted")
	}
}
