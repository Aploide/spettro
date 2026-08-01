package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReplaceWithFallbackExactUnchanged(t *testing.T) {
	got, n, tier, err := replaceWithFallback("a\nfoo\nb", "foo", "bar", false, false)
	if err != nil || got != "a\nbar\nb" || n != 1 || tier != editTierExact {
		t.Fatalf("got %q n=%d tier=%d err=%v", got, n, tier, err)
	}
	// Exact replace_all still counts every occurrence.
	got, n, tier, err = replaceWithFallback("x x x", "x", "y", true, false)
	if err != nil || got != "y y y" || n != 3 || tier != editTierExact {
		t.Fatalf("got %q n=%d tier=%d err=%v", got, n, tier, err)
	}
}

func TestReplaceWithFallbackWhitespaceTier(t *testing.T) {
	content := "func main() {\n\tx :=\t1\n}"
	// Model quoted with spaces where the file has tabs.
	got, n, tier, err := replaceWithFallback(content, "\tx := 1", "\tx := 2", false, false)
	if err != nil {
		t.Fatal(err)
	}
	if tier != editTierWhitespace || n != 1 {
		t.Fatalf("tier=%d n=%d", tier, n)
	}
	if got != "func main() {\n\tx := 2\n}" {
		t.Fatalf("got %q", got)
	}
}

func TestReplaceWithFallbackLineTrimPreservesIndent(t *testing.T) {
	content := "if ok {\n\t\tdo()\n\t\tmore()\n}"
	// Model quoted at the wrong indentation level entirely.
	got, n, tier, err := replaceWithFallback(content, "do()\nmore()", "do()\nextra()\nmore()", false, false)
	if err != nil {
		t.Fatal(err)
	}
	if tier != editTierLineTrim || n != 1 {
		t.Fatalf("tier=%d n=%d", tier, n)
	}
	want := "if ok {\n\t\tdo()\n\t\textra()\n\t\tmore()\n}"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestReplaceWithFallbackReindentDelta(t *testing.T) {
	content := "    call(a,\n        b)"
	// Quoted with tabs; file uses spaces. new_string keeps the quoted base indent.
	got, _, tier, err := replaceWithFallback(content, "\tcall(a,\n\t\tb)", "\tcall(a, b)", false, false)
	if err != nil {
		t.Fatal(err)
	}
	if tier != editTierWhitespace {
		t.Fatalf("tier=%d", tier)
	}
	if got != "    call(a, b)" {
		t.Fatalf("got %q", got)
	}
}

func TestReplaceWithFallbackCombinedDrift(t *testing.T) {
	// No indentation in the quote AND internal spacing drift in the file:
	// must fall through tier 2 and match at tier 3.
	content := "func main() {\n\tx :=\t1\n}"
	got, n, tier, err := replaceWithFallback(content, "x := 1", "x := 2", false, false)
	if err != nil {
		t.Fatal(err)
	}
	if tier != editTierLineTrim || n != 1 {
		t.Fatalf("tier=%d n=%d", tier, n)
	}
	if got != "func main() {\n\tx := 2\n}" {
		t.Fatalf("got %q", got)
	}
}

func TestReplaceWithFallbackAmbiguous(t *testing.T) {
	content := "  foo()\nbar\n\tfoo()"
	_, _, _, err := replaceWithFallback(content, "\t foo()", "\t baz()", false, false)
	if err == nil || !strings.Contains(err.Error(), "add surrounding context") {
		t.Fatalf("err=%v", err)
	}
	// replace_all resolves the ambiguity and keeps each line's own indent.
	got, n, _, err := replaceWithFallback(content, "\t foo()", "\t baz()", true, false)
	if err != nil || n != 2 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if got != "  baz()\nbar\n\tbaz()" {
		t.Fatalf("got %q", got)
	}
}

func TestReplaceWithFallbackNotFoundAndBlankPattern(t *testing.T) {
	if _, _, _, err := replaceWithFallback("abc", "zzz", "y", false, false); err == nil {
		t.Fatal("want not-found error")
	}
	// A whitespace-only pattern must not fuzzy-match everywhere.
	if _, _, _, err := replaceWithFallback("a\nb", "   \n\t", "y", false, false); err == nil {
		t.Fatal("want not-found error for blank pattern")
	}
}

func newEditTestRuntime(t *testing.T) (*toolRuntime, string) {
	t.Helper()
	dir := t.TempDir()
	return &toolRuntime{cwd: dir, readSet: map[string]struct{}{}}, dir
}

func TestRunFileEditFuzzyTierReported(t *testing.T) {
	rt, dir := newEditTestRuntime(t)
	path := filepath.Join(dir, "f.go")
	if err := os.WriteFile(path, []byte("func f() {\n\treturn  1\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	args, _ := json.Marshal(map[string]any{
		"path":       "f.go",
		"old_string": "\treturn 1",
		"new_string": "\treturn 2",
	})
	out, err := rt.runFileEdit(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "whitespace normalization") {
		t.Fatalf("tier not reported: %q", out)
	}
	b, _ := os.ReadFile(path)
	if string(b) != "func f() {\n\treturn 2\n}\n" {
		t.Fatalf("file: %q", b)
	}
}

func TestRunMultiEditRollsBackOnFuzzyAmbiguity(t *testing.T) {
	rt, dir := newEditTestRuntime(t)
	orig := "  foo()\nmid\n\tfoo()\n"
	path := filepath.Join(dir, "g.go")
	if err := os.WriteFile(path, []byte(orig), 0o644); err != nil {
		t.Fatal(err)
	}
	args, _ := json.Marshal(map[string]any{
		"path": "g.go",
		"edits": []map[string]any{
			{"old_string": "mid", "new_string": "MID"},
			{"old_string": "\t foo()", "new_string": "\t bar()"}, // fuzzy-ambiguous
		},
	})
	_, err := rt.runMultiEdit(context.Background(), args)
	if err == nil || !strings.Contains(err.Error(), "file untouched") {
		t.Fatalf("err=%v", err)
	}
	b, _ := os.ReadFile(path)
	if string(b) != orig {
		t.Fatalf("file modified despite failure: %q", b)
	}
}

func TestRunMultiEditFuzzySucceeds(t *testing.T) {
	rt, dir := newEditTestRuntime(t)
	path := filepath.Join(dir, "h.go")
	if err := os.WriteFile(path, []byte("a\n    b\nc\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	args, _ := json.Marshal(map[string]any{
		"path": "h.go",
		"edits": []map[string]any{
			{"old_string": "\tb", "new_string": "\tB"},
		},
	})
	out, err := rt.runMultiEdit(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "edit 1 matched") {
		t.Fatalf("out=%q", out)
	}
	b, _ := os.ReadFile(path)
	if string(b) != "a\n    B\nc\n" {
		t.Fatalf("file: %q", b)
	}
}
