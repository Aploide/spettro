package tui

// The option preview pane and the notes affordance of the ask-user form. The
// rest of the form's rendering lives in dialog_question_view.go.
//
// A preview is a preformatted sketch the agent wrote — an ASCII layout, a
// snippet, a config fragment. It is shown verbatim: never markdown-rendered,
// never reflowed, and never handed to the terminal without being stripped of
// whatever escape sequences it happens to carry.

import (
	"fmt"
	"strconv"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"spettro/internal/agent"
)

const (
	// questionPreviewGap is the blank column between the option list and the
	// pane, so the pane's border never touches a label.
	questionPreviewGap = 2
	// questionPreviewMinListW and questionPreviewMinPaneW are the narrowest each
	// column is worth drawing at. Below either one the side-by-side layout is
	// dropped rather than squeezed: a two-column page where neither column can
	// be read is worse than one column that can.
	questionPreviewMinListW = 30
	questionPreviewMinPaneW = 26
	// questionPreviewListSharePct caps the list's share of the width, so a
	// question with long labels cannot squeeze the pane down to its border.
	questionPreviewListSharePct = 45
	// questionPreviewMinInnerW keeps a snug box wide enough for its own
	// truncation footer, so a narrow sketch does not report "… 12 more lines"
	// in a box too small to say it.
	questionPreviewMinInnerW = 16
	// questionPreviewMinLines is the shortest pane worth drawing: two border
	// rows, a line of the preview and the row its truncation footer would need.
	// A box with room for one line can only ever show a cut sketch as if it were
	// the whole one.
	questionPreviewMinLines = 4
	// questionPreviewStackedMaxLines caps the pane in the stacked fallback,
	// where every line it takes is a line the option list does not get, and
	// questionPreviewStackedMinListLines is what the list keeps whatever the
	// preview would like: two options and the way out of the form.
	questionPreviewStackedMaxLines     = 8
	questionPreviewStackedMinListLines = 4
	// questionNotesMinBudget is the body height from which the notes hint is
	// worth a row. Below it the answers are worth more; the key legend still
	// names `n`, and a note already written is shown whatever the budget.
	questionNotesMinBudget = 8
)

// focusedPreview is the preview of the option the cursor is on, or empty for a
// row that has none — the client's own rows (free text, Submit, chat) never do.
// The pane follows the cursor, so a mixed question collapses it rather than
// leaving a hole where the last preview was.
func (m Model) focusedPreview(question agent.AskUserQuestion) string {
	q := m.pendingQuestion
	rows := questionRows(question)
	if len(rows) == 0 {
		return ""
	}
	row := rows[min(max(q.cursor[q.tab], 0), len(rows)-1)]
	if row.option < 0 || row.option >= len(question.Options) {
		return ""
	}
	return strings.Trim(question.Options[row.option].Preview, "\n")
}

// questionListWidth is the width the answer list would like: its widest row
// drawn whole, descriptions included, since those are what a narrow column
// makes unreadable first. The pane takes what is left over, so a question with
// short labels gives it more room without the list ever going below the
// minimum. It is measured from the question rather than from the focused
// option, so the list does not reflow as the cursor moves between options
// whose previews are different sizes.
func (m Model) questionListWidth(question agent.AskUserQuestion) (labels, full int) {
	rows := questionRows(question)
	numWidth := 1
	for _, row := range rows {
		numWidth = max(numWidth, len(strconv.Itoa(row.number)))
	}
	indent := questionRowIndent(numWidth)
	for _, row := range rows {
		w := indent + lipgloss.Width(row.label)
		if box := m.questionRowCheckbox(row); box != "" {
			w += lipgloss.Width(box) + 1
		}
		if row.recommended {
			w += lipgloss.Width("  " + glyphs().recommended + " recommended")
		}
		labels = max(labels, w)
		full = max(full, w, indent+lipgloss.Width(row.description))
	}
	return labels, full
}

// questionPreviewLayout splits width between the option list and the preview
// pane. A zero pane width means the terminal is too narrow to put them side by
// side; the caller stacks the pane under the list instead, or drops it.
func questionPreviewLayout(width, labels, full int) (listW, paneW int) {
	// The share is what the list gets for the asking. A truncated label is worse
	// than a wrapped description, though, so labels that do not fit within it
	// take the room anyway — up to what the pane needs to exist at all.
	share := max(width*questionPreviewListSharePct/100,
		min(labels, width-questionPreviewGap-questionPreviewMinPaneW))
	listW = min(max(full, questionPreviewMinListW), share)
	paneW = width - listW - questionPreviewGap
	if listW < questionPreviewMinListW || paneW < questionPreviewMinPaneW {
		return width, 0
	}
	return listW, paneW
}

// questionPreviewPane draws the bordered box, matching the mention picker's
// rounded frame. budget is the whole box including its border; nil comes back
// when there is no room for one.
func (m Model) questionPreviewPane(preview string, width, budget int) []string {
	if width < questionPreviewMinPaneW || budget < questionPreviewMinLines {
		return nil
	}
	// The frame costs two columns of border and two of padding, and two rows of
	// border.
	content := m.questionPreviewLines(preview, max(width-4, 8), budget-2)
	if len(content) == 0 {
		return nil
	}
	// A sketch narrower than the column gets a box its own size rather than a
	// field of empty border: the frame is there to say where the preview ends.
	inner := questionPreviewMinInnerW
	for _, line := range content {
		inner = max(inner, ansi.StringWidth(line))
	}
	box := lipgloss.NewStyle().
		BorderStyle(lipgloss.RoundedBorder()).
		BorderForeground(colorBorder).
		Width(min(inner+4, width)).
		PaddingLeft(1).PaddingRight(1).
		Render(strings.Join(content, "\n"))
	return strings.Split(box, "\n")
}

