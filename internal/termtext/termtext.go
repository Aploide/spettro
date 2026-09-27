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
// Carriage returns are resolved the way a terminal shows them: a progress
// meter that redraws itself with "\r" (downloads, test runners) leaves only
// its final state on screen, so only the text after the last "\r" is kept.
// A trailing "\r" (a CRLF line ending split on "\n") is simply removed.
func SanitizeLine(s string) string {
	s = strings.TrimRight(s, "\r")
	if i := strings.LastIndexByte(s, '\r'); i >= 0 {
		s = s[i+1:]
	}
	if isPlainPrintable(s) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range ansi.Strip(s) {
		switch {
		case r == '\t':
			b.WriteString(strings.Repeat(" ", TabWidth))
		case r < 0x20, r == 0x7f, r >= 0x80 && r <= 0x9f:
			// A control character has no width and no meaning on screen;
			// dropping it is the only handling that cannot move the cursor.
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// EscapeControls returns s with every control character made visible instead
// of removed: tabs become TabWidth spaces and other C0 controls are written in
// caret notation ("^M" for a carriage return, "^[" for the escape that starts
// a colour sequence, "^?" for DEL); C1 controls are written as "\u0085"-style
// escapes. A trailing "\r" (a CRLF line ending split on "\n") is dropped,
// since it is how the line ends, not what it says.
//
// Use it, not SanitizeLine, for text the user is asked to judge: a command
// waiting for approval or a file change being shown as a diff. SanitizeLine
// shows what a terminal would, and a terminal would let "rm -rf ~ #\recho hi"
// display as "echo hi"; this shows every character that is actually there.
func EscapeControls(s string) string {
	s = strings.TrimRight(s, "\r")
	if isPlainPrintable(s) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 8)
	for _, r := range s {
		switch {
		case r == '\t':
			b.WriteString(strings.Repeat(" ", TabWidth))
		case r < 0x20:
			b.WriteByte('^')
			b.WriteRune(r + 0x40)
		case r == 0x7f:
			b.WriteString("^?")
		case r >= 0x80 && r <= 0x9f:
			fmt.Fprintf(&b, "\\u%04x", r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// isPlainPrintable reports whether s has no byte SanitizeLine would change,
// so the common case (ordinary ASCII or UTF-8 text) skips the rebuild. Bytes
// at or above 0x80 are allowed through here and checked by the slow path only
// when some other byte already forced it; C1 controls (U+0080..U+009F) are
// encoded as 0xC2 0x80..0x9F, which this check catches.
func isPlainPrintable(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < 0x20 || c == 0x7f {
			return false
		}
		if c == 0xc2 && i+1 < len(s) && s[i+1] >= 0x80 && s[i+1] <= 0x9f {
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
func Wrap(s string, width int) []string {
	width = max(width, 1)
	if ansi.StringWidth(s) <= width {
		return []string{s}
	}
	return strings.Split(ansi.Wrap(s, width, ""), "\n")
}
