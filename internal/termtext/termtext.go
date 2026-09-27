// Package termtext makes untrusted text safe to place on a terminal cell grid.
//
// Everything the TUI draws that it did not write itself (tool arguments, tool
// output, file contents in a diff, model text in a preview) can carry bytes
// that a terminal interprets instead of printing: escape sequences that move
// the cursor or change colours, carriage returns that jump back to column 0,
// tabs whose width depends on the terminal's tab stops. Any of those reaching
// the screen raw makes a line occupy a different number of cells than the
// layout measured, which is how a single tool result used to push the frame
// past the terminal edge.
//
// The helpers here turn such text into plain printable runes whose display
// width lipgloss and x/ansi measure exactly, and bound it to a cell budget.
// They work on one line at a time; callers split on "\n" first.
package termtext

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"
)

// TabWidth is the number of spaces a tab becomes. It matches lipgloss's own
// tab conversion, so a line sanitized here renders at the same width whether
// or not it later passes through a lipgloss style.
const TabWidth = 4

// Ellipsis marks text that was cut to fit. It is one cell wide.
const Ellipsis = "…"

// SanitizeLine returns s as plain printable text: ANSI escape sequences are
// removed, tabs become TabWidth spaces, and every other control character is
// dropped. s is expected to be a single line; a "\n" inside it is treated like
// any other control character and dropped.
//
// Bytes that are not valid UTF-8 (a Windows-1252 file, a binary blob) each
// become U+FFFD, the replacement character, before anything else looks at
// the text. Left in place, a lone byte in 0x80..0x9F is a C1 control to the
// terminal and to the renderer: 0x9B starts a CSI sequence that swallows
// the text after it or recolours it, and 0x80 or 0x96 vanish.
//
// Carriage returns are resolved the way a terminal shows them: a progress
// meter that redraws itself with "\r" (downloads, test runners) leaves only
// its final state on screen, so only the text after the last "\r" is kept.
// A trailing "\r" (a CRLF line ending split on "\n") is simply removed.
func SanitizeLine(s string) string {
	s = strings.TrimRight(s, "\r")
	if i := strings.LastIndexByte(s, '\r'); i >= 0 {
		s = s[i+1:]
	}
	if isPlainPrintable(s, modeSanitize, false) {
		return s
	}
	s = strings.ToValidUTF8(s, string(utf8.RuneError))
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range ansi.Strip(s) {
		switch {
		case r == '\t':
			b.WriteString(strings.Repeat(" ", TabWidth))
		case isControl(r):
			// A control character has no width and no meaning on screen;
			// dropping it is the only handling that cannot move the cursor.
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// EscapeControls returns s with every character that would not show as
// itself made visible instead of removed:
//
//   - a tab becomes TabWidth spaces;
//   - other C0 controls are written in caret notation ("^M" for a carriage
//     return, "^[" for the escape that starts a colour sequence, "^?" for
//     DEL);
//   - C1 controls and the characters a terminal draws as nothing or as a
//     plain space (see Hidden) are written as "\u0085"-style escapes
//     ("\U000e0100" past U+FFFF). They print nothing, or a blank that is
//     not a space, but still count: bidi overrides and isolates
//     (U+202A..U+202E, U+2066..U+2069) make a terminal that applies bidi
//     reorder the text around them, zero-width characters and variation
//     selectors hide inside a word (a string of variation selectors can
//     carry a whole payload that decodes to a command), and a no-break
//     space looks like the space that separates two shell words while
//     being part of one;
//   - a letter of another script that is drawn like a Latin one (the
//     Cyrillic small i, U+0456, in "g\u0456thub.com", a fullwidth or
//     mathematical "a") is written as a "\u0456"-style escape when it sits
//     in a word that reads as Latin text, where it can only be posing as
//     the letter it resembles (see lookalike.go); a word wholly in that
//     script is left alone;
//   - a byte that is not valid UTF-8 is written as "\x9b".
//
// A trailing "\r" (a CRLF line ending split on "\n") is dropped, since it is
// how the line ends, not what it says.
//
// Use it, not SanitizeLine, for text the user is asked to judge: a command
// waiting for approval or a file change being shown as a diff. SanitizeLine
// shows what a terminal would, and a terminal would let "rm -rf ~ #\recho hi"
// display as "echo hi"; this shows every character that is actually there.
// The price is that text which legitimately uses one of these characters (an
// emoji joined with U+200D or styled with U+FE0F, French text with no-break
// spaces) shows the escape instead.
//
// Two characters are still shown as something else: a tab as spaces and a
// trailing carriage return as nothing. That keeps a tab-indented diff
// readable in a small dialog; where even those must be told apart, use
// EscapeExact.
func EscapeControls(s string) string {
	return escape(s, false)
}

// EscapeExact is EscapeControls with nothing shown as something else, for a
// view that promises every character (the full approval review, a command
// preview): a tab is written as TabMark followed by spaces up to TabWidth
// cells, so it keeps a tab's width but can no longer pass for spaces, a
// trailing carriage return is written as "^M" like any other, and a literal
// TabMark in s is escaped ("\u21e5") so it cannot pass for a tab. Both
// differences matter to a shell: a "<<-EOF" heredoc ends at a tab-indented
// "EOF" but not at a space-indented one, nor at "EOF\r".
func EscapeExact(s string) string {
	return escape(s, true)
}

// TabMark stands for a tab in text escaped with EscapeExact. It is the
// symbol of the tab key and one cell wide on every terminal (its East Asian
// width is neutral).
const TabMark = "\u21e5"

// EscapeLines applies EscapeExact to every line of multi-line text s,
// keeping the line breaks: for text that leaves the terminal (an editor's
// permission prompt, a chat message) and must show every character there
// too.
func EscapeLines(s string) string {
	if isPlainPrintable(s, modeExact, true) {
		return s
	}
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		lines[i] = EscapeExact(line)
	}
	return strings.Join(lines, "\n")
}

// HasHidden reports whether multi-line text s holds a character that does
// not show as itself (anything EscapeExact would rewrite, other than a tab or
// the "\r" of a CRLF line ending), a look-alike letter posing as Latin
// included. A host that must present s raw, such as an
// editor's side-by-side diff, uses it to decide whether to show an escaped
// copy as well.
func HasHidden(s string) bool {
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && size == 1:
			return true
		case r == '\t', r == '\n':
		case r == '\r':
			if i+1 < len(s) && s[i+1] != '\n' {
				return true
			}
		case isControl(r), Hidden(r):
			return true
		}
		i += size
	}
	return len(posingLookalikes(s)) > 0
}

