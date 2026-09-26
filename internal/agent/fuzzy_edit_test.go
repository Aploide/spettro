package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// Typographic characters, spelled by code point so the source stays ASCII.
var (
	lsquo = string(rune(0x2018))
	rsquo = string(rune(0x2019))
	ldquo = string(rune(0x201C))
	rdquo = string(rune(0x201D))
	ellip = string(rune(0x2026))
)

func TestApplyEdit(t *testing.T) {
	goFunc := "func f() {\n\tif x {\n\t\ty()\n\t}\n}\n"
	tests := []struct {
		name       string
		content    string
		old, new   string
		replaceAll bool
		lineOffset int
		want       string
		wantTier   int
		wantCount  int
		wantErr    string   // substring of the error; empty means success
		wantNote   string   // substring of one of the notes
		errHas     []string // further substrings the error must contain
	}{
		// Exact matching and uniqueness.
		{name: "exact", content: "a\nfoo\nb", old: "foo", new: "bar", want: "a\nbar\nb", wantTier: editTierExact, wantCount: 1},
		{name: "exact replace_all", content: "x x x", old: "x", new: "y", replaceAll: true, want: "y y y", wantTier: editTierExact, wantCount: 3},
		{name: "exact ambiguous lists lines", content: "x := 1\nfoo()\nx := 1\n", old: "x := 1", new: "x := 2",
			wantErr: "matches 2 locations (lines 1, 3)", errHas: []string{"replace_all", "more surrounding context"}},
		{name: "ambiguous lines honour offset", content: "x\ny\nx", old: "x", new: "z", lineOffset: 10,
			wantErr: "lines 11, 13"},
		{name: "empty old", content: "a", old: "", new: "b", wantErr: "old_string is empty"},

		// Line endings, BOM and trailing newlines.
		{name: "crlf file lf old", content: "a\r\nb\r\nc\r\n", old: "a\nb", new: "a\nB\nextra", want: "a\r\nB\r\nextra\r\nc\r\n", wantTier: editTierExact, wantCount: 1},
		{name: "lf file crlf old", content: "a\nb\nc\n", old: "a\r\nb", new: "x\r\ny", want: "x\ny\nc\n", wantTier: editTierExact, wantCount: 1},
		{name: "bom preserved", content: "\xef\xbb\xbfa\r\nb\r\n", old: "a\nb", new: "c", want: "\xef\xbb\xbfc\r\n", wantTier: editTierExact, wantCount: 1},
		{name: "trailing newline at eof", content: "a\nb", old: "b\n", new: "c\n", want: "a\nc", wantTier: editTierLineExact, wantCount: 1},
		{name: "trailing newline with fuzzy tier", content: "a\n  b  \nc", old: "a\nb\n", new: "a\nB\n", want: "a\nB\nc", wantTier: editTierLineTrim, wantCount: 1},
		{name: "trailing newline deletes whole line", content: "keep\n  drop()\nkeep2\n", old: "   drop()\n", new: "", want: "keep\nkeep2\n", wantTier: editTierIndentFlexible, wantCount: 1},
		{name: "mixed line endings", content: "a\r\nb\nc", old: "a\nb", new: "x", want: "x\nc", wantTier: editTierLineExact, wantCount: 1},

		// Indentation handling.
		{name: "indentation flexible reindents to file", content: goFunc, old: "if x {\n\ty()\n}", new: "if x {\n\ty()\n\tz()\n}",
			want: "func f() {\n\tif x {\n\t\ty()\n\t\tz()\n\t}\n}\n", wantTier: editTierIndentFlexible, wantCount: 1},
		{name: "spaces quote into tab file", content: goFunc, old: "    if x {\n        y()\n    }", new: "    if x {\n        y()\n        z()\n    }",
			want: "func f() {\n\tif x {\n\t\ty()\n\t\tz()\n\t}\n}\n", wantTier: editTierLineTrim, wantCount: 1},
		{name: "tab quote into space file", content: "def f():\n    if x:\n        y()\n", old: "\tif x:\n\t\ty()", new: "\tif x:\n\t\ty()\n\t\tz()",
			want: "def f():\n    if x:\n        y()\n        z()\n", wantTier: editTierLineTrim, wantCount: 1},
		{name: "single line quote grows nested block", content: "func f() {\n\treturn nil\n}\n", old: "    return nil", new: "    if err != nil {\n        return err\n    }\n    return nil",
			want: "func f() {\n\tif err != nil {\n\t\treturn err\n\t}\n\treturn nil\n}\n", wantTier: editTierIndentFlexible, wantCount: 1},
		{name: "reindent delta keeps file spaces", content: "    call(a,\n        b)", old: "\tcall(a,\n\t\tb)", new: "\tcall(a, b)",
			want: "    call(a, b)", wantTier: editTierLineTrim, wantCount: 1},

		// Whitespace.
		{name: "internal whitespace", content: "func main() {\n\tx :=\t1\n}", old: "\tx := 1", new: "\tx := 2",
			want: "func main() {\n\tx := 2\n}", wantTier: editTierWhitespace, wantCount: 1, wantNote: "whitespace normalization"},
		{name: "indent and internal whitespace", content: "func main() {\n\tx :=\t1\n}", old: "x := 1", new: "x := 2",
			want: "func main() {\n\tx := 2\n}", wantTier: editTierWhitespace, wantCount: 1},
		{name: "whitespace substring inside line", content: "v := foo(a,   b) + bar", old: "foo(a, b)", new: "foo(b, a)",
			want: "v := foo(b, a) + bar", wantTier: editTierWhitespace, wantCount: 1},

		// Typographic characters.
		{name: "smart quotes in old", content: "msg := \"hello\"\n", old: "msg := " + ldquo + "hello" + rdquo, new: "msg := " + ldquo + "bye" + rdquo,
			want: "msg := \"bye\"\n", wantTier: editTierUnicode, wantCount: 1},
		{name: "smart quotes in file kept in new", content: "It" + rsquo + "s done", old: "It's done", new: "It" + rsquo + "s finished",
			want: "It" + rsquo + "s finished", wantTier: editTierUnicode, wantCount: 1},
		{name: "ellipsis maps back to original bytes", content: "wait" + ellip + " done\n", old: "wait... done", new: "ok",
			want: "ok\n", wantTier: editTierUnicode, wantCount: 1},
		{name: "single quotes plus indent drift", content: "\tx := " + lsquo + "a" + rsquo + "\n", old: "x := 'a'", new: "x := 'b'",
			want: "\tx := 'b'\n", wantTier: editTierUnicode, wantCount: 1},

		// Escapes and boundaries.
		{name: "double escaped newline and tab", content: "a\n\tb\n", old: `a\n\tb`, new: `a\n\tc`, want: "a\n\tc\n", wantTier: editTierEscape, wantCount: 1},
		{name: "double escaped quotes", content: `fmt.Println("hi")`, old: `fmt.Println(\"hi\")`, new: `fmt.Println(\"bye\")`,
			want: `fmt.Println("bye")`, wantTier: editTierEscape, wantCount: 1},
		{name: "trimmed boundary", content: "x := foo(1) + 2", old: "  foo(1)  ", new: "  bar(1)  ", want: "x := bar(1) + 2", wantTier: editTierTrimmedBoundary, wantCount: 1},

		// Line-number prefixes copied from reads.
		{name: "file-read prefixes stripped", content: "a\nb\nc\n", old: "2. b\n3. c", new: "2. B\n3. c", want: "a\nB\nc\n",
			wantTier: editTierExact, wantCount: 1, wantNote: "line-number prefixes"},
		{name: "cat -n prefixes stripped", content: "a\n\tb\nc\n", old: "     2\t\tb\n     3\tc\n", new: "\tB\nc\n", want: "a\n\tB\nc\n", wantTier: editTierExact, wantCount: 1},
		{name: "prefix on blank line", content: "a\n\nc", old: "1. a\n2.\n3. c", new: "x", want: "x", wantTier: editTierExact, wantCount: 1},
		{name: "prefix number disambiguates", content: "x\ny\nx\ny\n", old: "3. x\n4. y", new: "z\nw", want: "x\ny\nz\nw\n", wantTier: editTierExact, wantCount: 1},
		{name: "non consecutive numbers not stripped", content: "a\nb\nc\n", old: "1. a\n3. c", new: "z", wantErr: "not found"},
		{name: "real numbered list matches literally", content: "1. one\n2. two\n", old: "1. one", new: "1. uno", want: "1. uno\n2. two\n", wantTier: editTierExact, wantCount: 1},

		// Block anchors and context.
		{name: "block anchor tolerates stale middle", content: "func a() {\n\tx := compute(1)\n\ty := x * 2\n\treturn y\n}\n",
			old: "func a() {\n\tx := compute(2)\n\ty := x * 2\n\treturn y\n}", new: "func a() {\n\treturn 0\n}",
			want: "func a() {\n\treturn 0\n}\n", wantTier: editTierBlockAnchor, wantCount: 1, wantNote: "first and last lines"},
		{name: "block anchor tolerates a missing line", content: "func a() {\n\tone()\n\ttwo()\n\tthree()\n\tfour()\n}\n",
			old: "func a() {\n\tone()\n\ttwo()\n\tfour()\n}", new: "func a() {}",
			want: "func a() {}\n", wantTier: editTierBlockAnchor, wantCount: 1},
		{name: "block anchor ambiguous", content: "func a() {\n\tx := 1\n\treturn x\n}\nfunc a() {\n\tx := 2\n\treturn x\n}\n",
			old: "func a() {\n\tx := 3\n\treturn x\n}", new: "gone", replaceAll: true, wantErr: "anchored matching finds 2 locations (lines 1, 5)"},
		{name: "context aware when half the lines agree", content: "func b() {\n\talpha()\n\tbeta()\n\tgamma_delta_epsilon()\n\tzeta_eta_theta_iota()\n}\n",
			old: "func b() {\n\talpha()\n\tbeta()\n\tXXXXXXXXXXXXXXXXXXXX\n\tYYYYYYYYYYYYYYYYYYYYYYY\n}", new: "func b() {}",
			want: "func b() {}\n", wantTier: editTierContextAware, wantCount: 1},
		{name: "weak anchors refused", content: "{\n\talpha()\n}\n", old: "{\n\tbeta()\n}", new: "x", wantErr: "not found"},

		// Fuzzy ambiguity and replace_all.
		{name: "fuzzy ambiguous stops chain", content: "  foo()\nbar\n\tfoo()", old: "\t foo()", new: "\t baz()",
			wantErr: "indentation-flexible matching finds 2 locations (lines 1, 3)"},
		{name: "fuzzy replace_all keeps each indent", content: "  foo()\nbar\n\tfoo()", old: "\t foo()", new: "\t baz()", replaceAll: true,
			want: "  baz()\nbar\n\tbaz()", wantTier: editTierIndentFlexible, wantCount: 2},

		// Misses.
		{name: "blank pattern never fuzzy matches", content: "a\n\nb", old: "   \n\t", new: "y", wantErr: "not found"},
		{name: "not found shows closest block", content: "package main\n\nfunc add(a, b int) int {\n\treturn a + b\n}\n",
			old: "func add(a, b int) int {\n\treturn a - b", new: "x", wantErr: "Closest match is lines 3-4 (",
			errHas: []string{"3. func add(a, b int) int {\n4. \treturn a + b\nCopy old_string verbatim"}},
		{name: "not found closest by similarity only", content: "one\ncomputeTotalPrice(items)\nthree\n",
			old: "computeTotalPrize(itemz)", new: "x", wantErr: "Closest match is lines 2-2", errHas: []string{"2. computeTotalPrice(items)"}},
		{name: "not found strong anchor line despite wrong block", content: "a1\nsetupDatabaseConnection(cfg)\nb1\nc1\n",
			old: "setupDatabaseConection(cfg)\nfoo()\nbar()\nbaz()", new: "x", wantErr: "Closest match is lines 2-5",
			errHas: []string{"2. setupDatabaseConnection(cfg)\n3. b1"}},
		{name: "not found nothing similar", content: "alpha\nbeta\n", old: "zzzzqqqq", new: "x", wantErr: "nothing similar"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res, err := applyEdit(tc.content, editRequest{Old: tc.old, New: tc.new, ReplaceAll: tc.replaceAll, LineOffset: tc.lineOffset})
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("want error %q, got success %q (tier %d)", tc.wantErr, res.Content, res.Tier)
				}
				for _, s := range append([]string{tc.wantErr}, tc.errHas...) {
					if !strings.Contains(err.Error(), s) {
						t.Fatalf("error %q lacks %q", err, s)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if res.Content != tc.want {
				t.Fatalf("content\n got %q\nwant %q", res.Content, tc.want)
			}
			if res.Tier != tc.wantTier || res.Count != tc.wantCount {
				t.Fatalf("tier=%d (want %d) count=%d (want %d)", res.Tier, tc.wantTier, res.Count, tc.wantCount)
			}
			if tc.wantNote != "" && !strings.Contains(strings.Join(res.Notes, "; "), tc.wantNote) {
				t.Fatalf("notes %q lack %q", res.Notes, tc.wantNote)
			}
		})
	}
}

