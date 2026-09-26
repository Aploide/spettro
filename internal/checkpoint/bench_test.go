package checkpoint

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// mediumRepo builds a committed git project of files×dirs source-like files,
// roughly the shape of a mid-sized Go/TS repository.
func mediumRepo(b *testing.B, dirs, files int) string {
	b.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		b.Skip("git not installed")
	}
	project := b.TempDir()
	body := strings.Repeat("func placeholder() { return }\n", 60)
	for d := range dirs {
		dir := filepath.Join(project, fmt.Sprintf("pkg%03d", d))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			b.Fatal(err)
		}
		for f := range files {
			name := filepath.Join(dir, fmt.Sprintf("file%03d.go", f))
			if err := os.WriteFile(name, []byte(fmt.Sprintf("package p%d\n// %d\n%s", d, f, body)), 0o644); err != nil {
				b.Fatal(err)
			}
		}
	}
	for _, args := range [][]string{
		{"init", "--quiet"},
		{"add", "-A"},
		{"-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false", "commit", "--quiet", "-m", "init"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = project
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_SYSTEM="+os.DevNull)
		if out, err := cmd.CombinedOutput(); err != nil {
			b.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	return project
}

// conversationBlob is a run-start conversation of realistic size (~256 KB).
var conversationBlob = []byte(`{"messages":"` + strings.Repeat("x", 256<<10) + `"}`)

// BenchmarkSnapshotNoChange measures a snapshot of an unchanged 5000-file
// tree: the common case for consecutive shell commands in one run.
func BenchmarkSnapshotNoChange(b *testing.B) {
	project := mediumRepo(b, 100, 50)
	c, err := Open(b.TempDir(), project)
	if err != nil {
		b.Fatal(err)
	}
	if _, err := c.Snapshot("warmup", "p", conversationBlob); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for range b.N {
		if _, err := c.Snapshot("shell-exec", "p", conversationBlob); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkSnapshotOneFileChanged measures a snapshot after a single edit in
// a 5000-file tree: the common case for file-edit steps.
func BenchmarkSnapshotOneFileChanged(b *testing.B) {
	project := mediumRepo(b, 100, 50)
	c, err := Open(b.TempDir(), project)
	if err != nil {
		b.Fatal(err)
	}
	if _, err := c.Snapshot("warmup", "p", conversationBlob); err != nil {
		b.Fatal(err)
	}
	target := filepath.Join(project, "pkg050", "file025.go")
	b.ResetTimer()
	for i := range b.N {
		b.StopTimer()
		if err := os.WriteFile(target, []byte(fmt.Sprintf("package p\n// edit %d\n", i)), 0o644); err != nil {
			b.Fatal(err)
		}
		b.StartTimer()
		if _, err := c.Snapshot("file-edit", "p", conversationBlob); err != nil {
			b.Fatal(err)
		}
	}
}
