package agent

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"
)

func newReadRuntime(t *testing.T, files map[string]string) *toolRuntime {
	t.Helper()
	r := newShellTestRuntime(t)
	r.requiredReads = map[string]struct{}{}
	writeTree(t, r.cwd, files)
	return r
}

func numberedLines(from, to int, text func(int) string) string {
	var b strings.Builder
	for i := from; i <= to; i++ {
		b.WriteString(formatNumberedLine(i, text(i)))
	}
	return b.String()
}

func TestFileReadNumbersFullAndRangedReadsAlike(t *testing.T) {
	r := newReadRuntime(t, map[string]string{"a.go": "package a\n\nfunc A() {}\n"})
	full, err := r.runFileRead(context.Background(), []byte(`{"path":"a.go"}`))
	if err != nil {
		t.Fatal(err)
	}
	if want := "     1\tpackage a\n     2\t\n     3\tfunc A() {}\n"; full != want {
		t.Fatalf("full read = %q, want %q", full, want)
	}
	if _, ok := r.readSet["a.go"]; !ok {
		t.Fatal("file not marked as read")
	}
	want := "     3\tfunc A() {}\n"
	for _, args := range []string{
		`{"path":"a.go","start_line":3,"end_line":3}`,
		`{"path":"a.go","offset":3,"limit":1}`,
		`{"file_path":"a.go","offset":"3","limit":"5"}`,
		`{"path":"a.go","start_line":3}`,
	} {
		got, err := r.runFileRead(context.Background(), []byte(args))
		if err != nil {
			t.Fatalf("%s: %v", args, err)
		}
		if got != want {
			t.Fatalf("%s = %q, want %q", args, got, want)
		}
	}
	if got := sliceLines("package a\n\nfunc A() {}\n", 3, 3); got != want {
		t.Fatalf("sliceLines = %q, want the file-read format %q", got, want)
	}
}

var continueRe = regexp.MustCompile(`\[showing lines (\d+)-(\d+) of (\d+); continue with file-read \{"path":"([^"]+)","offset":(\d+),"limit":2000\}\]$`)

func TestFileReadCapsAtDefaultLinesWithContinuationFooter(t *testing.T) {
	var b strings.Builder
	for i := 1; i <= 2500; i++ {
		fmt.Fprintf(&b, "l%d\n", i)
	}
	r := newReadRuntime(t, map[string]string{"big.txt": b.String()})
	out, err := r.runFileRead(context.Background(), []byte(`{"path":"big.txt"}`))
	if err != nil {
		t.Fatal(err)
	}
	m := continueRe.FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("missing continuation footer: %q", out[len(out)-200:])
	}
	if m[1] != "1" || m[2] != "2000" || m[3] != "2500" || m[4] != "big.txt" || m[5] != "2001" {
		t.Fatalf("footer = %v", m[1:])
	}
	if !strings.HasPrefix(out, numberedLines(1, 2000, func(i int) string { return fmt.Sprintf("l%d", i) })) {
		t.Fatal("first 2000 lines not returned in order")
	}
	rest, err := r.runFileRead(context.Background(), []byte(`{"path":"big.txt","offset":2001}`))
	if err != nil {
		t.Fatal(err)
	}
	if rest != numberedLines(2001, 2500, func(i int) string { return fmt.Sprintf("l%d", i) }) {
		t.Fatalf("continuation read wrong: %q", rest[:80])
	}
}

