package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestFileReadPrefixesAreStrippedByFileEdit ties the two units together: a
// model that pastes file-read's numbered lines ("     7\tcode") into
// old_string/new_string still gets a clean edit, and the file never gains
// a line-number prefix.
func TestFileReadPrefixesAreStrippedByFileEdit(t *testing.T) {
	r := newShellTestRuntime(t)
	src := "package p\n\nfunc a() int {\n\treturn 1\n}\n\nfunc b() int {\n\treturn 1\n}\n"
	path := filepath.Join(r.cwd, "p.go")
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	allowed := map[string]struct{}{"file-read": {}, "file-edit": {}}
	out, err := r.execute(context.Background(), toolCall{Tool: "file-read", Args: mustJSON(t, map[string]any{"path": "p.go"})}, allowed)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.SplitAfter(out, "\n")
	if len(lines) < 9 || !strings.HasPrefix(lines[7], "     8\t") {
		t.Fatalf("file-read should number every line cat -n style, got %q", out)
	}
	// Lines 7-9 (func b) copied verbatim from the read, prefixes included.
	// "return 1" alone is ambiguous; the prefixed block is not.
	oldString := strings.Join(lines[6:9], "")
	newString := strings.Replace(oldString, "return 1", "return 2", 1)
	res, err := r.execute(context.Background(), toolCall{Tool: "file-edit", Args: mustJSON(t, map[string]any{
		"path": "p.go", "old_string": oldString, "new_string": newString,
	})}, allowed)
	if err != nil {
		t.Fatalf("pasted file-read lines must edit cleanly: %v", err)
	}
	got, _ := os.ReadFile(path)
	want := strings.Replace(src, "func b() int {\n\treturn 1", "func b() int {\n\treturn 2", 1)
	if string(got) != want {
		t.Fatalf("edit result:\n%s\nwant:\n%s\n(tool said %q)", got, want, res)
	}
}