func TestStripLineNumberPrefixes(t *testing.T) {
	tests := []struct {
		in, want string
		first    int
		ok       bool
	}{
		{"10. a\n11. b", "a\nb", 10, true},
		{"  7\tx\n  8\ty\n", "x\ny\n", 7, true},
		{"3: a\n4: b", "a\nb", 3, true},
		{"5" + string(rune(0x2192)) + "a", "a", 5, true},
		{"1. a\nb", "1. a\nb", 0, false},       // not every line numbered
		{"1. a\n1. b", "1. a\n1. b", 0, false}, // not consecutive
		{"3.14 is pi", "3.14 is pi", 0, false},
	}
	for _, tc := range tests {
		got, first, ok := stripLineNumberPrefixes(tc.in)
		if got != tc.want || first != tc.first || ok != tc.ok {
			t.Errorf("strip(%q) = %q,%d,%v want %q,%d,%v", tc.in, got, first, ok, tc.want, tc.first, tc.ok)
		}
	}
}

func TestDetectIndentUnit(t *testing.T) {
	tests := []struct {
		name  string
		lines []string
		want  indentUnit
	}{
		{"tabs", []string{"a", "\tb", "\t\tc"}, indentUnit{tab: true}},
		{"four spaces", []string{"a", "    b", "        c", "    d"}, indentUnit{width: 4}},
		{"two spaces", []string{"a:", "  b:", "    c: 1"}, indentUnit{width: 2}},
		{"comment continuation ignored", []string{"\t/*", "\t * x", "\tfoo()"}, indentUnit{tab: true}},
		{"flat", []string{"a", "b"}, indentUnit{}},
	}
	for _, tc := range tests {
		if got := detectIndentUnit(tc.lines); got != tc.want {
			t.Errorf("%s: got %+v want %+v", tc.name, got, tc.want)
		}
	}
}

