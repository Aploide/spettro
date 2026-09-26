package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// An edits[] item may carry expected_replacements: it is accepted (not an
// unknown field), enforced per item, and above 1 replaces every occurrence.
func TestFileEditPerItemExpectedReplacements(t *testing.T) {
	r, dir := newEditTestRuntime(t)
	p := writeTestFile(t, dir, "e.txt", "a x a\nb\nc c\n")
	args, _ := json.Marshal(map[string]any{"path": "e.txt", "edits": []map[string]any{
		{"old_string": "a", "new_string": "A", "expected_replacements": 2},
		{"old_string": "b", "new_string": "B", "expected_replacements": 1},
		{"old_string": "c c", "new_string": "C", "expected_replacements": nil},
	}})
	if _, err := r.runFileEdit(context.Background(), args); err != nil {
		t.Fatal(err)
	}
	if got := readTestFile(t, p); got != "A x A\nB\nC\n" {
		t.Fatalf("file = %q", got)
	}

	// A count that does not match fails the whole call, naming the item.
	before := readTestFile(t, p)
	args, _ = json.Marshal(map[string]any{"path": "e.txt", "edits": []map[string]any{
		{"old_string": "B", "new_string": "b"},
		{"old_string": "A", "new_string": "a", "expected_replacements": 3},
	}})
	_, err := r.runFileEdit(context.Background(), args)
	if err == nil || !strings.Contains(err.Error(), "edit 2: expected 3 replacements, got 2") {
		t.Fatalf("mismatched per-item count: %v", err)
	}
	if got := readTestFile(t, p); got != before {
		t.Fatalf("a failed call wrote the file: %q", got)
	}

	// The top-level count still counts the whole call.
	args, _ = json.Marshal(map[string]any{"path": "e.txt", "expected_replacements": 2, "edits": []map[string]any{
		{"old_string": "B", "new_string": "b"},
		{"old_string": "C", "new_string": "c"},
	}})
	if _, err := r.runFileEdit(context.Background(), args); err != nil {
		t.Fatalf("top-level count over edits[]: %v", err)
	}
	args, _ = json.Marshal(map[string]any{"path": "e.txt", "expected_replacements": 3, "edits": []map[string]any{
		{"old_string": "b", "new_string": "B"},
	}})
	if _, err := r.runFileEdit(context.Background(), args); err == nil || !strings.Contains(err.Error(), "expected 3 replacements, got 1") {
		t.Fatalf("top-level count mismatch: %v", err)
	}
}

// With old_string alone, expected_replacements above 1 replaces every
// occurrence instead of failing on the ambiguity.
func TestFileEditSingleExpectedReplacementsReplacesAll(t *testing.T) {
	r, dir := newEditTestRuntime(t)
	p := writeTestFile(t, dir, "s.txt", "foo\nfoo\nbar\n")
	args, _ := json.Marshal(map[string]any{"path": "s.txt", "old_string": "foo", "new_string": "baz", "expected_replacements": 2})
	if _, err := r.runFileEdit(context.Background(), args); err != nil {
		t.Fatal(err)
	}
	if got := readTestFile(t, p); got != "baz\nbaz\nbar\n" {
		t.Fatalf("file = %q", got)
	}
	args, _ = json.Marshal(map[string]any{"path": "s.txt", "old_string": "baz", "new_string": "qux", "expected_replacements": 3})
	if _, err := r.runFileEdit(context.Background(), args); err == nil || !strings.Contains(err.Error(), "expected 3 replacements, got 2") {
		t.Fatalf("count mismatch: %v", err)
	}
	// Zero and negative counts are no constraint.
	args, _ = json.Marshal(map[string]any{"path": "s.txt", "old_string": "bar", "new_string": "BAR", "expected_replacements": 0,
		"edits": []map[string]any{{"old_string": "baz\nbaz", "new_string": "x", "expected_replacements": -1}}})
	if _, err := r.runFileEdit(context.Background(), args); err != nil {
		t.Fatalf("zero/negative counts: %v", err)
	}
}
