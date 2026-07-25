package tui_test

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"spettro/internal/agent"
	"spettro/internal/tui"
)

// oneQuestion builds the single-question form the flat ask-user payload
// normalises to — the shape almost every question still arrives in.
func oneQuestion(question string, allowCustom bool, options ...agent.AskUserOption) agent.AskUserForm {
	return agent.AskUserForm{Questions: []agent.AskUserQuestion{{
		Header:      question,
		Question:    question,
		Options:     options,
		AllowCustom: allowCustom || len(options) == 0,
	}}}
}

func opts(labels ...string) []agent.AskUserOption {
	out := make([]agent.AskUserOption, 0, len(labels))
	for _, l := range labels {
		out = append(out, agent.AskUserOption{Label: l})
	}
	return out
}

// twoQuestions is the multi-question form the tab strip exists for.
func twoQuestions() agent.AskUserForm {
	return agent.AskUserForm{Questions: []agent.AskUserQuestion{
		{Header: "Focus area", Question: "Where should I start?", Options: opts("Parser", "Renderer")},
		{Header: "Panel layout", Question: "How should the panel sit?", Options: opts("Left", "Right"), AllowCustom: true},
	}}
}

func openForm(t *testing.T, form agent.AskUserForm) tui.Model {
	t.Helper()
	m := tui.NewModelForTesting()
	m.MarkReadyAndTrustedForTesting()
	m.SetDimensionsForTesting(100, 40)
	m.SetPendingAskUserFormForTesting(form)
	return m.RecalcLayoutForTesting()
}

func pressQuestion(t *testing.T, m tui.Model, keys ...tea.KeyPressMsg) tui.Model {
	t.Helper()
	for _, k := range keys {
		next, _ := m.UpdateQuestionForTesting(k)
		m = next.(tui.Model)
	}
	return m
}

func key(s string) tea.KeyPressMsg {
	switch s {
	case "up":
		return tea.KeyPressMsg{Code: tea.KeyUp}
	case "down":
		return tea.KeyPressMsg{Code: tea.KeyDown}
	case "left":
		return tea.KeyPressMsg{Code: tea.KeyLeft}
	case "right":
		return tea.KeyPressMsg{Code: tea.KeyRight}
	case "tab":
		return tea.KeyPressMsg{Code: tea.KeyTab}
	case "shift+tab":
		return tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift}
	case "enter":
		return tea.KeyPressMsg{Code: tea.KeyEnter}
	case "esc":
		return tea.KeyPressMsg{Code: tea.KeyEscape}
	}
	return tea.KeyPressMsg{Code: rune(s[0]), Text: s}
}

// --- the modal shell ---

// The question routes through the modal table, not through an ad-hoc
// interception in updateMain: a question raised mid-run blocks a tool call, so
// it must not be buried by an incidental picker the user left open.
func TestQuestionModal_OutranksSelectorAndRoutesKeys(t *testing.T) {
	m := openForm(t, twoQuestions())
	m = m.OpenSelectorForTesting()

	view := m.ViewForTesting()
	if !strings.Contains(view, "Where should I start?") {
		t.Fatalf("the question must stay on screen while a picker is open:\n%s", view)
	}
	// The key went to the question, not to the selector behind it.
	m = pressQuestion(t, m, key("right"))
	if got := m.QuestionTabForTesting(); got != 1 {
		t.Fatalf("expected the form to handle navigation, tab = %d", got)
	}
}

// The form answers a question about the conversation, so the conversation has
// to stay readable behind it: it draws in the input box, not over the screen.
func TestQuestionModal_KeepsTheConversationVisible(t *testing.T) {
	m := tui.NewModelForTesting()
	m.MarkReadyAndTrustedForTesting()
	m.SetDimensionsForTesting(100, 40)
	m.AddMessageForTesting(tui.ChatMessage{Role: tui.RoleAssistant, Content: "earlier answer from the agent"})
	m.SetPendingAskUserFormForTesting(twoQuestions())
	m = m.RecalcLayoutForTesting().RefreshViewportForTesting()

	view := m.ViewForTesting()
	if !strings.Contains(view, "earlier answer from the agent") {
		t.Fatalf("the transcript must stay visible while a question is open:\n%s", view)
	}
	if !strings.Contains(view, "Where should I start?") {
		t.Fatalf("the question must render in the input box:\n%s", view)
	}
}

