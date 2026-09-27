package tui

import (
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

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

// Found in review: a list item or quote is wrapped by hangIndent and each
// row is styled on its own, so a bold phrase or a code span that crossed a
// row break lost its style on the rows after the one holding its opening
// escape. A plain paragraph (wrapped later by lipgloss) kept it.
func TestMarkdownListWrapKeepsSpanStyleAcrossRows(t *testing.T) {
	const width = 38
	cases := []struct {
		name, md, span string
		styled         func(sgrState) bool
	}{
		{
			name:   "bold in a list item",
			md:     "- **this bold phrase is long enough to wrap across the row boundary** tail",
			span:   "this bold phrase is long enough to wrap across the row boundary",
			styled: func(s sgrState) bool { return s.bold },
		},
		{
			name:   "bold in a quote",
			md:     "> **this bold phrase is long enough to wrap across the row boundary** tail",
			span:   "this bold phrase is long enough to wrap across the row boundary",
			styled: func(s sgrState) bool { return s.bold },
		},
		{
			name:   "code span in a list item",
			md:     "- run `go test ./internal/tui -run TestSomethingLongEnoughToWrap` now",
			span:   "TestSomethingLongEnoughToWrap",
			styled: func(s sgrState) bool { return s.background },
		},
	}
	for _, tc := range cases {
		rows := strings.Split(renderMarkdown(tc.md, width), "\n")
		if len(rows) < 2 {
			t.Fatalf("%s: expected the item to wrap at %d cells, got %q", tc.name, width, rows)
		}
		// Collect the characters with the style each is drawn in, row by
		// row, as a viewport drawing one row at a time sees them. The quote
		// bar counts as a space, and runs of spaces (the hanging indent,
		// the row break) collapse to one, so the span reads as written.
		var text strings.Builder
		var states []sgrState
		for _, row := range rows {
			for _, c := range append(styledCells(row), styledCell{r: ' '}) {
				if c.r == '│' {
					c.r = ' '
				}
				if c.r == ' ' && strings.HasSuffix(text.String(), " ") {
					continue
				}
				text.WriteRune(c.r)
				states = append(states, c.state)
			}
		}
		runes := []rune(text.String())
		start := strings.Index(text.String(), tc.span)
		if start < 0 {
			t.Fatalf("%s: span %q not found in %q", tc.name, tc.span, text.String())
		}
		start = len([]rune(text.String()[:start]))
		for i := start; i < start+len([]rune(tc.span)); i++ {
			if runes[i] != ' ' && !tc.styled(states[i]) {
				t.Fatalf("%s: %q at cell %d lost the span's style\nrows: %q", tc.name, string(runes[i]), i, rows)
			}
		}
	}
}

// sgrState is the part of a terminal's pen state the markdown tests check.
type sgrState struct{ bold, background bool }

// styledCell is one printed rune and the pen state it is drawn in.
type styledCell struct {
	r     rune
	state sgrState
}

// styledCells replays row's SGR sequences from a reset pen, as a terminal
// drawing the row alone would, and returns each printed rune with its state.
// Extended colours (38/48;5;n and 38/48;2;r;g;b) are skipped whole so their
// numbers are not read as attributes.
func styledCells(row string) []styledCell {
	var out []styledCell
	var st sgrState
	for i := 0; i < len(row); {
		if strings.HasPrefix(row[i:], "\x1b[") {
			end := strings.IndexByte(row[i:], 'm')
			if end < 0 {
				break
			}
			params := strings.Split(row[i+2:i+end], ";")
			for j := 0; j < len(params); j++ {
				switch p := params[j]; p {
				case "", "0":
					st = sgrState{}
				case "1":
					st.bold = true
				case "22":
					st.bold = false
				case "49":
					st.background = false
				case "38", "48", "58":
					if p == "48" {
						st.background = true
					}
					if j+1 < len(params) && params[j+1] == "5" {
						j += 2
					} else if j+1 < len(params) && params[j+1] == "2" {
						j += 4
					}
				default:
					if n, err := strconv.Atoi(p); err == nil && (n >= 40 && n <= 47 || n >= 100 && n <= 107) {
						st.background = true
					}
				}
			}
			i += end + 1
			continue
		}
		r, size := utf8.DecodeRuneInString(row[i:])
		out = append(out, styledCell{r: r, state: st})
		i += size
	}
	return out
}
