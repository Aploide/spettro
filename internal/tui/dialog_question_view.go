package tui

// Rendering of the ask-user question form. State and key handling live in
// dialog_question.go.
//
// The form draws inside the input box, where the prompt the user is answering
// has always been: the tab strip across the top, the question below it, then
// the answer list. The conversation stays visible above it — a question is
// asked *about* what is on screen, so covering the screen to ask it is exactly
// backwards. Nothing is boxed beyond the input border; the border in this
// feature belongs to the preview pane (task 06).

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"spettro/internal/agent"
)

// questionMinContentH is the smallest conversation pane the question block is
// allowed to leave behind.
const questionMinContentH = 3

// questionBlockBudget is how many lines the form may occupy inside the input
// box. Everything else on screen — header, eyes, separators, status bar, the
// box's own border and agent label, the parallel-agent strip — keeps its space,
// and the conversation pane keeps a minimum. The renderer windows its answer
// list to fit this; without it a question with many options pushes the input
// box off the bottom of the terminal.
func (m Model) questionBlockBudget() int {
	if m.height <= 0 {
		// No WindowSizeMsg yet: nothing is on screen to overflow, and guessing
		// a budget here would truncate a question the terminal can hold.
		return math.MaxInt32
	}
	paneW := m.paneWidth()
	// Measure the chrome instead of hard-coding its line counts: the eyes art
	// and the header change height on their own, and a stale constant here
	// reappears as an off-by-one row past the bottom of the screen.
	fixed := lipgloss.Height(m.viewHeader()) +
		lipgloss.Height(renderEyes(m.mode, m.eyeFrame, m.thinking, paneW)) +
		2 + // the separators bracketing the conversation pane
		lipgloss.Height(m.viewStatusBar(paneW)) +
		3 // the input box's border plus the agent label inside it
	if m.sidePanelWidth() <= 0 {
		if pa := m.renderParallelAgents(); pa != "" {
			fixed += lipgloss.Height(pa)
		}
	}
	return max(m.height-fixed-questionMinContentH, 4)
}

// questionSpacedMinBudget is the block height from which the form can afford a
// blank line between its sections. Below it the rows themselves are worth more
// than the breathing room, so the spacing is the first thing dropped.
const questionSpacedMinBudget = 14

// questionMaxGaps is how many blank lines the spaced layout can insert: one
// between each pair of sections (strip, question, answers, hint).
const questionMaxGaps = 3

// renderQuestionForm draws the whole form block: the strip, then the active
// question page or the Submit page. Its height is what recalcLayout reserves,
// so every line it returns has to be one the terminal can actually hold.
func (m Model) renderQuestionForm() string {
	q := m.pendingQuestion
	if q == nil {
		return ""
	}
	innerW := max(m.paneWidth()-6, 20)
	budget := m.questionBlockBudget()
	spaced := budget >= questionSpacedMinBudget

	var sections [][]string
	var head []string
	if strip := m.renderQuestionStrip(innerW); strip != "" {
		head = append(head, "  "+strip)
	}
	// Answering this one brings up the next, so say so rather than letting a
	// second form appear out of nowhere.
	if n := len(m.questionQueue); n > 0 {
		head = append(head, styleMuted.Render("  ↓ "+plural(n, "more question")+" waiting"))
	}
	if len(head) > 0 {
		sections = append(sections, head)
	}

	// The gaps are reserved before the body is laid out, so the answer list is
	// windowed against the height the block will really occupy.
	gaps := 0
	if spaced {
		gaps = questionMaxGaps
	}
	body := budget - len(head) - gaps
	if q.onSubmitTab() {
		sections = append(sections, m.renderQuestionSubmitPage(innerW, body)...)
	} else {
		sections = append(sections, m.renderQuestionPage(innerW, body)...)
	}

	joined := make([]string, 0, len(sections)*2)
	for i, section := range sections {
		if i > 0 && spaced {
			joined = append(joined, "")
		}
		joined = append(joined, section...)
	}

	// A line wider than the box wraps inside it, and the wrap is a row the
	// layout did not reserve — the input box would then hang off the bottom of
	// the terminal. Cut instead: the tail of a hint is worth less than the frame.
	// Split first: a block like the answer list arrives as one multi-line entry,
	// and cutting that as a single string would eat its newlines with it.
	boxW := max(m.paneWidth()-4, 12)
	out := strings.Split(strings.Join(joined, "\n"), "\n")
	for i, line := range out {
		if ansi.StringWidth(line) > boxW {
			out[i] = ansi.Cut(line, 0, boxW)
		}
	}
	return strings.Join(out, "\n")
}