func TestQuestionModal_TabStripShowsHeadersAndSubmit(t *testing.T) {
	restore := tui.UseASCIIGlyphsForTesting(false)
	defer restore()

	m := openForm(t, twoQuestions())
	strip := m.QuestionStripForTesting()
	for _, want := range []string{"Focus area", "Panel layout", "Submit", "○"} {
		if !strings.Contains(strip, want) {
			t.Fatalf("strip is missing %q:\n%s", want, strip)
		}
	}
	if strings.Contains(strip, "●") {
		t.Fatalf("nothing is answered yet, so no chip may be checked:\n%s", strip)
	}
}

// Answering flips the tab's glyph and nothing else: the cursor stays where the
// user put it, because revising an answer must not mean arrowing back to it.
func TestQuestionModal_AnsweringDoesNotAdvanceTheTab(t *testing.T) {
	restore := tui.UseASCIIGlyphsForTesting(false)
	defer restore()

	m := openForm(t, twoQuestions())
	m = pressQuestion(t, m, key("down"), key("enter"))

	if got := m.QuestionTabForTesting(); got != 0 {
		t.Fatalf("answering must leave the active tab alone, tab = %d", got)
	}
	if got := m.QuestionCursorForTesting(0); got != 1 {
		t.Fatalf("the cursor must stay on the answered row, cursor = %d", got)
	}
	if !m.QuestionAnsweredForTesting(0) {
		t.Fatal("the question should be recorded as answered")
	}
	if strip := m.QuestionStripForTesting(); !strings.Contains(strip, "● Focus area") {
		t.Fatalf("the answered chip must be checked:\n%s", strip)
	}
}

// Leaving a tab and coming back to it must cost nothing: cursor, selection and
// typed text are all per question.
func TestQuestionModal_TabSwitchPreservesPerQuestionState(t *testing.T) {
	m := openForm(t, twoQuestions())
	m = pressQuestion(t, m, key("down"), key("enter")) // answer Q1 with "Renderer"
	m = pressQuestion(t, m, key("tab"))                // to Q2
	// Q2's free-text entry is the third row (two options + the custom row).
	m = pressQuestion(t, m, key("down"), key("down"), key("enter"))
	m = pressQuestion(t, m, key("h"), key("i"), key("enter"))

	if got := m.QuestionCustomForTesting(1); got != "hi" {
		t.Fatalf("typed answer lost, custom = %q", got)
	}
	m = pressQuestion(t, m, key("shift+tab")) // back to Q1
	if got := m.QuestionTabForTesting(); got != 0 {
		t.Fatalf("shift+tab should move back one tab, tab = %d", got)
	}
	if got := m.QuestionCursorForTesting(0); got != 1 {
		t.Fatalf("Q1 cursor was clobbered by the visit to Q2, cursor = %d", got)
	}
	m = pressQuestion(t, m, key("tab"))
	if got := m.QuestionCursorForTesting(1); got != 2 {
		t.Fatalf("Q2 cursor was clobbered, cursor = %d", got)
	}
	if got := m.QuestionCustomForTesting(1); got != "hi" {
		t.Fatalf("Q2 typed answer was clobbered, custom = %q", got)
	}
}

// tab from the last question lands on Submit, and Submit sends every answer at
// once — including the ones the user never filled in, marked as skipped.
func TestQuestionModal_SubmitTabSendsAnswersAndMarksSkipped(t *testing.T) {
	m := tui.NewModelForTesting()
	m.MarkReadyAndTrustedForTesting()
	m.SetDimensionsForTesting(100, 40)
	m.SetThinkingForTesting(true)
	m, handle := m.DeliverAskUserForTesting(twoQuestions())

	m = pressQuestion(t, m, key("enter"))           // answer Q1 with "Parser"
	m = pressQuestion(t, m, key("tab"), key("tab")) // Q2, then Submit
	if got := m.QuestionTabForTesting(); got != 2 {
		t.Fatalf("tab from the last question must land on Submit, tab = %d", got)
	}
	if view := m.ViewForTesting(); !strings.Contains(view, "skipped") {
		t.Fatalf("the submit page must warn about unanswered questions:\n%s", view)
	}

	m = pressQuestion(t, m, key("enter"))
	answers, err, ok := handle.Answered()
	if !ok || err != nil {
		t.Fatalf("form should have been submitted, got (%v, %v)", err, ok)
	}
	if len(answers) != 2 {
		t.Fatalf("expected one answer per question, got %d", len(answers))
	}
	if len(answers[0].Selected) != 1 || answers[0].Selected[0] != "Parser" {
		t.Fatalf("first answer = %+v", answers[0])
	}
	if !answers[1].Skipped {
		t.Fatalf("the unanswered question must come back skipped, got %+v", answers[1])
	}
	if m.HasPendingAskUserForTesting() {
		t.Fatal("the form should be closed after submitting")
	}
}

