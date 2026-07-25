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
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
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
	g := glyphs()

	head := wrapPlainLines("  "+question.Question, width)
	questionLines := len(head)
	head = append(head, wrapPlainLines("  "+strings.TrimSpace(q.form.Context), width)...)

	if q.editing {
		// Prompt line + textarea + footer sit below the question.
		head = clampTextLines(head, max(budget-2-lipgloss.Height(m.ta.View()), 1), width)
		return [][]string{
			m.styleQuestionHead(head, questionLines),
			{styleMuted.Render("  type your answer and press enter:"), m.ta.View()},
			{styleMuted.Render("  " + m.questionHint())},
		}
	}

	// Reserve the footer, one option row, and the line the "… N more" marker
	// takes when the list has to be windowed.
	head = clampTextLines(head, max(budget-3, 1), width)
	headLines := m.styleQuestionHead(head, questionLines)
	if question.MultiSelect {
		// Say it where the answer is given, not in the key legend: the rows
		// look the same either way until task 05 gives them checkboxes.
		headLines = append(headLines, styleMuted.Render("  select all that apply"))
	}

	rows := questionRows(question)
	picker := make([]pickerOption, 0, len(rows))
	labelW := 0
	for i, row := range rows {
		opt := pickerOption{Label: row.label, Separated: row.custom && i > 0}
		switch {
		case row.recommended:
			opt.Badge = g.recommended + " recommended"
		case row.custom && strings.TrimSpace(q.custom[q.tab]) != "":
			opt.Badge = "“" + truncateLabel(q.custom[q.tab], 40) + "”"
		case row.description != "":
			// Task 04 gives descriptions their own muted line; folded in until
			// then so a distinction the agent drew is not lost.
			opt.Badge = row.description
		}
		if row.option >= 0 && q.selected[q.tab][row.option] {
			opt.Label = g.checked + " " + opt.Label
		}
		// The row prefix takes 4 columns and the badge follows the label.
		opt.Label = truncateLabel(opt.Label, max(width-6-lipgloss.Width(opt.Badge), 8))
		if opt.Badge != "" {
			// Only badged rows set the column: padding to a long bare label
			// (the free-text entry, usually) would push the descriptions away
			// from the answers they describe.
			labelW = max(labelW, lipgloss.Width(opt.Label))
		}
		picker = append(picker, opt)
	}
	// Pad the labels to a column so the descriptions line up: ragged badges
	// read as noise beside the answers they belong to.
	for i := range picker {
		if pad := labelW - lipgloss.Width(picker[i].Label); picker[i].Badge != "" && pad > 0 {
			picker[i].Label += strings.Repeat(" ", pad)
		}
	}

	visible, cursor, hidden := windowPickerRows(picker, q.cursor[q.tab], budget-len(headLines)-1)
	list := m.renderQuestionRows(visible, cursor)
	if hidden > 0 {
		list = append(list, styleMuted.Render(fmt.Sprintf("    … %d more (↑ ↓ to scroll)", hidden)))
	}
	return [][]string{headLines, list, {styleMuted.Render("  " + m.questionHint())}}
}

// renderQuestionRows draws the answer list. It is deliberately the question
// form's own renderer rather than the approval picker's: task 04 turns these
// rows into numbered ones with their own description lines, and the approval
// dialogs must not follow them there.
func (m Model) renderQuestionRows(rows []pickerOption, cursor int) []string {
	g := glyphs()
	mc := m.currentColor()
	out := make([]string, 0, len(rows)+1)
	for i, row := range rows {
		if row.Separated {
			out = append(out, styleMuted.Render("    ─────"))
		}
		line := styleMuted.Render("    " + row.Label)
		if i == cursor {
			line = lipgloss.NewStyle().Foreground(mc).Bold(true).Render("  " + g.cursor + " " + row.Label)
		}
		if row.Badge != "" {
			line += styleMuted.Render("  " + row.Badge)
		}
		out = append(out, line)
	}
	return out
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
		return "↑↓ move  enter answers  esc declines"
	default:
		return "↑↓ move  enter records  tab/←→ switch tab  esc declines"
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