// renderQuestionPage draws the active question and its answer list as separate
// sections, so the caller can space them out when the terminal has the room.
// budget is how many lines are left for all of them together.
func (m Model) renderQuestionPage(width, budget int) [][]string {
	q := m.pendingQuestion
	question, ok := q.question()
	if !ok {
		return nil
	}

	head := wrapPlainLines("  "+question.Question, width)
	questionLines := len(head)
	head = append(head, wrapPlainLines("  "+strings.TrimSpace(q.form.Context), width)...)

	// Reserve the footer, one option row, and the line the "… N more" marker
	// takes when the list has to be windowed.
	head = clampTextLines(head, max(budget-3, 1), width)
	headLines := m.styleQuestionHead(head, questionLines)
	if question.MultiSelect {
		// Say it where the answer is given, not in the key legend: the rows
		// look the same either way until task 05 gives them checkboxes.
		headLines = append(headLines, styleMuted.Render("  select all that apply"))
	}

	list := m.renderQuestionAnswerList(question, width, budget-len(headLines)-1)
	return [][]string{headLines, list, {styleMuted.Render("  " + m.questionHint())}}
}

// questionRowIndent is the column the labels start at, and the column their
// description lines are indented to: two of margin, the cursor and its space,
// then the widest row number and its ". ".
func questionRowIndent(numWidth int) int { return 4 + numWidth + 2 }

// renderQuestionAnswerList draws the numbered rows of one question inside
// budget lines. The rows below the rule — the chat escape hatch — are pinned to
// the bottom: they are the way out of the form, so a long option list must not
// scroll them away.
func (m Model) renderQuestionAnswerList(question agent.AskUserQuestion, width, budget int) []string {
	q := m.pendingQuestion
	rows := questionRows(question)
	if len(rows) == 0 || budget < 1 {
		return nil
	}
	numWidth := len(strconv.Itoa(len(rows)))
	indent := questionRowIndent(numWidth)
	cursor := min(max(q.cursor[q.tab], 0), len(rows)-1)

	// The rule above the chat row is a block of its own, so the same windowing
	// arithmetic covers it whether it is pinned or scrolling.
	cursorBlock := cursor
	build := func(descriptions bool) (blocks [][]string, total int) {
		blocks = make([][]string, 0, len(rows)+1)
		for i, row := range rows {
			if row.chat {
				blocks = append(blocks, []string{styleMuted.Render("    " + strings.Repeat(glyphs().rule, max(width-4, 4)))})
				total++
				if i <= cursor {
					cursorBlock = i + 1
				}
			}
			block := m.renderQuestionRow(row, i+1, numWidth, indent, width, i == cursor, descriptions)
			blocks = append(blocks, block)
			total += len(block)
		}
		return blocks, total
	}
	flatten := func(blocks [][]string, total int) []string {
		out := make([]string, 0, total)
		for _, block := range blocks {
			out = append(out, block...)
		}
		return out
	}

	blocks, total := build(true)
	if total <= budget {
		return flatten(blocks, total)
	}
	// Descriptions yield to rows before rows yield to scrolling: a list the user
	// can see whole beats a windowed one they have to arrow through, and the
	// labels are what they are choosing between.
	if bare, bareTotal := build(false); bareTotal <= budget {
		return flatten(bare, bareTotal)
	}

	// Too long to show whole: the option list scrolls under the rule and the
	// chat row, which stay pinned to the bottom — the way out of the form must
	// not be the first thing to scroll away. The tail costs two lines, and a
	// windowed list needs two of its own (a row and the "… N more" marker), so
	// below four the answers win instead: esc still leaves the form.
	const tail = 2
	if budget-tail >= 2 {
		out := m.windowedQuestionRows(blocks[:len(blocks)-tail], cursorBlock, budget-tail, indent)
		for _, block := range blocks[len(blocks)-tail:] {
			out = append(out, block...)
		}
		return out
	}
	return m.windowedQuestionRows(blocks, cursorBlock, budget, indent)
}