// A narrow terminal cannot show every chip, so the strip scrolls to keep the
// active one visible and marks the hidden ones with the arrows.
func TestQuestionModal_StripScrollsToKeepActiveChipVisible(t *testing.T) {
	form := agent.AskUserForm{Questions: []agent.AskUserQuestion{
		{Header: "Very long first header", Question: "One?", Options: opts("a", "b")},
		{Header: "Very long second header", Question: "Two?", Options: opts("a", "b")},
		{Header: "Very long third header", Question: "Three?", Options: opts("a", "b")},
		{Header: "Very long fourth header", Question: "Four?", Options: opts("a", "b")},
	}}
	m := tui.NewModelForTesting()
	m.MarkReadyAndTrustedForTesting()
	m.SetDimensionsForTesting(48, 24)
	m.SetPendingAskUserFormForTesting(form)
	m = m.RecalcLayoutForTesting()

	if strip := m.QuestionStripForTesting(); strings.Contains(strip, "fourth") {
		t.Fatalf("48 columns cannot hold every chip:\n%s", strip)
	}
	m = pressQuestion(t, m, key("right"), key("right"), key("right"))
	strip := m.QuestionStripForTesting()
	if !strings.Contains(strip, "fourth") {
		t.Fatalf("the active chip must be scrolled into view:\n%s", strip)
	}
	if lipgloss.Width(strip) > 48 {
		t.Fatalf("the strip is %d columns wide on a 48-column terminal:\n%s", lipgloss.Width(strip), strip)
	}
	if !strings.Contains(strip, "←") {
		t.Fatalf("hidden chips to the left must be signalled:\n%s", strip)
	}
}

// A plain one-question prompt gains no chrome it cannot use: no tabs, no
// Submit chip, and answering it sends the form.
func TestQuestionModal_SingleQuestionHasNoStrip(t *testing.T) {
	m := openForm(t, oneQuestion("Which database?", false, opts("Postgres", "SQLite")...))
	if strip := m.QuestionStripForTesting(); strip != "" {
		t.Fatalf("a one-question form must render without the strip:\n%s", strip)
	}
	if view := m.ViewForTesting(); strings.Contains(view, "Submit") {
		t.Fatalf("no Submit chip on a one-question form:\n%s", view)
	}
}

// esc declines the whole form. Nothing is partially delivered: the model is
// told the user declined.
func TestQuestionModal_EscDeclinesTheWholeForm(t *testing.T) {
	m := tui.NewModelForTesting()
	m.SetDimensionsForTesting(100, 40)
	m.SetThinkingForTesting(true)
	m, handle := m.DeliverAskUserForTesting(twoQuestions())

	m = pressQuestion(t, m, key("enter")) // answer the first question
	m = pressQuestion(t, m, key("esc"))

	answers, err, ok := handle.Answered()
	if !ok || err == nil {
		t.Fatalf("esc must decline the form, got (%v, %v, %v)", answers, err, ok)
	}
	if answers != nil {
		t.Fatalf("a declined form delivers nothing, got %+v", answers)
	}
	if m.HasPendingAskUserForTesting() {
		t.Fatal("esc must close the modal")
	}
	if m.BannerForTesting() != "question declined" {
		t.Fatalf("expected the decline banner, got %q", m.BannerForTesting())
	}
}

// Terminals that cannot render the box-drawing glyphs get the ASCII set, and
// they get it everywhere at once.
func TestQuestionModal_ASCIIGlyphFallback(t *testing.T) {
	restore := tui.UseASCIIGlyphsForTesting(true)
	defer restore()

	m := openForm(t, twoQuestions())
	strip := m.QuestionStripForTesting()
	if strings.ContainsAny(strip, "○●✓") {
		t.Fatalf("unicode glyphs leaked into the ASCII strip:\n%s", strip)
	}
	if !strings.Contains(strip, "[ ] Focus area") {
		t.Fatalf("expected the ASCII checkbox:\n%s", strip)
	}
}

// --- the answer list ---

