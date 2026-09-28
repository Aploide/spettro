package diff

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// A rendered diff line must never be wider than the width it was given. Tabs
// (every Go file), wide characters and escape sequences in file contents all
// used to slip past the rune-based truncation and wrap in the terminal.
func TestRenderRespectsWidthWithTabsAndWideText(t *testing.T) {
	body := "--- a/x.go\n+++ b/x.go\n@@ -1,4 +1,4 @@\n" +
		"-\t\t\treturn " + strings.Repeat("old ", 60) + "\n" +
		"+\t\t\treturn " + strings.Repeat("宽", 120) + "\n" +
		" \t\x1b[31mcolored\x1b[0m context\r\n" +
		"+" + strings.Repeat("A", 5000) + "\n"
	for _, width := range []int{30, 60, 100, 119, 120, 160} {
		out := Render(body, Options{Width: width, Indent: "  ", MaxLines: 3, ExpandHint: "(ctrl+o to expand)"})
		for i, line := range strings.Split(out, "\n") {
			if w := ansi.StringWidth(line); w > width {
				t.Fatalf("width %d: line %d is %d cells: %q", width, i, w, ansi.Strip(line))
			}
			if strings.ContainsAny(ansi.Strip(line), "\t\r") {
				t.Fatalf("width %d: line %d still holds a tab or carriage return: %q", width, i, line)
			}
		}
		if strings.Contains(ansi.Strip(out), "\x1b") {
			t.Fatalf("width %d: an escape sequence from the file reached the terminal", width)
		}
	}
}

// Control characters in a file are shown, not interpreted and not dropped: a
// diff is what the user approves a write from, so nothing may be hidden.
func TestRenderShowsControlCharacters(t *testing.T) {
	body := "--- a/x\n+++ b/x\n@@ -0,0 +1,1 @@\n+rm -rf ~ #\recho hi \x1b[31m\n"
	out := ansi.Strip(Render(body, Options{Width: 100}))
	if !strings.Contains(out, "rm -rf ~ #^Mecho hi ^[[31m") {
		t.Fatalf("control characters were not made visible:\n%s", out)
	}
}

// The side-by-side layout pads each half to a fixed number of cells so the
// divider lines up. Padding by runes put it in a different column on every
// row holding wide characters.
func TestSideBySideDividerStaysInOneColumn(t *testing.T) {
	body := "--- a/x\n+++ b/x\n@@ -1,2 +1,2 @@\n-short\n-中文中文\n+short two\n+更长的中文内容\n"
	out := Render(body, Options{Width: 140})
	col := -1
	for _, line := range strings.Split(ansi.Strip(out), "\n") {
		i := strings.Index(line, "│")
		if i < 0 {
			continue
		}
		cells := ansi.StringWidth(line[:i])
		if col >= 0 && cells != col {
			t.Fatalf("divider moved from column %d to %d:\n%s", col, cells, ansi.Strip(out))
		}
		col = cells
	}
	if col < 0 {
		t.Fatalf("expected a side-by-side layout:\n%s", out)
	}
}