// escape is EscapeControls, or EscapeExact when exact is set.
func escape(s string, exact bool) string {
	mode := modeEscape
	if exact {
		mode = modeExact
	} else {
		s = strings.TrimRight(s, "\r")
	}
	if isPlainPrintable(s, mode, false) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 8)
	// The words in which a look-alike letter poses as Latin (lookalike.go).
	posing, nextPosing := posingLookalikes(s), 0
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && size == 1:
			fmt.Fprintf(&b, "\\x%02x", s[i])
		case latinLookalike(r) && inSpans(posing, &nextPosing, i):
			writeCodePoint(&b, r)
		case r == '\t' && exact:
			b.WriteString(TabMark)
			b.WriteString(strings.Repeat(" ", TabWidth-1))
		case r == '\t':
			b.WriteString(strings.Repeat(" ", TabWidth))
		case r < 0x20:
			b.WriteByte('^')
			b.WriteRune(r + 0x40)
		case r == 0x7f:
			b.WriteString("^?")
		case isControl(r), Hidden(r), exact && r == tabMarkRune:
			writeCodePoint(&b, r)
		default:
			b.WriteRune(r)
		}
		i += size
	}
	return b.String()
}

// tabMarkRune is TabMark as a rune.
const tabMarkRune = '\u21e5'

// writeCodePoint writes r as "\u202e", or "\U000e0100" past U+FFFF, so the
// escape always has a fixed number of digits and cannot run into the text
// after it.
func writeCodePoint(b *strings.Builder, r rune) {
	if r > 0xffff {
		fmt.Fprintf(b, "\\U%08x", r)
		return
	}
	fmt.Fprintf(b, "\\u%04x", r)
}

// Hidden reports whether r is drawn by a terminal as nothing, or as a blank
// that is not the ASCII space, so that a reader cannot tell it is there or
// cannot tell it from a space:
//
//   - Unicode format characters (general category Cf): bidi controls,
//     zero-width space and joiners, the byte-order mark, the tag characters;
//   - variation selectors (U+FE00..U+FE0F, U+E0100..U+E01EF, the Mongolian
//     ones), which change nothing on screen after most characters and can be
//     strung together to smuggle data;
//   - the other default-ignorable code points: U+034F, the Hangul fillers
//     U+115F, U+1160, U+3164 and U+FFA0, U+17B4..U+17B5 and the unassigned
//     ranges reserved for them;
//   - every space separator other than U+0020 (the no-break space, the en
//     and em spaces, the ideographic space and the rest of category Zs) and
//     the line and paragraph separators U+2028 and U+2029;
//   - U+2800, the blank braille pattern, which is not a space to a program
//     but looks exactly like one.
func Hidden(r rune) bool {
	if r < 0x80 {
		return false
	}
	switch {
	case r == 0x2800:
		return true
	case unicode.Is(unicode.Zs, r), r == 0x2028, r == 0x2029:
		return true
	}
	return unicode.In(r, unicode.Cf, unicode.Variation_Selector, unicode.Other_Default_Ignorable_Code_Point)
}