// windowedQuestionRows renders the run of row blocks around the cursor that
// fits in budget lines, marking what it left out.
func (m Model) windowedQuestionRows(blocks [][]string, cursor, budget, indent int) []string {
	budget = max(budget, 1)
	start, end, hidden := windowQuestionBlocks(blocks, min(cursor, len(blocks)-1), budget)
	out := make([]string, 0, budget)
	for _, block := range blocks[start:end] {
		out = append(out, block...)
	}
	// The marker outranks the tail of the row above it: a windowed page that
	// does not say so reads as the whole list. One row taller than the budget is
	// a line the frame does not have, so trim rather than trust the window — a
	// row can be taller on its own than the page is. A one-line page is the
	// exception: an answer beats the news that there are others.
	if hidden > 0 && budget >= 2 {
		if len(out) >= budget {
			out = out[:budget-1]
		}
		out = append(out, styleMuted.Render(strings.Repeat(" ", indent)+fmt.Sprintf("… %d more (↑ ↓ to scroll)", hidden)))
	}
	return out[:min(len(out), budget)]
}

// renderQuestionRow draws one numbered row: `❯ 3. Label ● recommended`, with
// the option's description wrapped underneath at the label column. The
// recommended marker is drawn on every row state — the cursor moving off the
// agent's suggestion must not erase it.
func (m Model) renderQuestionRow(row questionRow, number, numWidth, indent, width int, focused, descriptions bool) []string {
	g := glyphs()
	q := m.pendingQuestion

	accent := lipgloss.NewStyle().Foreground(m.currentColor())
	// The chevron sits at the left margin, outside the number column, so the
	// numbers stay in one column whichever row is focused.
	chevron := "    "
	if focused {
		chevron = accent.Render("  " + g.cursor + " ")
	}

	label := row.label
	if row.option >= 0 && q.selected[q.tab][row.option] {
		label = g.checked + " " + label
	}
	suffix := ""
	if row.recommended {
		suffix = "  " + g.recommended + " recommended"
	}
	label = truncateLabel(label, max(width-indent-lipgloss.Width(suffix), 8))

	labelStyle := lipgloss.NewStyle().Foreground(colorText)
	if focused {
		labelStyle = accent.Bold(true)
	}
	line := chevron +
		styleMuted.Render(fmt.Sprintf("%*d. ", numWidth, number)) +
		labelStyle.Render(label)
	if suffix != "" {
		line += accent.Render(suffix)
	}
	out := []string{line}

	// The free-text row becomes the field itself while it is being typed in, so
	// the answer is written where the row that offers it sits. Neither the field
	// nor the answer already given is a description: both stay when the page is
	// too short to afford the muted lines.
	if row.custom {
		if q.editing {
			return append(out, indentLines(m.questionCustomField(width-indent), indent)...)
		}
		if text := strings.TrimSpace(q.custom[q.tab]); text != "" {
			return append(out, indentLines([]string{"“" + truncateLabel(text, max(width-indent-2, 8)) + "”"}, indent)...)
		}
	}
	if !descriptions {
		return out
	}
	for _, desc := range questionDescriptionLines(row.description, width-indent) {
		out = append(out, indentLines([]string{styleMuted.Render(desc)}, indent)...)
	}
	return out
}

// questionCustomFieldMaxLines caps the inline text field so a multi-line draft
// cannot squeeze the option list it is sitting in.
const questionCustomFieldMaxLines = 3