// questionPreviewLines is the preview clipped to the pane: sanitised, cut to
// width rather than wrapped, and capped in height with a muted footer saying
// what was left out. The result is memoised on the form — the pane redraws on
// every keystroke, and this pass reads the whole preview however little of it
// fits on screen.
func (m Model) questionPreviewLines(preview string, width, maxLines int) []string {
	q := m.pendingQuestion
	key := fmt.Sprintf("%d:%d:%d:%d", q.tab, q.cursor[q.tab], width, maxLines)
	if key == q.previewKey && q.previewLines != nil {
		return q.previewLines
	}
	lines := clipPreviewLines(preview, width, maxLines)
	q.previewKey, q.previewLines = key, lines
	return lines
}

// clipPreviewLines does the work questionPreviewLines caches.
func clipPreviewLines(preview string, width, maxLines int) []string {
	if strings.TrimSpace(preview) == "" || width < 1 || maxLines < 1 {
		return nil
	}
	g := glyphs()
	raw := strings.Split(strings.ReplaceAll(preview, "\r\n", "\n"), "\n")

	// One line of the cap is spent on the footer, so the user is told the sketch
	// continues rather than reading a cut one as the whole thing. A cap of one
	// leaves no room for that: there the line itself is worth more.
	keep := len(raw)
	var footer string
	if len(raw) > maxLines {
		keep = maxLines
		if maxLines >= 2 {
			keep = maxLines - 1
			footer = fmt.Sprintf("… %d more lines", len(raw)-keep)
		}
	}

	out := make([]string, 0, keep+1)
	for _, line := range raw[:keep] {
		line = sanitizePreviewLine(line)
		if ansi.StringWidth(line) > width {
			line = ansi.Cut(line, 0, max(width-1, 1)) + g.clip
		}
		out = append(out, line)
	}
	if footer != "" {
		out = append(out, styleMuted.Render(truncateLabel(footer, width)))
	}
	return out
}

// sanitizePreviewLine makes one line of agent-supplied text safe to print. The
// preview is model output: it can carry escape sequences, and a cursor-moving
// or colour-setting one reaching the terminal raw would draw outside the pane
// the layout budgeted for. Tabs become spaces so the sketch's own alignment
// survives whatever tab stop the terminal uses.
func sanitizePreviewLine(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range ansi.Strip(s) {
		switch {
		case r == '\t':
			b.WriteString("    ")
		case r < 0x20, r == 0x7f, r >= 0x80 && r <= 0x9f:
			// A control character has no width and no meaning here; dropping it
			// is the only handling that cannot move the cursor.
		default:
			b.WriteRune(r)
		}
	}
	return strings.TrimRight(b.String(), " ")
}

// blockWidth is the width of the widest line of a rendered block.
func blockWidth(lines []string) int {
	widest := 0
	for _, line := range lines {
		widest = max(widest, ansi.StringWidth(line))
	}
	return widest
}

// questionColumns lays the answer list and the preview column side by side.
// The list is clipped and padded to its own column so the pane's left border
// stays in one place whatever the row beside it happens to be — a windowed
// list's "… N more" marker is wider than the labels above it.
func questionColumns(list, pane []string, listW, budget int) []string {
	height := min(max(len(list), len(pane)), max(budget, 1))
	out := make([]string, 0, height)
	for i := range height {
		left, right := "", ""
		if i < len(list) {
			left = list[i]
		}
		if i < len(pane) {
			right = pane[i]
		}
		if right == "" {
			out = append(out, left)
			continue
		}
		if w := ansi.StringWidth(left); w > listW {
			left = ansi.Cut(left, 0, listW)
		}
		out = append(out, left+strings.Repeat(" ", listW+questionPreviewGap-min(ansi.StringWidth(left), listW))+right)
	}
	return out
}

// --- notes ---

// questionNotesRows is the affordance the mockup puts under the pane: what `n`
// does, or the note it already attached. It costs a row, so on a short page it
// yields to the answers — unless the user has written a note, which must not
// vanish from the page it belongs to.
func (m Model) questionNotesRows(width, budget int) []string {
	q := m.pendingQuestion
	if q.notesEditing {
		return nil // the field itself is on screen
	}
	note := strings.TrimSpace(q.notes[q.tab])
	if budget < questionPreviewMinLines || (note == "" && budget < questionNotesMinBudget) {
		return nil
	}
	text := "Notes: press n to add notes"
	if note != "" {
		text = "Notes: “" + note + "” — n to edit"
	}
	return []string{styleMuted.Render("  " + truncateLabel(text, max(width-2, 8)))}
}

// questionNoteField is the note being typed. It spans the width under the
// answer list rather than sharing the pane's column: it is a sentence, and a
// sentence typed in a 30-column gutter is a worse experience than one typed
// where the rest of the form's text entry happens.
func (m Model) questionNoteField(width int) []string {
	out := []string{styleMuted.Render("  Notes:")}
	return append(out, indentLines(m.questionCustomField(width-2), 2)...)
}