// isControl reports whether r is a C0 control, DEL or a C1 control: the
// characters a terminal acts on instead of printing.
func isControl(r rune) bool {
	return r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f)
}

// escapeMode is what isPlainPrintable checks text against: the characters
// SanitizeLine, EscapeControls or EscapeExact would rewrite.
type escapeMode int

const (
	modeSanitize escapeMode = iota // control characters
	modeEscape                     // and hidden characters (Hidden)
	modeExact                      // and a literal TabMark
)

// isPlainPrintable reports whether s is valid UTF-8 holding nothing the
// function mode stands for would rewrite, so that it would return s
// unchanged. newlines allows "\n" (EscapeLines works on whole texts). It
// lets the common case, ordinary ASCII, skip the rebuild after one pass over
// the bytes; only text with a byte at or above 0x80 is decoded rune by rune.
func isPlainPrintable(s string, mode escapeMode, newlines bool) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < 0x20 && !(newlines && c == '\n')) || c == 0x7f {
			return false
		}
		if c >= 0x80 {
			return isPlainUnicode(s[i:], mode, newlines)
		}
	}
	return true
}

// isPlainUnicode is the rune-by-rune half of isPlainPrintable, for text that
// is not pure ASCII.
func isPlainUnicode(s string, mode escapeMode, newlines bool) bool {
	if !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if newlines && r == '\n' {
			continue
		}
		if isControl(r) || (mode >= modeEscape && (Hidden(r) || latinLookalike(r))) || (mode == modeExact && r == tabMarkRune) {
			// A look-alike letter only sends the text down the slow path,
			// which decides whether it poses as Latin there.
			return false
		}
	}
	return true
}

// SingleLine sanitizes s and folds it onto one line: every run of whitespace,
// newlines included, becomes one space. It is for labels (a tool call's
// command or path shown in a header row) where a multi-line value would break
// the row apart.
func SingleLine(s string) string {
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		lines[i] = SanitizeLine(line)
	}
	return strings.Join(strings.Fields(strings.Join(lines, " ")), " ")
}

// Fit cuts s to at most width display cells, ending it with Ellipsis when
// anything was removed. ANSI styling in s is preserved. A width below 1
// yields "".
func Fit(s string, width int) string {
	if width < 1 {
		return ""
	}
	if ansi.StringWidth(s) <= width {
		return s
	}
	return ansi.Truncate(s, width, Ellipsis)
}

// FitLeft is Fit for text whose end matters most, such as a file path: it
// keeps the last cells of s and marks the cut at the start ("…/pkg/file.go").
func FitLeft(s string, width int) string {
	if width < 1 {
		return ""
	}
	w := ansi.StringWidth(s)
	if w <= width {
		return s
	}
	return ansi.TruncateLeft(s, w-width+ansi.StringWidth(Ellipsis), Ellipsis)
}

// Wrap breaks s into lines of at most width display cells, at spaces where
// possible and inside a word only when the word alone is wider than width.
// s should already be sanitized; ANSI styling in it is preserved. A width
// below 1 is treated as 1 so the result is always usable.
//
// The escape sequences are only kept, not carried: a style opened before a
// break still applies only to the row that holds its escape once the rows
// are drawn separately. Styled text whose spans can cross a break should be
// wrapped with lipgloss.Wrap, which reopens the style on every row.
func Wrap(s string, width int) []string {
	width = max(width, 1)
	if ansi.StringWidth(s) <= width {
		return []string{s}
	}
	return strings.Split(ansi.Wrap(s, width, ""), "\n")
}