func TestQuestionModal_DownMovesCursor(t *testing.T) {
	m := openForm(t, oneQuestion("Which option?", false, opts("Option A", "Option B")...))
	m = pressQuestion(t, m, key("down"))
	if got := m.QuestionCursorForTesting(0); got != 1 {
		t.Fatalf("expected cursor 1 after down, got %d", got)
	}
}

// The agent's recommended answer is annotated on the row itself, so it stays
// readable after the user arrows away from it — the cursor alone used to be
// the only signal.
func TestQuestionModal_RecommendedBadgeSurvivesCursorMove(t *testing.T) {
	form := oneQuestion("Which database?", false,
		agent.AskUserOption{Label: "Postgres"},
		agent.AskUserOption{Label: "SQLite", IsRecommended: true},
	)
	m := openForm(t, form)

	if got := m.QuestionCursorForTesting(0); got != 1 {
		t.Fatalf("expected the recommended option to be focused, cursor = %d", got)
	}
	if view := m.ViewQuestionForTesting(); !strings.Contains(view, "recommended") {
		t.Fatalf("recommended marker missing from the answer list:\n%s", view)
	}
	m = pressQuestion(t, m, key("up"))
	if got := m.QuestionCursorForTesting(0); got != 0 {
		t.Fatal("expected the cursor to move off the recommended option")
	}
	if view := m.ViewQuestionForTesting(); !strings.Contains(view, "recommended") {
		t.Fatalf("recommended marker lost once the cursor moved:\n%s", view)
	}
}

// The free-text entry is a client affordance, not one of the agent's answers:
// it stays last and is drawn below a separator.
func TestQuestionModal_CustomEntryIsLastAndSeparated(t *testing.T) {
	m := openForm(t, oneQuestion("Which database?", true, opts("Postgres", "SQLite")...))
	view := m.ViewQuestionForTesting()

	sep := strings.Index(view, "─")
	own := strings.Index(view, "Type something.")
	if sep < 0 || own < 0 || own < sep {
		t.Fatalf("the free-text entry must render last, below a separator:\n%s", view)
	}
}

// With no options to separate from, the free-text entry stands alone: no stray
// divider above the only row.
func TestQuestionModal_NoSeparatorWithoutAgentOptions(t *testing.T) {
	m := openForm(t, oneQuestion("What should we call it?", true))
	if view := m.ViewQuestionForTesting(); strings.Contains(view, "─────") {
		t.Fatalf("unexpected separator in a freeform-only prompt:\n%s", view)
	}
}

// A question with nothing to pick from puts the keyboard straight in the
// textarea: there is no list to arrow through, so one more enter is a step for
// its own sake.
func TestQuestionModal_FreeTextOnlyQuestionOpensTyping(t *testing.T) {
	m := tui.NewModelForTesting()
	m.SetDimensionsForTesting(100, 40)
	m.SetThinkingForTesting(true)
	m, _ = m.DeliverAskUserForTesting(oneQuestion("What should we call it?", true))

	if !m.QuestionEditingForTesting() {
		t.Fatal("a question with no options must open in the free-text entry")
	}
}

func TestQuestionModal_TypedAnswerStaysWhileTyping(t *testing.T) {
	m := openForm(t, oneQuestion("What should we do?", true))
	m = pressQuestion(t, m, key("enter")) // open the free-text entry
	if !m.QuestionEditingForTesting() {
		t.Fatal("enter on the free-text row must start editing")
	}
	m = pressQuestion(t, m, key("y"))
	if !m.HasPendingAskUserForTesting() {
		t.Fatal("the form must stay open while typing")
	}
	if got := strings.TrimSpace(m.TextareaValueForTesting()); got != "y" {
		t.Fatalf("expected the typed text to stay in the textarea, got %q", got)
	}
}

// esc out of the free-text entry returns to the agent's options rather than
// declining outright, as long as there are options to return to.
func TestQuestionModal_EscFromCustomEntryReturnsToOptions(t *testing.T) {
	m := openForm(t, oneQuestion("Which option?", true, opts("Option A", "Option B")...))
	m = pressQuestion(t, m, key("down"), key("down"), key("enter")) // custom row
	m = pressQuestion(t, m, key("esc"))

	if !m.HasPendingAskUserForTesting() {
		t.Fatal("esc from the free-text entry must keep the form open")
	}
	if m.QuestionEditingForTesting() {
		t.Fatal("esc from the free-text entry must return to the option list")
	}
}

