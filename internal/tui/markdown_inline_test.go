package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// Found by looking at a VHS screenshot of a streamed reply: a bold span
// followed by a link on the same line showed "1mbold" and a stray "[".
func TestInlineMarkdownBoldThenLinkLeavesNoEscapeDebris(t *testing.T) {
	out := renderInlineMarkdown("Here is a **bold** claim and a [link](https://example.com/a).")
	if got, want := ansi.Strip(out), "Here is a bold claim and a link (https://example.com/a)."; got != want {
		t.Fatalf("plain text = %q, want %q (raw %q)", got, want, out)
	}
}

func TestInlineMarkdownMarkupInsideCodeAndLinkTextIsHandled(t *testing.T) {
	cases := map[string]string{
		"`**not bold**` then **bold**":   " **not bold**  then bold",
		"[**bold link**](u) and *it*":    "bold link (u) and it",
		"**bold with `code` inside** ok": "bold with  code  inside ok",
	}
	for in, want := range cases {
		if got := ansi.Strip(renderInlineMarkdown(in)); got != want {
			t.Errorf("%q: plain text = %q, want %q", in, got, want)
		}
	}
}

func TestInlineMarkdownIntrawordUnderscoresStayLiteral(t *testing.T) {
	for _, in := range []string{
		"/Users/me/projects/file_with_a_long_name.go",
		"set max_retries and retry_delay_ms",
		"__init__ is a dunder",
		"a lone _ underscore",
	} {
		out := renderInlineMarkdown(in)
		if ansi.Strip(out) != in {
			t.Errorf("%q rendered as %q", in, ansi.Strip(out))
		}
		if strings.Contains(out, "\x1b[3m") {
			t.Errorf("%q: underscores inside words turned on italics: %q", in, out)
		}
	}
}

func TestInlineMarkdownUnderscoreEmphasis(t *testing.T) {
	cases := map[string]string{
		"an _emphasized_ word":       "an emphasized word",
		"(_quoted_) and _two words_": "(quoted) and two words",
		"_snake_case_ inside":        "snake_case inside",
	}
	for in, want := range cases {
		out := renderInlineMarkdown(in)
		if got := ansi.Strip(out); got != want {
			t.Errorf("%q: plain text = %q, want %q", in, got, want)
		}
		if !strings.Contains(out, "\x1b[3m") {
			t.Errorf("%q: no italics in %q", in, out)
		}
	}
}

func TestInlineMarkdownSpacedAsterisksStayLiteral(t *testing.T) {
	in := "2 * 3 * 4 and a ** b ** c"
	if got := ansi.Strip(renderInlineMarkdown(in)); got != in {
		t.Fatalf("plain text = %q, want %q", got, in)
	}
}

// Wrapped list items hang under their text and nested items keep their
// indent; a wrapped quote keeps its bar on every row. Before, the rows were
// wrapped later by the transcript and fell back to column 0.
func TestMarkdownListAndQuoteWrapWithHangingIndent(t *testing.T) {
	const width = 32
	md := "1. first item\n" +
		"   - nested bullet that is long enough to wrap at this width for sure\n" +
		"> a blockquote that is also long enough to wrap across rows"
	rows := strings.Split(ansi.Strip(renderMarkdown(md, width)), "\n")
	for i, row := range rows {
		if w := ansi.StringWidth(row); w > width {
			t.Fatalf("row %d is %d cells wide (max %d): %q", i, w, width, row)
		}
	}
	if !strings.HasPrefix(rows[1], "     • nested") {
		t.Fatalf("nested bullet not indented under its parent: %q", rows[1])
	}
	var sawHang, quoteRows int
	for _, row := range rows[2:] {
		switch {
		case strings.HasPrefix(row, "│ "):
			quoteRows++
		case strings.HasPrefix(row, "       ") && strings.TrimSpace(row) != "":
			sawHang++
		default:
			t.Fatalf("row lost its indent or quote bar: %q\nall rows:\n%s", row, strings.Join(rows, "\n"))
		}
	}
	if sawHang == 0 || quoteRows < 2 {
		t.Fatalf("expected wrapped list rows and a wrapped quote, got:\n%s", strings.Join(rows, "\n"))
	}
}