// questionCustomField renders the shared textarea narrowed to the column the
// row sits at, so the inline field is the existing free-text flow rather than a
// second text-entry implementation. m is a value receiver, so resizing here
// touches only this render's copy.
func (m Model) questionCustomField(width int) []string {
	m.ta.SetWidth(max(width, 12))
	// The shared input is three rows tall; inline it grows with the draft
	// instead, so an empty field is one line rather than two blank ones under
	// the row it replaced.
	m.ta.SetHeight(min(max(m.ta.LineCount(), 1), questionCustomFieldMaxLines))
	return strings.Split(m.ta.View(), "\n")
}

// questionDescriptionMaxLines bounds one row's description. Descriptions reflow
// rather than clip, but an essay under one option must not push the others off
// the page.
const questionDescriptionMaxLines = 3

// questionDescriptionLines wraps a description to the width left beside the
// label column.
func questionDescriptionLines(desc string, width int) []string {
	if strings.TrimSpace(desc) == "" {
		return nil
	}
	width = max(width, 8)
	return clampTextLines(wrapPlainLines(desc, width), questionDescriptionMaxLines, width)
}

// indentLines shifts a rendered block right to the label column.
func indentLines(lines []string, indent int) []string {
	pad := strings.Repeat(" ", max(indent, 0))
	out := make([]string, len(lines))
	for i, line := range lines {
		out[i] = pad + line
	}
	return out
}

// windowQuestionBlocks keeps the cursor's row visible within budget terminal
// lines, growing the window outwards from it. Unlike windowPickerRows the rows
// are variable-height — a row is its label plus its wrapped description — so
// the window is measured in lines, not rows. When rows are dropped it reserves
// one line for the caller's "… N more" marker.
func windowQuestionBlocks(blocks [][]string, cursor, budget int) (start, end, hidden int) {
	if len(blocks) == 0 {
		return 0, 0, 0
	}
	if cursor < 0 || cursor >= len(blocks) {
		cursor = 0
	}
	budget = max(budget, 1)
	total := 0
	for _, block := range blocks {
		total += len(block)
	}
	if total <= budget {
		return 0, len(blocks), 0
	}
	avail := max(budget-1, 1)

	start, end = cursor, cursor+1
	used := len(blocks[cursor])
	for {
		grew := false
		if end < len(blocks) && used+len(blocks[end]) <= avail {
			used += len(blocks[end])
			end++
			grew = true
		}
		if start > 0 && used+len(blocks[start-1]) <= avail {
			start--
			used += len(blocks[start])
			grew = true
		}
		if !grew {
			return start, end, len(blocks) - (end - start)
		}
	}
}

// styleQuestionHead styles the wrapped question/context block: the question
// reads as the thing being asked, the form's context stays muted under it.
func (m Model) styleQuestionHead(head []string, questionLines int) []string {
	title := lipgloss.NewStyle().Bold(true).Foreground(colorText)
	out := make([]string, 0, len(head))
	for i, line := range head {
		if i < questionLines {
			out = append(out, title.Render(line))
			continue
		}
		out = append(out, styleMuted.Render(line))
	}
	return out
}

// questionHint is the key legend under the answer list; it names only the keys
// that do something on the page being shown.
func (m Model) questionHint() string {
	q := m.pendingQuestion
	switch {
	case q.editing:
		return "enter sends  esc goes back"
	case q.singlePage():
		return "↑↓ or 1-9 pick  enter answers  esc declines"
	default:
		return "↑↓ or 1-9 pick  enter records  tab/←→ switch tab  esc declines"
	}
}