// Answering the only question of a one-question form sends it: there is no
// second question to move to, so a Submit tab would be chrome for its own sake.
func TestQuestionModal_SingleQuestionAnswerSubmits(t *testing.T) {
	m := tui.NewModelForTesting()
	m.SetDimensionsForTesting(100, 40)
	m.SetThinkingForTesting(true)
	m, handle := m.DeliverAskUserForTesting(oneQuestion("Which option?", false, opts("Option A", "Option B")...))

	m = pressQuestion(t, m, key("enter"))
	answers, err, ok := handle.Answered()
	if !ok || err != nil {
		t.Fatalf("expected the form to resolve, got (%v, %v)", err, ok)
	}
	if len(answers) != 1 || len(answers[0].Selected) != 1 || answers[0].Selected[0] != "Option A" {
		t.Fatalf("unexpected answers: %+v", answers)
	}
	if m.HasPendingAskUserForTesting() {
		t.Fatal("expected the form to close after answering")
	}
	if m.BannerForTesting() != "answer sent" {
		t.Fatalf("expected the success banner, got %q", m.BannerForTesting())
	}
}

// A multi-select question must be able to come back with more than one answer.
// Task 05 gives it checkbox rows and a space toggle; until then enter toggles,
// and the question gets a Submit tab because picking an option cannot mean
// "and I am done".
func TestQuestionModal_MultiSelectAccumulatesAnswers(t *testing.T) {
	form := agent.AskUserForm{Questions: []agent.AskUserQuestion{{
		Header:      "Features",
		Question:    "Which features?",
		Options:     opts("Search", "Preview", "Notes"),
		MultiSelect: true,
	}}}
	m := tui.NewModelForTesting()
	m.MarkReadyAndTrustedForTesting()
	m.SetDimensionsForTesting(100, 40)
	m.SetThinkingForTesting(true)
	m, handle := m.DeliverAskUserForTesting(form)

	m = pressQuestion(t, m, key("enter"), key("down"), key("down"), key("enter"))
	if _, _, ok := handle.Answered(); ok {
		t.Fatal("a multi-select question must not submit on the first pick")
	}
	m = pressQuestion(t, m, key("tab"), key("enter")) // Submit tab, then send

	answers, err, ok := handle.Answered()
	if !ok || err != nil {
		t.Fatalf("expected the form to submit, got (%v, %v)", err, ok)
	}
	if len(answers) != 1 || len(answers[0].Selected) != 2 {
		t.Fatalf("expected both picks to come back, got %+v", answers)
	}
	if answers[0].Selected[0] != "Search" || answers[0].Selected[1] != "Notes" {
		t.Fatalf("unexpected selection: %+v", answers[0].Selected)
	}
}

// --- layout ---

// The modal owns the screen, so what it renders has to fit it: an unwindowed
// option list used to push the frame past the bottom of the terminal.
func TestQuestionModal_StaysInsideTerminalBounds(t *testing.T) {
	options := opts("Blue", "Green", "Red", "Yellow", "Purple", "Cyan", "Magenta", "Orange")
	options[1].IsRecommended = true
	form := oneQuestion("What's your favorite color? "+strings.Repeat("padding ", 12), true, options...)
	form.Context = "Just testing the ask-user tool as requested, with a context line long enough to wrap on a narrow terminal"

	for _, size := range []struct{ w, h int }{{120, 40}, {100, 30}, {100, 24}, {80, 24}} {
		m := tui.NewModelForTesting()
		m.MarkReadyAndTrustedForTesting()
		m.SetDimensionsForTesting(size.w, size.h)
		m.SetPendingAskUserFormForTesting(form)
		m = m.RecalcLayoutForTesting()

		view := m.ViewForTesting()
		if got := lipgloss.Height(view); got > size.h {
			t.Fatalf("%dx%d: view is %d lines, overflowing the terminal by %d:\n%s",
				size.w, size.h, got, got-size.h, view)
		}
		// Whatever it had to drop, the focused answer stays on screen.
		if !strings.Contains(view, "Green") {
			t.Fatalf("%dx%d: the focused option must stay visible:\n%s", size.w, size.h, view)
		}
	}
}

