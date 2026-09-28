package diff

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"spettro/internal/termtext"
)

// With Wrap, nothing is cut: every row fits the width, and the rows of a
// line, gutter and sign removed, join back into the whole line, escaped
// characters included. Overflows says when the plain render would cut.
func TestRenderWrapKeepsEveryCharacter(t *testing.T) {
	long := "\treturn " + strings.Repeat("宽 word  ", 40) + "\x1b[2J END"
	body := "--- a/x.go\n+++ b/" + strings.Repeat("dir/", 40) + "x.go\n@@ -1,2 +1,2 @@\n" +
		" context\n" +
		"-old " + strings.Repeat("o", 200) + "\n" +
		"+" + long + "\n"
	for _, width := range []int{20, 40, 80, 119, 200} {
		out := Render(body, Options{Width: width, Wrap: true})
		rows := strings.Split(ansi.Strip(out), "\n")
		for i, row := range rows {
			if w := ansi.StringWidth(row); w > width {
				t.Fatalf("width %d: row %d is %d cells: %q", width, i, w, row)
			}
			if strings.Contains(row, "…") {
				t.Fatalf("width %d: row %d was cut: %q", width, i, row)
			}
		}
		// The added line starts at its "+ " sign; its continuation rows
		// follow under a blank gutter, to the end of the diff.
		var joined strings.Builder
		in := false
		for _, row := range rows {
			if i := strings.Index(row, "+ "); i >= 0 && !in && strings.Contains(row, "retu") {
				in = true
				joined.WriteString(row[i+2:])
				continue
			}
			if in {
				joined.WriteString(row)
			}
		}
		want := strings.ReplaceAll(termtext.EscapeControls(long), " ", "")
		if got := strings.ReplaceAll(joined.String(), " ", ""); got != want {
			t.Fatalf("width %d: wrapped line = %q, want %q", width, got, want)
		}
		if !Overflows(body, Options{Width: width}) {
			t.Fatalf("width %d: Overflows missed the long line", width)
		}
		if Overflows(body, Options{Width: width, Wrap: true}) {
			t.Fatalf("width %d: a wrapped render cuts nothing", width)
		}
	}
	short := "--- a/x\n+++ b/x\n@@ -1 +1 @@\n-a\n+b\n"
	if Overflows(short, Options{Width: 80, Indent: "  "}) {
		t.Fatal("a short diff does not overflow")
	}
}

// Intra-line emphasis survives wrapping: each row carries the part of the
// changed span that falls on it.
func TestShiftSpans(t *testing.T) {
	spans := []span{{2, 5}, {8, 12}}
	for _, tc := range []struct {
		offset, n int
		want      []span
	}{
		{0, 4, []span{{2, 4}}},
		{4, 4, []span{{0, 1}}},
		{8, 10, []span{{0, 4}}},
		{12, 5, nil},
	} {
		got := shiftSpans(spans, tc.offset, tc.n)
		if len(got) != len(tc.want) {
			t.Fatalf("shiftSpans(%d, %d) = %v, want %v", tc.offset, tc.n, got, tc.want)
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Fatalf("shiftSpans(%d, %d) = %v, want %v", tc.offset, tc.n, got, tc.want)
			}
		}
	}
}
