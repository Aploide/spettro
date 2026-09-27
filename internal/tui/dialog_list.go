package tui

import (
	"fmt"
	"image/color"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"spettro/internal/termtext"
)

// listDialog is the layout shared by the full-screen pickers (the model
// selector, the resume list): a bordered box holding a title, optional rows
// above the list (a filter line), a window of the list and the key hints.
//
// It exists so every picker fits the terminal whole. Each picker used to
// reserve a fixed "height-12, at least 4" rows for its list and let its key
// hint wrap, so on a small terminal (40x15) the box came out taller than the
// screen and clampFrame cropped its bottom border and keys; a long row
// (a model id) wrapped and grew it further. Here every row is one line, the
// hints are packed into as few rows as the width allows and counted, and the
// list gets exactly the rows that are left.
//
// Layout, full form (a blank row separates the parts):
//
//	╭──────────────╮  border
//	│              │  padding
//	│ title        │
//	│              │
//	│ head row     │  once per head row, each after a blank row
//	│ ↑ 3 more     │  spacer; names the rows above the window, if any
//	│ list rows    │
//	│ ↓ 5 more     │  spacer; names the rows below the window, if any
//	│ key hints    │  one or more rows
//	│              │  padding
//	╰──────────────╯
//
// When that leaves fewer than minListDialogRows list rows, the compact form
// drops the padding, the blank rows and the spacers (and so the "more"
// markers) to give the list every row it can.
type listDialog struct {
	title  string   // rendered title row
	head   []string // rows between the title and the list
	rows   []string // the whole list, one row per entry
	hints  []string // key hints, such as "enter select"
	border color.Color
}

// minListDialogRows is how many list rows the full form must leave before
// the compact form is used instead.
const minListDialogRows = 3

// Chrome of the two forms, not counting head rows and hint rows: border (2),
// padding (2), the title and the two spacers (the upper one is also the
// blank row under the title when there is no head row); or, compact, the
// border and the title.
const (
	listDialogFullChrome    = 7
	listDialogCompactChrome = 3
)

// hintRows packs the key hints into rows of at most width cells, two spaces
// between hints, so a narrow terminal gets two short rows rather than one
// row wrapped mid-hint. A hint wider than width on its own is cut.
func (d listDialog) hintRows(width int) []string {
	var rows []string
	var cur strings.Builder
	for _, h := range d.hints {
		h = termtext.Fit(h, width)
		switch {
		case cur.Len() == 0:
			cur.WriteString(h)
		case ansi.StringWidth(cur.String())+2+ansi.StringWidth(h) <= width:
			cur.WriteString("  ")
			cur.WriteString(h)
		default:
			rows = append(rows, cur.String())
			cur.Reset()
			cur.WriteString(h)
		}
	}
	if cur.Len() > 0 {
		rows = append(rows, cur.String())
	}
	return rows
}

// layout reports whether the dialog uses the compact form and how many list
// rows it shows, for a terminal height rows tall and content innerW wide.
// It is also how a picker with its own scroll state (resume) learns its
// page size, so paging and drawing agree.
func (d listDialog) layout(innerW, height int) (compact bool, listRows int) {
	hints := len(d.hintRows(innerW))
	full := height - listDialogFullChrome - 2*len(d.head) - hints
	want := min(len(d.rows), minListDialogRows)
	if full >= want {
		return false, max(min(full, len(d.rows)), 1)
	}
	compactRows := height - listDialogCompactChrome - len(d.head) - hints
	return true, max(min(compactRows, len(d.rows)), 1)
}

// windowStart returns the first list row to show so that row cursor is on
// screen, centring it when the list scrolls.
func windowStart(total, visible, cursor int) int {
	if total <= visible {
		return 0
	}
	start := max(cursor-visible/2, 0)
	return min(start, total-visible)
}

// view renders the dialog dialogWidth cells wide for a terminal height rows
// tall, showing the window of the list that starts at start (see
// windowStart; it is clamped here).
func (d listDialog) view(dialogWidth, height, start int) string {
	innerW := dialogInnerWidth(dialogWidth)
	compact, visible := d.layout(innerW, height)
	start = max(min(start, len(d.rows)-visible), 0)
	end := min(start+visible, len(d.rows))

	lines := []string{termtext.Fit(d.title, innerW)}
	for _, h := range d.head {
		if !compact {
			lines = append(lines, "")
		}
		lines = append(lines, termtext.Fit(h, innerW))
	}
	if !compact {
		lines = append(lines, moreMarker("↑", start, innerW))
	}
	for _, row := range d.rows[start:end] {
		lines = append(lines, termtext.Fit(row, innerW))
	}
	if !compact {
		lines = append(lines, moreMarker("↓", len(d.rows)-end, innerW))
	}
	for _, h := range d.hintRows(innerW) {
		lines = append(lines, styleMuted.Render(h))
	}

	vpad := 1
	if compact {
		vpad = 0
	}
	return lipgloss.NewStyle().
		BorderStyle(lipgloss.RoundedBorder()).
		BorderForeground(d.border).
		Width(dialogWidth+2).
		Padding(vpad, 2).
		Render(strings.Join(lines, "\n"))
}

// moreMarker is the spacer row above or below a list window: blank when
// nothing is hidden on that side, else "↑ 3 more" in the muted style.
func moreMarker(arrow string, hidden, width int) string {
	if hidden <= 0 {
		return ""
	}
	return styleMuted.Render(termtext.Fit(fmt.Sprintf("  %s %d more", arrow, hidden), width))
}