// The sections are spaced apart when the terminal can afford it — packed
// line-on-line the block reads as a wall — and the blank lines are the first
// thing dropped when it cannot.
func TestQuestionModal_SpacingYieldsToRows(t *testing.T) {
	form := twoQuestions()

	m := tui.NewModelForTesting()
	m.MarkReadyAndTrustedForTesting()
	m.SetDimensionsForTesting(120, 44)
	m.SetPendingAskUserFormForTesting(form)
	m = m.RecalcLayoutForTesting()
	if !strings.Contains(m.ViewQuestionForTesting(), "\n\n") {
		t.Fatalf("a tall terminal should space the sections out:\n%s", m.ViewQuestionForTesting())
	}

	short := tui.NewModelForTesting()
	short.MarkReadyAndTrustedForTesting()
	short.SetDimensionsForTesting(120, 24)
	short.SetPendingAskUserFormForTesting(form)
	short = short.RecalcLayoutForTesting()
	block := short.ViewQuestionForTesting()
	if strings.Contains(block, "\n\n") {
		t.Fatalf("a short terminal must spend its rows on answers, not blank lines:\n%s", block)
	}
	if !strings.Contains(block, "Parser") || !strings.Contains(block, "Renderer") {
		t.Fatalf("both answers should fit once the spacing is dropped:\n%s", block)
	}
}

// Every line of the block has to fit the input box: a wider one wraps, and the
// wrap is a row the layout never reserved, so the box slides off the bottom of
// the terminal.
func TestQuestionModal_FormBlockFitsTheInputBox(t *testing.T) {
	form := agent.AskUserForm{Questions: []agent.AskUserQuestion{
		{Header: "Focus", Question: "Where should I start? " + strings.Repeat("long ", 10),
			Options: opts("Parser", "Renderer", "Cache", "Docs", "Tests"), AllowCustom: true},
		{Header: "Layout", Question: "How should the panel sit?", Options: opts("Left", "Right")},
	}}
	for _, size := range []struct{ w, h int }{{120, 40}, {100, 30}, {80, 24}, {60, 24}} {
		m := tui.NewModelForTesting()
		m.MarkReadyAndTrustedForTesting()
		m.SetDimensionsForTesting(size.w, size.h)
		m.SetPendingAskUserFormForTesting(form)
		m = m.RecalcLayoutForTesting()

		block := m.ViewQuestionForTesting()
		if got := lipgloss.Width(block); got > size.w-4 {
			t.Fatalf("%dx%d: the form block is %d columns wide inside a %d-column box:\n%s",
				size.w, size.h, got, size.w-4, block)
		}
		if view := m.ViewForTesting(); lipgloss.Height(view) > size.h {
			t.Fatalf("%dx%d: view is %d lines:\n%s", size.w, size.h, lipgloss.Height(view), view)
		}
		// However little room is left, the row the cursor is on stays visible.
		if !strings.Contains(block, "Parser") {
			t.Fatalf("%dx%d: the focused option must stay visible:\n%s", size.w, size.h, block)
		}
	}
}

// Scrolling the cursor past the visible window keeps it on screen.
func TestQuestionModal_WindowFollowsCursor(t *testing.T) {
	m := tui.NewModelForTesting()
	m.MarkReadyAndTrustedForTesting()
	m.SetDimensionsForTesting(80, 24)
	m.SetPendingAskUserFormForTesting(oneQuestion("Pick one", false,
		opts("first", "second", "third", "fourth", "fifth", "sixth", "seventh", "last")...))
	m = m.RecalcLayoutForTesting()

	for range 7 {
		m = pressQuestion(t, m, key("down"))
	}
	view := m.RecalcLayoutForTesting().ViewForTesting()
	if !strings.Contains(view, "last") {
		t.Fatalf("cursor row scrolled out of view:\n%s", view)
	}
	if got := lipgloss.Height(view); got > 24 {
		t.Fatalf("view is %d lines, overflowing:\n%s", got, view)
	}
}

// --- the queue ---

