package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newWriteTestRuntime returns a runtime whose workspace holds keep.go, already
// read (so writes to it are allowed), and the allowed set for execute.
func newWriteTestRuntime(t *testing.T) (*toolRuntime, map[string]struct{}) {
	t.Helper()
	r := newShellTestRuntime(t)
	if err := os.WriteFile(filepath.Join(r.cwd, "keep.go"), []byte("package keep\n\nfunc Important() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r.readSet["keep.go"] = struct{}{}
	allowed := map[string]struct{}{"file-write": {}, "file-edit": {}}
	return r, allowed
}

func readWorkspaceFile(t *testing.T, r *toolRuntime, rel string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(r.cwd, rel))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// A misspelled content or new_string key used to decode as "" under the
// lenient decoder: the file was truncated, or the matched code deleted, and the
// call still reported success. Write tools must reject such calls untouched.
func TestWriteToolsRejectMisnamedDestructiveFields(t *testing.T) {
	const original = "package keep\n\nfunc Important() {}\n"
	cases := []struct {
		tool string
		args string
	}{
		{"file-write", `{"path":"keep.go","text":"package c\n"}`},
		{"file-write", `{"path":"keep.go"}`},
		{"file-write", `{"path":"keep.go","content":null}`},
		{"file-edit", `{"path":"keep.go","old_string":"func Important() {}","new_text":"x"}`},
		{"file-edit", `{"path":"keep.go","old_string":"func Important() {}"}`},
		{"file-edit", `{"path":"keep.go","edits":[{"old_string":"func Important() {}"}]}`},
		{"multi-edit", `{"path":"keep.go","edits":[{"old_string":"func Important() {}"}]}`},
		{"multi-edit", `{"path":"keep.go","edits":[{"old_string":"func Important() {}","new":"x"}]}`},
	}
	for _, c := range cases {
		r, allowed := newWriteTestRuntime(t)
		out, err := r.execute(context.Background(), toolCall{Tool: c.tool, Args: []byte(c.args)}, allowed)
		if err == nil {
			t.Errorf("%s %s: accepted (%q)", c.tool, c.args, out)
		}
		if got := readWorkspaceFile(t, r, "keep.go"); got != original {
			t.Errorf("%s %s: file changed to %q", c.tool, c.args, got)
		}
	}
}

// The spellings other harnesses use for the same fields are mapped explicitly.
func TestWriteToolsAcceptKnownAliases(t *testing.T) {
	ctx := context.Background()

	r, allowed := newWriteTestRuntime(t)
	if _, err := r.execute(ctx, toolCall{Tool: "file-write", Args: []byte(`{"file_path":"new.txt","file_text":"hello world"}`)}, allowed); err != nil {
		t.Fatal(err)
	}
	if got := readWorkspaceFile(t, r, "new.txt"); got != "hello world" {
		t.Fatalf("file_text write = %q", got)
	}
	if _, err := r.execute(ctx, toolCall{Tool: "file-write", Args: []byte(`{"path":"new.txt","contents":"second"}`)}, allowed); err != nil {
		t.Fatal(err)
	}
	if got := readWorkspaceFile(t, r, "new.txt"); got != "second" {
		t.Fatalf("contents write = %q", got)
	}
	// An explicit empty content is still a deliberate truncation.
	if _, err := r.execute(ctx, toolCall{Tool: "file-write", Args: []byte(`{"path":"new.txt","content":""}`)}, allowed); err != nil {
		t.Fatal(err)
	}
	if got := readWorkspaceFile(t, r, "new.txt"); got != "" {
		t.Fatalf("empty content write = %q", got)
	}

	if _, err := r.execute(ctx, toolCall{Tool: "file-edit", Args: []byte(`{"file_path":"keep.go","old_str":"func Important() {}","new_str":"func Important() int { return 1 }"}`)}, allowed); err != nil {
		t.Fatal(err)
	}
	if got := readWorkspaceFile(t, r, "keep.go"); !strings.Contains(got, "func Important() int { return 1 }") {
		t.Fatalf("old_str/new_str edit = %q", got)
	}
	if _, err := r.execute(ctx, toolCall{Tool: "multi-edit", Args: []byte(`{"path":"keep.go","edits":[{"old_str":"package keep","new_str":"package kept"},{"old_string":"return 1","new_string":""}]}`)}, allowed); err != nil {
		t.Fatal(err)
	}
	if got := readWorkspaceFile(t, r, "keep.go"); !strings.HasPrefix(got, "package kept") || strings.Contains(got, "return 1") {
		t.Fatalf("multi-edit aliases = %q", got)
	}
}

func TestDecodeJSONExactRejectsUnknownFields(t *testing.T) {
	var args struct {
		Path string `json:"path"`
	}
	if err := decodeJSONExact([]byte(`{"path":"a","file_text":"x"}`), &args); err == nil || !strings.Contains(err.Error(), "file_text") {
		t.Fatalf("unknown field not named in error: %v", err)
	}
}