// HardWrap breaks plain text s into rows of at most width display cells,
// cutting inside words rather than at spaces. Unlike Wrap it keeps every
// character: no space is dropped at a break and none is added, so joining
// the rows gives back s exactly. That is what the approval review needs,
// where a run of spaces in a command or a line of a file is part of what the
// user approves.
//
// A space next to a break would be the one character of a row nobody can
// see: at the end of a row it looks like nothing, and "./build/a/ ~/" (which
// deletes the home directory) would read like "./build/a/~/" (which does
// not). So a row breaks between two non-space characters, backing up within
// the row when the cell where it fills up is next to a space. Only when the
// row has no such place (a row of spaces, a run of one-letter words) does it
// break just before a space, so the space starts the next row; and only a
// row of nothing but spaces is cut where it fills up.
//
// Rows break between grapheme clusters, never inside one; a cluster wider
// than width (a wide character at width 1) takes a row of its own. Nor does a
// row break inside an escape EscapeControls or EscapeExact wrote ("^M",
// "\x9b", "\u202e", "\U000e0100") when the row can hold it whole: split, "\u"
// at the end of one row and "202e" at the start of the next would read as two
// things. s must not contain terminal escape sequences (escape it with
// EscapeExact first). A width below 1 is treated as 1; an empty s yields one
// empty row.
func HardWrap(s string, width int) []string {
	width = max(width, 1)
	if ansi.StringWidth(s) <= width {
		return []string{s}
	}
	var rows []string
	// row holds the clusters of the row being filled, from rowStart.
	var row []wrapCluster
	rowStart, used := 0, 0
	for pos := 0; pos < len(s); {
		// The same clusters and widths ansi.StringWidth measures, so a row
		// is never wider than the layout thinks.
		cluster, w := ansi.FirstGraphemeCluster(s[pos:], ansi.GraphemeWidth)
		if n := escapeTokenLen(s[pos:]); n > 0 && n <= width {
			// An escape is ASCII: its length is its width.
			cluster, w = s[pos:pos+n], n
		}
		next := wrapCluster{pos: pos, w: w, space: cluster == " "}
		for used+w > width && len(row) > 0 {
			k := wrapBreak(row, next.space)
			end := pos
			if k < len(row) {
				end = row[k].pos
			}
			rows = append(rows, s[rowStart:end])
			rowStart = end
			n := copy(row, row[k:])
			row = row[:n]
			used = 0
			for _, c := range row {
				used += c.w
			}
		}
		row = append(row, next)
		used += w
		pos += max(len(cluster), 1)
	}
	return append(rows, s[rowStart:])
}

// EscapeLen is the length in bytes of the escape EscapeControls or
// EscapeExact wrote at the start of s ("^M", "\x9b", "\u202e",
// "\U000e0100"), or 0 when s does not start with one. A caller that cuts
// escaped text into pieces (a chat message, a row) uses it to keep each
// escape in one piece: split, "\u" and "202e" would read as two things.
func EscapeLen(s string) int {
	return escapeTokenLen(s)
}

// escapeTokenLen is the length of the escape s starts with, as
// EscapeControls and EscapeExact write them: "^" and one character of
// @A-Z[\]^_? (caret notation), "\x" and two hex digits, "\u" and four,
// "\U" and eight. It is 0 when s does not start with one. Text that happens
// to look like an escape is kept together too, which costs nothing.
func escapeTokenLen(s string) int {
	if len(s) < 2 {
		return 0
	}
	switch {
	case s[0] == '^' && (s[1] >= '@' && s[1] <= '_' || s[1] == '?'):
		return 2
	case s[0] != '\\':
		return 0
	}
	var digits int
	switch s[1] {
	case 'x':
		digits = 2
	case 'u':
		digits = 4
	case 'U':
		digits = 8
	default:
		return 0
	}
	if len(s) < 2+digits {
		return 0
	}
	for _, c := range []byte(s[2 : 2+digits]) {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return 0
		}
	}
	return 2 + digits
}

// wrapCluster is one grapheme cluster of the row HardWrap is filling.
type wrapCluster struct {
	pos   int  // byte offset in the text
	w     int  // cells
	space bool // an ASCII space
}

// wrapBreak picks where HardWrap ends a full row: the number of the row's
// clusters that stay on it (the rest move to the next row, in front of the
// cluster that did not fit, which is a space when nextSpace is set). See
// HardWrap for the order of preference. It is at least 1, so every row
// holds something.
func wrapBreak(row []wrapCluster, nextSpace bool) int {
	after := func(k int) bool { // the cluster after the first k
		if k < len(row) {
			return row[k].space
		}
		return nextSpace
	}
	for k := len(row); k >= 1; k-- {
		if !row[k-1].space && !after(k) {
			return k
		}
	}
	for k := len(row); k >= 1; k-- {
		if !row[k-1].space {
			return k
		}
	}
	return len(row)
}