// A second form arriving while the user is answering the first must not
// replace it: both tool calls are blocked on a reply, so dropping either one
// strands a run. They are asked in arrival order instead.
func TestAskUser_SecondFormQueuesInsteadOfReplacing(t *testing.T) {
	m := tui.NewModelForTesting()
	m.SetDimensionsForTesting(100, 40)
	m.SetThinkingForTesting(true)

	m, first := m.DeliverAskUserForTesting(oneQuestion("Which database?", false, opts("Postgres", "SQLite")...))
	m, second := m.DeliverAskUserForTesting(oneQuestion("Which cache?", false, opts("Redis", "Memcached")...))

	if got := m.PendingQuestionTextForTesting(); got != "Which database?" {
		t.Fatalf("the first question must stay on screen, got %q", got)
	}
	if got := m.QuestionQueueLenForTesting(); got != 1 {
		t.Fatalf("expected the second form to be queued, queue = %d", got)
	}
	if _, _, ok := second.Answered(); ok {
		t.Fatal("the queued form must not be answered before it is asked")
	}
	if view := m.ViewQuestionForTesting(); !strings.Contains(view, "more question") {
		t.Fatalf("the modal must say another question is waiting:\n%s", view)
	}

	// Answering the first promotes the second, and each answer reaches its own
	// tool call.
	m = pressQuestion(t, m, key("enter"))
	answers, err, ok := first.Answered()
	if !ok || err != nil || len(answers) != 1 || answers[0].Selected[0] != "Postgres" {
		t.Fatalf("first form answered with (%+v, %v, %v)", answers, err, ok)
	}
	if got := m.PendingQuestionTextForTesting(); got != "Which cache?" {
		t.Fatalf("the queued form must be asked next, got %q", got)
	}
	if got := m.QuestionQueueLenForTesting(); got != 0 {
		t.Fatalf("queue should be drained, got %d", got)
	}

	m = pressQuestion(t, m, key("enter"))
	answers, err, ok = second.Answered()
	if !ok || err != nil || len(answers) != 1 || answers[0].Selected[0] != "Redis" {
		t.Fatalf("second form answered with (%+v, %v, %v)", answers, err, ok)
	}
	if m.HasPendingAskUserForTesting() {
		t.Fatal("no form should remain pending")
	}
}

// The half-typed answer belongs to the form on screen: a question arriving
// mid-sentence must not reset the input.
func TestAskUser_QueuedFormKeepsTypedAnswerIntact(t *testing.T) {
	m := tui.NewModelForTesting()
	m.SetDimensionsForTesting(100, 40)
	m.SetThinkingForTesting(true)

	m, _ = m.DeliverAskUserForTesting(oneQuestion("What should we call it?", true))
	m = pressQuestion(t, m, key("enter"), key("h"))

	m, _ = m.DeliverAskUserForTesting(oneQuestion("And the cache?", true))

	if !m.QuestionEditingForTesting() {
		t.Fatal("the user must stay in the answer they were typing")
	}
	if got := strings.TrimSpace(m.TextareaValueForTesting()); got != "h" {
		t.Fatalf("typed answer was clobbered by the incoming question, got %q", got)
	}
	if got := m.PendingQuestionTextForTesting(); got != "What should we call it?" {
		t.Fatalf("the question being answered must stay on screen, got %q", got)
	}
}

// Interrupting the run must unblock every waiting form, not just the one on
// screen.
func TestAskUser_StopAgentAnswersQueuedForms(t *testing.T) {
	m := tui.NewModelForTesting()
	m.SetDimensionsForTesting(100, 40)
	m.SetThinkingForTesting(true)

	m, first := m.DeliverAskUserForTesting(oneQuestion("One?", false, opts("a", "b")...))
	m, second := m.DeliverAskUserForTesting(oneQuestion("Two?", false, opts("a", "b")...))

	m.StopAgentForTesting()

	if _, err, ok := first.Answered(); !ok || err == nil {
		t.Fatal("the on-screen form must be cancelled")
	}
	if _, err, ok := second.Answered(); !ok || err == nil {
		t.Fatal("a queued form must be cancelled too, or its tool call hangs forever")
	}
	if m.QuestionQueueLenForTesting() != 0 {
		t.Fatal("the queue must be cleared on interrupt")
	}
}

// A form that lands after the run has ended cannot be answered by anyone, so
// it must be failed rather than silently dropped.
func TestAskUser_FormAfterRunEndsIsFailed(t *testing.T) {
	m := tui.NewModelForTesting()
	m.SetDimensionsForTesting(100, 40)
	m.SetThinkingForTesting(false)

	m, late := m.DeliverAskUserForTesting(oneQuestion("Too late?", true))

	if _, err, ok := late.Answered(); !ok || err == nil {
		t.Fatal("a form arriving after the run must be answered with an error")
	}
	if m.HasPendingAskUserForTesting() || m.QuestionQueueLenForTesting() != 0 {
		t.Fatal("nothing should be shown for a form the run can no longer use")
	}
}