// renderQuestionSubmitPage is the Submit tab. Task 07 turns it into the full
// review page; today it lists what will be sent and what will not.
func (m Model) renderQuestionSubmitPage(width, budget int) [][]string {
	q := m.pendingQuestion
	g := glyphs()

	rows := make([]pickerOption, 0, len(q.form.Questions))
	for i, question := range q.form.Questions {
		mark := g.unchecked
		if q.answered(i) {
			mark = g.checked
		}
		label := mark + " " + question.Header + ": " + questionAnswerSummary(q, i)
		rows = append(rows, pickerOption{Label: truncateLabel(label, max(width-6, 8))})
	}
	// The list is informational, so no row carries a cursor: it is windowed
	// around the first unanswered question, which is the one the user would go
	// back to.
	focus := max(q.firstUnanswered(), 0)
	visible, _, hidden := windowPickerRows(rows, focus, max(budget-3, 1))

	list := make([]string, 0, len(visible)+2)
	for _, row := range visible {
		list = append(list, styleMuted.Render("    "+row.Label))
	}
	if hidden > 0 {
		list = append(list, styleMuted.Render(fmt.Sprintf("    … %d more", hidden)))
	}
	if !q.complete() {
		list = append(list, styleWarn.Render("  "+g.warn+" unanswered questions are sent as skipped"))
	}
	return [][]string{
		{lipgloss.NewStyle().Bold(true).Foreground(colorText).Render("  Send these answers to the agent?")},
		list,
		{styleMuted.Render("  enter sends  tab/←→ back to a question  esc declines")},
	}
}

// questionAnswerSummary renders one question's collected answer for the review
// list.
func questionAnswerSummary(q *questionForm, i int) string {
	if custom := strings.TrimSpace(q.custom[i]); custom != "" {
		return "“" + custom + "”"
	}
	labels := make([]string, 0, len(q.form.Questions[i].Options))
	for j, opt := range q.form.Questions[i].Options {
		if q.selected[i][j] {
			labels = append(labels, opt.Label)
		}
	}
	if len(labels) == 0 {
		return "(not answered)"
	}
	return strings.Join(labels, ", ")
}

// renderQuestionStrip draws the tab strip: one chip per question, then the
// Submit chip, with ← / → affordances marking chips scrolled off either end.
// A one-question form gets no strip at all — there is nowhere else to go, and
// the plain prompt should not grow chrome it cannot use.
func (m Model) renderQuestionStrip(width int) string {
	q := m.pendingQuestion
	if q.singlePage() {
		return ""
	}
	g := glyphs()

	chips := make([]string, 0, q.tabCount())
	for i, question := range q.form.Questions {
		mark := g.unchecked
		if q.answered(i) {
			mark = g.checked
		}
		chips = append(chips, mark+" "+question.Header)
	}
	chips = append(chips, g.submit+" Submit")

	// The arrows are always drawn so the chips never shift sideways as the
	// strip scrolls; only their colour says whether there is more that way.
	const arrowW = 3 // "← " and " →"
	start, end := questionStripWindow(chips, q.tab, max(width-2*arrowW, 10))

	rendered := make([]string, 0, end-start)
	for i := start; i < end; i++ {
		style := styleMuted
		if i == q.tab {
			style = lipgloss.NewStyle().Background(colorSelBg).Foreground(m.currentColor()).Bold(true)
		}
		rendered = append(rendered, style.Render(" "+chips[i]+" "))
	}

	left, right := styleMuted.Render("←"), styleMuted.Render("→")
	if start > 0 {
		left = lipgloss.NewStyle().Foreground(m.currentColor()).Render("←")
	}
	if end < len(chips) {
		right = lipgloss.NewStyle().Foreground(m.currentColor()).Render("→")
	}
	return left + " " + strings.Join(rendered, " ") + " " + right
}

// questionStripWindow picks the run of chips that fits in width while keeping
// the active one visible, growing outwards from it so the neighbouring tabs
// stay in view as long as they fit.
func questionStripWindow(chips []string, active, width int) (start, end int) {
	if len(chips) == 0 {
		return 0, 0
	}
	if active < 0 || active >= len(chips) {
		active = 0
	}
	chipW := func(i int) int { return lipgloss.Width(chips[i]) + 3 } // chip padding + gap

	start, end = active, active+1
	used := chipW(active)
	for {
		grew := false
		if end < len(chips) && used+chipW(end) <= width {
			used += chipW(end)
			end++
			grew = true
		}
		if start > 0 && used+chipW(start-1) <= width {
			start--
			used += chipW(start)
			grew = true
		}
		if !grew {
			return start, end
		}
	}
}
