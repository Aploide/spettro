package tui

import (
	"strings"

	"github.com/charmbracelet/x/ansi"
)

// textSelection is a mouse drag selection over the rendered frame, in screen
// cell coordinates. A left press arms it (active); the first motion event
// while the button is held starts the drag (dragging), which highlights the
// selected span in the view. On release the plain text under the span is
// copied to the system clipboard via OSC 52, so text can be selected and
// copied without turning mouse capture off (wheel scroll and side panel
// clicks keep working throughout).
type textSelection struct {
	active   bool
	dragging bool
	startX   int
	startY   int
	endX     int
	endY     int
}

// normalized returns the selection endpoints ordered top-to-bottom,
// left-to-right (reading order), with the end cell inclusive.
func (s textSelection) normalized() (x1, y1, x2, y2 int) {
	x1, y1, x2, y2 = s.startX, s.startY, s.endX, s.endY
	if y2 < y1 || (y2 == y1 && x2 < x1) {
		x1, y1, x2, y2 = x2, y2, x1, y1
	}
	return x1, y1, x2, y2
}

// lineSpan returns the [from, to) cell range the selection covers on screen
// row y, following terminal stream-selection semantics: the first line runs
// from the anchor to end-of-line, middle lines are whole, the last line runs
// from column 0 through the end cell. to < 0 means end-of-line.
func (s textSelection) lineSpan(y int) (from, to int, ok bool) {
	x1, y1, x2, y2 := s.normalized()
	if y < y1 || y > y2 {
		return 0, 0, false
	}
	from, to = 0, -1
	if y == y1 {
		from = x1
	}
	if y == y2 {
		to = x2 + 1
	}
	return from, to, true
}

const (
	sgrReverse   = "\x1b[7m"
	sgrUnreverse = "\x1b[27m"
)

// reverseSpan wraps the [from, to) cell range of a styled line in reverse
// video, preserving the surrounding styling. Any SGR reset inside the span
// would cancel the reverse attribute, so reverse is re-asserted after each.
func reverseSpan(line string, from, to int) string {
	w := ansi.StringWidth(line)
	if to < 0 || to > w {
		to = w
	}
	if from >= to || from >= w {
		return line
	}
	left := ansi.Cut(line, 0, from)
	mid := ansi.Cut(line, from, to)
	right := ansi.Cut(line, to, w)
	mid = strings.ReplaceAll(mid, "\x1b[0m", "\x1b[0m"+sgrReverse)
	mid = strings.ReplaceAll(mid, "\x1b[m", "\x1b[m"+sgrReverse)
	return left + sgrReverse + mid + sgrUnreverse + right
}

// applySelectionHighlight overlays the drag selection onto a rendered frame.
func applySelectionHighlight(frame string, sel textSelection) string {
	lines := strings.Split(frame, "\n")
	for y := range lines {
		if from, to, ok := sel.lineSpan(y); ok {
			lines[y] = reverseSpan(lines[y], from, to)
		}
	}
	return strings.Join(lines, "\n")
}

// extractSelection returns the plain text under the selection: ANSI stripped,
// per-line trailing whitespace trimmed, lines joined with newlines.
func extractSelection(frame string, sel textSelection) string {
	lines := strings.Split(frame, "\n")
	var out []string
	for y := range lines {
		from, to, ok := sel.lineSpan(y)
		if !ok {
			continue
		}
		plain := ansi.Strip(lines[y])
		w := ansi.StringWidth(plain)
		if to < 0 || to > w {
			to = w
		}
		if from > w {
			from = w
		}
		var seg string
		if from < to {
			seg = ansi.Cut(plain, from, to)
		}
		out = append(out, strings.TrimRight(seg, " "))
	}
	return strings.Join(out, "\n")
}
