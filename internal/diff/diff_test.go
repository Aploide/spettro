package diff

import (
	"regexp"
	"strings"
	"testing"
)

var ansiRE = regexp.MustCompile("\x1b\\[[0-9;]*m")

func stripANSI(s string) string { return ansiRE.ReplaceAllString(s, "") }

func TestUnifiedEqualContentsIsEmpty(t *testing.T) {
	if got := Unified("a.txt", "same\n", "same\n"); got != "" {
		t.Fatalf("expected empty diff, got %q", got)
	}
}

func TestUnifiedBasicEdit(t *testing.T) {
	old := "one\ntwo\nthree\n"
	new := "one\n2\nthree\n"
	got := Unified("f.txt", old, new)
	want := "--- a/f.txt\n+++ b/f.txt\n@@ -1,3 +1,3 @@\n one\n-two\n+2\n three\n"
	if got != want {
		t.Fatalf("unexpected diff:\ngot:\n%s\nwant:\n%s", got, want)
	}
}

func TestUnifiedNewFile(t *testing.T) {
	got := Unified("n.txt", "", "a\nb\n")
	if !strings.HasPrefix(got, "--- /dev/null\n+++ b/n.txt\n") {
		t.Fatalf("new-file diff should use /dev/null header, got:\n%s", got)
	}
	if !strings.Contains(got, "+a\n+b\n") {
		t.Fatalf("new-file diff should add every line, got:\n%s", got)
	}
}

func TestUnifiedSeparateHunks(t *testing.T) {
	var oldSB, newSB strings.Builder
	for i := 0; i < 30; i++ {
		line := "line\n"
		oldSB.WriteString(line)
		newSB.WriteString(line)
	}
	old := "CHANGED-TOP\n" + oldSB.String() + "bottom\n"
	new := "changed-top\n" + newSB.String() + "BOTTOM\n"
	got := Unified("f.txt", old, new)
	if strings.Count(got, "@@ -") != 2 {
		t.Fatalf("expected 2 hunks for distant edits, got:\n%s", got)
	}
}

func TestUnifiedTooLarge(t *testing.T) {
	big := strings.Repeat("x", maxInputBytes+1)
	got := Unified("big.bin", big, big+"y")
	if !strings.Contains(got, "diff too large") {
		t.Fatalf("oversized input should yield summary, got %d bytes", len(got))
	}
}

func TestRenderUnifiedHasLineNumbersAndSigns(t *testing.T) {
	d := Unified("f.txt", "one\ntwo\nthree\n", "one\n2\nthree\n")
	out := stripANSI(Render(d, Options{Width: 80}))
	if !strings.Contains(out, "- two") || !strings.Contains(out, "+ 2") {
		t.Fatalf("rendered diff missing +/- lines:\n%s", out)
	}
	// The deletion carries the old line number 2; the addition the new number 2.
	if !strings.Contains(out, "2   - two") {
		t.Fatalf("deletion should show old line number:\n%s", out)
	}
	if !strings.Contains(out, "  2 + 2") {
		t.Fatalf("addition should show new line number:\n%s", out)
	}
}

func TestRenderCollapsesWithFooter(t *testing.T) {
	var oldSB strings.Builder
	for i := 0; i < 100; i++ {
		oldSB.WriteString("old line\n")
	}
	d := Unified("f.txt", oldSB.String(), "new\n")
	out := stripANSI(Render(d, Options{MaxLines: 10, ExpandHint: "(ctrl+o to expand)"}))
	lines := strings.Split(out, "\n")
	if len(lines) != 11 { // 10 body + footer
		t.Fatalf("expected 11 lines, got %d:\n%s", len(lines), out)
	}
	if !strings.Contains(lines[10], "more lines (ctrl+o to expand)") {
		t.Fatalf("missing truncation footer: %q", lines[10])
	}
}

func TestRenderSideBySideWhenWide(t *testing.T) {
	d := Unified("f.txt", "one\ntwo\nthree\n", "one\n2\nthree\n")
	out := stripANSI(Render(d, Options{Width: 160}))
	if !strings.Contains(out, "│") {
		t.Fatalf("wide render should be side-by-side:\n%s", out)
	}
	// del and add should share a row: "two ... │ ... 2"
	found := false
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, "two") && strings.Contains(l, "│") && strings.Contains(l, "2 2") {
			found = true
		}
	}
	if !found {
		t.Fatalf("paired del/add row not found:\n%s", out)
	}
}

func TestRenderParsesGitDiffOutput(t *testing.T) {
	gitDiff := `diff --git a/f.txt b/f.txt
index 1234567..89abcde 100644
--- a/f.txt
+++ b/f.txt
@@ -1,3 +1,3 @@
 one
-two
+2
 three
`
	out := stripANSI(Render(gitDiff, Options{Width: 80}))
	if !strings.Contains(out, "- two") || !strings.Contains(out, "+ 2") {
		t.Fatalf("git diff output should render:\n%s", out)
	}
}

func TestRenderIndentPrefix(t *testing.T) {
	d := Unified("f.txt", "a\n", "b\n")
	out := stripANSI(Render(d, Options{Indent: ">>"}))
	for _, l := range strings.Split(out, "\n") {
		if !strings.HasPrefix(l, ">>") {
			t.Fatalf("every line should carry the indent, got %q", l)
		}
	}
}

func TestRenderNeverExceedsWidth(t *testing.T) {
	long := strings.Repeat("abcdef ", 40) // ~280 cells
	d := Unified("f.txt", "short\n", long+"\n"+long+"\n")
	for _, width := range []int{40, 80, 160} {
		out := stripANSI(Render(d, Options{Width: width, Indent: "  "}))
		for _, l := range strings.Split(out, "\n") {
			if n := len([]rune(l)); n > width {
				t.Fatalf("width %d: line is %d cells: %q", width, n, l)
			}
		}
	}
}

func TestRenderEmptyInput(t *testing.T) {
	if Render("", Options{}) != "" || Render("  \n", Options{}) != "" {
		t.Fatal("empty diff should render to empty string")
	}
}