func TestFileReadCapsAtCharBudgetOnLineBoundary(t *testing.T) {
	line := strings.Repeat("x", 99)
	r := newReadRuntime(t, map[string]string{"wide.txt": strings.Repeat(line+"\n", 1000)})
	out, err := r.runFileRead(context.Background(), []byte(`{"path":"wide.txt"}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(out) > r.historyLimit("file-read") {
		t.Fatalf("output %d chars exceeds budget %d", len(out), r.historyLimit("file-read"))
	}
	m := continueRe.FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("missing continuation footer: %q", out[len(out)-200:])
	}
	var last, next int
	fmt.Sscan(m[2], &last)
	fmt.Sscan(m[5], &next)
	if last >= 1000 || next != last+1 {
		t.Fatalf("last=%d next=%d", last, next)
	}
	body := strings.TrimSuffix(out, m[0])
	if body != numberedLines(1, last, func(int) string { return line }) {
		t.Fatal("body does not end on a whole line")
	}
}

func TestFileReadRefusesBinaryAndHandlesEdges(t *testing.T) {
	r := newReadRuntime(t, map[string]string{
		"img.png":   "\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR",
		"latin.txt": "caf\xe9 cr\xe8me\n",
		"empty.txt": "",
		"dir/x.txt": "x\n",
	})
	if _, err := r.runFileRead(context.Background(), []byte(`{"path":"img.png"}`)); err == nil || !strings.Contains(err.Error(), "binary") {
		t.Fatalf("binary file not refused: %v", err)
	}
	if _, ok := r.readSet["img.png"]; ok {
		t.Fatal("refused binary file marked as read")
	}
	// Invalid UTF-8 that is still text (Latin-1) is not binary.
	if out, err := r.runFileRead(context.Background(), []byte(`{"path":"latin.txt"}`)); err != nil || !strings.HasPrefix(out, "     1\tcaf") {
		t.Fatalf("latin-1 text refused: %v %q", err, out)
	}
	if out, err := r.runFileRead(context.Background(), []byte(`{"path":"empty.txt"}`)); err != nil || out != "(empty file)" {
		t.Fatalf("empty file: %v %q", err, out)
	}
	if _, err := r.runFileRead(context.Background(), []byte(`{"path":"dir"}`)); err == nil || !strings.Contains(err.Error(), "directory") {
		t.Fatalf("directory read: %v", err)
	}
	if _, err := r.runFileRead(context.Background(), []byte(`{"path":"dir/x.txt","offset":5}`)); err == nil || !strings.Contains(err.Error(), "past the end") {
		t.Fatalf("offset past end: %v", err)
	}
}

func TestFileReadClipsLongLinesOnRuneBoundary(t *testing.T) {
	long := strings.Repeat("é", fileReadMaxLineChars) // 2 bytes each
	r := newReadRuntime(t, map[string]string{"min.js": long + "\nshort\n"})
	out, err := r.runFileRead(context.Background(), []byte(`{"path":"min.js"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !utf8.ValidString(out) {
		t.Fatal("clipping split a rune")
	}
	if !strings.Contains(out, "line truncated") || !strings.Contains(out, "     2\tshort\n") {
		t.Fatalf("unexpected output: %.120q", out)
	}
	if !strings.HasSuffix(out, fmt.Sprintf("[lines longer than %d chars were clipped]", fileReadMaxLineChars)) {
		t.Fatalf("clip note missing: %q", out[len(out)-120:])
	}
}

// file-read output that still ends up over budget is cut with a pointer to a
// ranged file-read, not to a job-output spool.
func TestSpoolResultFileReadPointsAtFileRead(t *testing.T) {
	r := newShellTestRuntime(t)
	out := numberedLines(1, 5000, func(i int) string { return "line content" })
	got := r.spoolResult("file-read", out)
	if strings.Contains(got, "job-output") {
		t.Fatalf("file-read truncation points at job-output: %q", got[len(got)-200:])
	}
	if len(got) > r.historyLimit("file-read") {
		t.Fatalf("truncated output exceeds budget: %d", len(got))
	}
	last := lastNumberedLine(strings.TrimSuffix(got, got[strings.LastIndex(got, "["):]))
	if want := fmt.Sprintf("[truncated; continue with file-read with offset %d]", last+1); !strings.HasSuffix(got, want) {
		t.Fatalf("footer = %q, want %q", got[strings.LastIndex(got, "["):], want)
	}
}

func TestFileReadThroughExecuteAcceptsAliases(t *testing.T) {
	r := newReadRuntime(t, map[string]string{"x.txt": "one\ntwo\n"})
	out, err := r.execute(t.Context(), toolCall{Tool: "file-read", Args: []byte(`{"file_path":"x.txt","offset":2,"limit":1,"description":"peek"}`)}, map[string]struct{}{"file-read": {}})
	if err != nil {
		t.Fatal(err)
	}
	if out != "     2\ttwo\n" {
		t.Fatalf("out = %q", out)
	}
}