func TestUnescapeEditString(t *testing.T) {
	tests := map[string]string{
		`a\nb`:         "a\nb",
		`\t\"x\"`:      "\t\"x\"",
		`c:\\path`:     `c:\path`,
		`keep \d \w`:   `keep \d \w`,
		`trailing \`:   `trailing \`,
		"\\`tick\\`":   "`tick`",
		`\$HOME \'q\'`: `$HOME 'q'`,
	}
	for in, want := range tests {
		if got := unescapeEditString(in); got != want {
			t.Errorf("unescape(%q) = %q want %q", in, got, want)
		}
	}
}

func TestLineSimilarity(t *testing.T) {
	if s := lineSimilarity("abc", "abc"); s != 1 {
		t.Fatalf("identical: %v", s)
	}
	if s := lineSimilarity("abcd", "abcf"); s != 0.75 {
		t.Fatalf("one substitution: %v", s)
	}
	if s := lineSimilarity("", "xyz"); s != 0 {
		t.Fatalf("empty vs text: %v", s)
	}
	// An inserted line costs only itself in a block comparison.
	if s := blockSimilarity([]string{"a", "b", "c"}, []string{"a", "x", "b", "c"}); s != 0.75 {
		t.Fatalf("block with insertion: %v", s)
	}
}

func TestEditDiffSummary(t *testing.T) {
	got := editDiffSummary("f.go", "a\r\nb\r\nc\r\n", "a\r\nB\r\nc\r\nd\r\n")
	for _, want := range []string{"diff (+2 -1 lines):", "@@", "-b", "+B", "+d", " a"} {
		if !strings.Contains(got, want) {
			t.Fatalf("summary lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "\r") || strings.Contains(got, "+++") {
		t.Fatalf("summary has CR or file headers:\n%s", got)
	}
	var before, after strings.Builder
	for i := range 200 {
		fmt.Fprintf(&before, "line %d\n", i)
		fmt.Fprintf(&after, "LINE %d\n", i)
	}
	big := editDiffSummary("f.go", before.String(), after.String())
	if !strings.Contains(big, "more diff lines)") || strings.Count(big, "\n") > editDiffMaxLines+2 {
		t.Fatalf("large diff not capped:\n%s", big)
	}
}

func newEditTestRuntime(t *testing.T) (*toolRuntime, string) {
	t.Helper()
	dir := t.TempDir()
	return &toolRuntime{cwd: dir, readSet: map[string]struct{}{}, requiredReads: map[string]struct{}{}}, dir
}

func writeTestFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func readTestFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func editArgs(path, oldStr, newStr string) []byte {
	b, _ := json.Marshal(map[string]any{"path": path, "old_string": oldStr, "new_string": newStr})
	return b
}

// runTool drives a tool through execute, the same path the agent loop uses.
func runTool(t *testing.T, rt *toolRuntime, tool string, args map[string]any) (string, error) {
	t.Helper()
	raw, _ := json.Marshal(args)
	return rt.execute(context.Background(), toolCall{Tool: tool, Args: raw}, map[string]struct{}{tool: {}})
}

func TestRunFileEditFuzzyTierReported(t *testing.T) {
	rt, dir := newEditTestRuntime(t)
	path := writeTestFile(t, dir, "f.go", "func f() {\n\treturn  1\n}\n")
	out, err := rt.runFileEdit(context.Background(), editArgs("f.go", "\treturn 1", "\treturn 2"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "whitespace normalization") {
		t.Fatalf("tier not reported: %q", out)
	}
	if got := readTestFile(t, path); got != "func f() {\n\treturn 2\n}\n" {
		t.Fatalf("file: %q", got)
	}
}

func TestRunFileEditReturnsDiffSummary(t *testing.T) {
	rt, dir := newEditTestRuntime(t)
	writeTestFile(t, dir, "f.go", "package f\n\nfunc f() int {\n\treturn 1\n}\n")
	out, err := rt.runFileEdit(context.Background(), editArgs("f.go", "\treturn 1", "\tx := 2\n\treturn x"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"edited f.go (1 replacements)", "diff (+2 -1 lines):", "-\treturn 1", "+\tx := 2", "+\treturn x"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "not byte-exact") {
		t.Fatalf("exact edit nagged about quoting:\n%s", out)
	}
}

func TestRunFileEditRejectsAmbiguousExactMatch(t *testing.T) {
	rt, dir := newEditTestRuntime(t)
	orig := "x := 1\nfoo()\nx := 1\n"
	path := writeTestFile(t, dir, "a.go", orig)
	_, err := rt.runFileEdit(context.Background(), editArgs("a.go", "x := 1", "x := 2"))
	if err == nil || !strings.Contains(err.Error(), "lines 1, 3") {
		t.Fatalf("err=%v", err)
	}
	if got := readTestFile(t, path); got != orig {
		t.Fatalf("file changed despite ambiguity: %q", got)
	}
}

func TestRunFileEditLineRangeReportsAbsoluteLines(t *testing.T) {
	rt, dir := newEditTestRuntime(t)
	writeTestFile(t, dir, "a.go", "x\nx\nq\nx\nq\nx\n")
	args, _ := json.Marshal(map[string]any{"path": "a.go", "old_string": "x", "new_string": "y", "start_line": 3, "end_line": 6})
	_, err := rt.runFileEdit(context.Background(), args)
	if err == nil || !strings.Contains(err.Error(), "lines 4, 6") {
		t.Fatalf("err=%v", err)
	}
}

func TestRunFileEditIdenticalStrings(t *testing.T) {
	rt, dir := newEditTestRuntime(t)
	writeTestFile(t, dir, "a.go", "x\n")
	if _, err := rt.runFileEdit(context.Background(), editArgs("a.go", "x", "x")); err == nil || !strings.Contains(err.Error(), "identical") {
		t.Fatalf("err=%v", err)
	}
}

func TestRunFileEditCRLFRoundTrip(t *testing.T) {
	rt, dir := newEditTestRuntime(t)
	path := writeTestFile(t, dir, "w.txt", "one\r\ntwo\r\nthree\r\n")
	if _, err := rt.runFileEdit(context.Background(), editArgs("w.txt", "two\nthree", "2\n3\n4")); err != nil {
		t.Fatal(err)
	}
	if got := readTestFile(t, path); got != "one\r\n2\r\n3\r\n4\r\n" {
		t.Fatalf("file: %q", got)
	}
}

func TestStaleReadGuard(t *testing.T) {
	ctx := context.Background()
	rt, dir := newEditTestRuntime(t)
	path := writeTestFile(t, dir, "s.go", "a := 1\nb := 2\n")

	// A file the agent never read can still be edited when old_string matches.
	if _, err := rt.runFileEdit(ctx, editArgs("s.go", "a := 1", "a := 10")); err != nil {
		t.Fatalf("edit of unread file: %v", err)
	}
	// The agent's own edit refreshes the stamp, so a follow-up edit passes.
	if _, err := rt.runFileEdit(ctx, editArgs("s.go", "b := 2", "b := 20")); err != nil {
		t.Fatalf("second edit: %v", err)
	}

	// Someone else changes the file: edits are refused until it is re-read.
	writeTestFile(t, dir, "s.go", "a := 10\nb := 20\nc := 3\n")
	_, err := rt.runFileEdit(ctx, editArgs("s.go", "c := 3", "c := 30"))
	if err == nil || !strings.Contains(err.Error(), "modified on disk since you last read it") {
		t.Fatalf("stale edit err=%v", err)
	}
	multi, _ := json.Marshal(map[string]any{"path": "s.go", "edits": []map[string]any{{"old_string": "c := 3", "new_string": "c := 30"}}})
	if _, err := rt.runMultiEdit(ctx, multi); err == nil || !strings.Contains(err.Error(), "modified on disk") {
		t.Fatalf("stale multi-edit err=%v", err)
	}
	if got := readTestFile(t, path); got != "a := 10\nb := 20\nc := 3\n" {
		t.Fatalf("stale edit wrote the file: %q", got)
	}

	if _, err := runTool(t, rt, "file-read", map[string]any{"path": "s.go", "start_line": 3, "end_line": 3}); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.runFileEdit(ctx, editArgs("s.go", "c := 3", "c := 30")); err != nil {
		t.Fatalf("edit after re-read: %v", err)
	}
	// A touch that leaves the content alone doesn't count as a change.
	writeTestFile(t, dir, "s.go", readTestFile(t, path))
	if _, err := rt.runFileEdit(ctx, editArgs("s.go", "c := 30", "c := 31")); err != nil {
		t.Fatalf("edit after same-content rewrite: %v", err)
	}
}

func TestFileWriteNeedsFullReadAndFreshContent(t *testing.T) {
	rt, dir := newEditTestRuntime(t)
	path := writeTestFile(t, dir, "w.go", "package w\n\nvar target = 1\n")

	// A grep hit marks the file as seen but is not a full read.
	if _, err := runTool(t, rt, "grep", map[string]any{"pattern": "target"}); err != nil {
		t.Fatal(err)
	}
	_, err := runTool(t, rt, "file-write", map[string]any{"path": "w.go", "content": "clobbered\n"})
	if err == nil || !strings.Contains(err.Error(), "search hit is not a full read") {
		t.Fatalf("write after grep err=%v", err)
	}

	if _, err := runTool(t, rt, "file-read", map[string]any{"path": "w.go"}); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, dir, "w.go", "package w\n\nvar target = 2\n")
	_, err = runTool(t, rt, "file-write", map[string]any{"path": "w.go", "content": "overwrite\n"})
	if err == nil || !strings.Contains(err.Error(), "modified on disk") {
		t.Fatalf("stale write err=%v", err)
	}
	if got := readTestFile(t, path); got != "package w\n\nvar target = 2\n" {
		t.Fatalf("stale write clobbered the file: %q", got)
	}

	if _, err := runTool(t, rt, "file-read", map[string]any{"path": "w.go"}); err != nil {
		t.Fatal(err)
	}
	if _, err := runTool(t, rt, "file-write", map[string]any{"path": "w.go", "content": "fresh\n"}); err != nil {
		t.Fatalf("write after re-read: %v", err)
	}
	// Its own write keeps the stamp current, so an edit right after passes.
	if _, err := rt.runFileEdit(context.Background(), editArgs("w.go", "fresh", "fresher")); err != nil {
		t.Fatalf("edit after own write: %v", err)
	}
}

func TestParallelEditsOnOneFileBothLand(t *testing.T) {
	rt, dir := newEditTestRuntime(t)
	var content strings.Builder
	for i := range 20 {
		fmt.Fprintf(&content, "v%d := %d\n", i, i)
	}
	path := writeTestFile(t, dir, "p.go", content.String())
	var wg sync.WaitGroup
	errs := make([]error, 20)
	for i := range 20 {
		wg.Go(func() {
			_, errs[i] = rt.runFileEdit(context.Background(), editArgs("p.go", fmt.Sprintf("v%d := %d\n", i, i), fmt.Sprintf("v%d := %d\n", i, i*100+1)))
		})
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("edit %d: %v", i, err)
		}
	}
	got := readTestFile(t, path)
	for i := range 20 {
		if !strings.Contains(got, fmt.Sprintf("v%d := %d\n", i, i*100+1)) {
			t.Fatalf("edit %d lost:\n%s", i, got)
		}
	}
}

func TestRunMultiEditRollsBackOnFuzzyAmbiguity(t *testing.T) {
	rt, dir := newEditTestRuntime(t)
	orig := "  foo()\nmid\n\tfoo()\n"
	path := writeTestFile(t, dir, "g.go", orig)
	args, _ := json.Marshal(map[string]any{
		"path": "g.go",
		"edits": []map[string]any{
			{"old_string": "mid", "new_string": "MID"},
			{"old_string": "\t foo()", "new_string": "\t bar()"}, // fuzzy-ambiguous
		},
	})
	_, err := rt.runMultiEdit(context.Background(), args)
	if err == nil || !strings.Contains(err.Error(), "file untouched") || !strings.Contains(err.Error(), "edit 2") {
		t.Fatalf("err=%v", err)
	}
	if got := readTestFile(t, path); got != orig {
		t.Fatalf("file modified despite failure: %q", got)
	}
}

func TestRunMultiEditFuzzySucceeds(t *testing.T) {
	rt, dir := newEditTestRuntime(t)
	path := writeTestFile(t, dir, "h.go", "a\n    b\nc\n")
	args, _ := json.Marshal(map[string]any{
		"path": "h.go",
		"edits": []map[string]any{
			{"old_string": "\tb", "new_string": "\tB"},
			{"old_string": "c", "new_string": "C"},
		},
	})
	out, err := rt.runMultiEdit(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "edit 1 matched") || !strings.Contains(out, "diff (+2 -2 lines)") {
		t.Fatalf("out=%q", out)
	}
	if got := readTestFile(t, path); got != "a\n    B\nC\n" {
		t.Fatalf("file: %q", got)
	}
}

func TestRunMultiEditNotFoundShowsClosestMatch(t *testing.T) {
	rt, dir := newEditTestRuntime(t)
	writeTestFile(t, dir, "n.go", "func a() {\n\treturn compute(1)\n}\n")
	args, _ := json.Marshal(map[string]any{
		"path":  "n.go",
		"edits": []map[string]any{{"old_string": "return compute(2)", "new_string": "return 0"}},
	})
	_, err := rt.runMultiEdit(context.Background(), args)
	if err == nil || !strings.Contains(err.Error(), "2. \treturn compute(1)") {
		t.Fatalf("err=%v", err)
	}
}
