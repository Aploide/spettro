package tui_test

import (
	"errors"
	"slices"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"spettro/internal/agent"
	"spettro/internal/tui"
)

// plainForm is the question block with its styling stripped. The rows carry a
// style change between the number and the label, so any assertion about how a
// row reads has to be made against the text a user actually sees.
func plainForm(m tui.Model) string { return ansi.Strip(m.ViewQuestionForTesting()) }

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

// The mockup's layout: a chevron at the left margin outside the number column,
// every row numbered, and each option's description on its own line indented to
// the label.
func TestQuestionModal_RowsAreNumberedWithDescriptionLines(t *testing.T) {
	restore := tui.UseASCIIGlyphsForTesting(false)
	defer restore()

	m := openForm(t, oneQuestion("Which part?", true,
		agent.AskUserOption{Label: "ACP extensions", Description: "Continue building out the extension surface."},
		agent.AskUserOption{Label: "TUI polish", Description: "Refine terminal UI behavior."},
	))
	lines := strings.Split(plainForm(m), "\n")

	want := []string{
		"  ❯ 1. ACP extensions",
		"       Continue building out the extension surface.",
		"    2. TUI polish",
		"       Refine terminal UI behavior.",
		"    3. Type something.",
	}
	for _, w := range want {
		if !slices.ContainsFunc(lines, func(l string) bool { return strings.TrimRight(l, " ") == w }) {
			t.Fatalf("missing row line %q in:\n%s", w, strings.Join(lines, "\n"))
		}
	}
}

// The agent's suggestion is where the cursor starts and carries a marker of its
// own, so it stays legible once the cursor moves on.
func TestQuestionModal_RecommendedMarkerAndInitialFocus(t *testing.T) {
	restore := tui.UseASCIIGlyphsForTesting(false)
	defer restore()

	m := openForm(t, oneQuestion("Which part?", false,
		agent.AskUserOption{Label: "Parser"},
		agent.AskUserOption{Label: "Renderer", IsRecommended: true},
	))
	if got := m.QuestionCursorForTesting(0); got != 1 {
		t.Fatalf("the recommended option must hold initial focus, cursor = %d", got)
	}
	if view := plainForm(m); !strings.Contains(view, "❯ 2. Renderer  ● recommended") {
		t.Fatalf("expected the marker after the label:\n%s", view)
	}
	m = pressQuestion(t, m, key("up"))
	if view := plainForm(m); !strings.Contains(view, "2. Renderer  ● recommended") {
		t.Fatalf("the marker must survive the cursor moving off it:\n%s", view)
	}
}

// The mockups number every row, so the digits have to pick one.
func TestQuestionModal_DigitHotkeySelectsTheMatchingRow(t *testing.T) {
	m := tui.NewModelForTesting()
	m.MarkReadyAndTrustedForTesting()
	m.SetDimensionsForTesting(100, 40)
	m.SetThinkingForTesting(true)
	m, handle := m.DeliverAskUserForTesting(oneQuestion("Which option?", false, opts("A", "B", "C")...))

	m = pressQuestion(t, m, key("3"))
	answers, err, ok := handle.Answered()
	if !ok || err != nil {
		t.Fatalf("a digit must select and answer, got (%v, %v)", err, ok)
	}
	if len(answers[0].Selected) != 1 || answers[0].Selected[0] != "C" {
		t.Fatalf("digit picked the wrong row: %+v", answers[0])
	}
	if got := m.QuestionCursorForTesting(0); got >= 0 {
		t.Fatalf("the form should be closed, cursor = %d", got)
	}
}

// A digit past the last row is not a selection — it must not wrap onto one.
func TestQuestionModal_DigitPastTheLastRowDoesNothing(t *testing.T) {
	m := tui.NewModelForTesting()
	m.MarkReadyAndTrustedForTesting()
	m.SetDimensionsForTesting(100, 40)
	m.SetThinkingForTesting(true)
	m, handle := m.DeliverAskUserForTesting(oneQuestion("Which option?", false, opts("A", "B")...))

	m = pressQuestion(t, m, key("9"))
	if _, _, ok := handle.Answered(); ok {
		t.Fatal("a digit with no row behind it must not answer the form")
	}
	if got := m.QuestionCursorForTesting(0); got != 0 {
		t.Fatalf("the cursor must not move, cursor = %d", got)
	}
}

// The free-text row becomes the field itself, commits with enter, and backs out
// with esc without touching what the other questions already hold.
func TestQuestionModal_CustomEntryCommitsAndCancels(t *testing.T) {
	m := openForm(t, twoQuestions())
	m = pressQuestion(t, m, key("enter")) // Q1 answered with "Parser"
	m = pressQuestion(t, m, key("tab"), key("3"))
	if !m.QuestionEditingForTesting() {
		t.Fatal("the digit on the free-text row must open the field")
	}
	if view := plainForm(m); !strings.Contains(view, "❯ 3. Type something.") {
		t.Fatalf("the field must render at the row it replaced:\n%s", view)
	}

	m = pressQuestion(t, m, key("h"), key("i"), key("esc"))
	if m.QuestionEditingForTesting() {
		t.Fatal("esc must return to the list")
	}
	if got := m.QuestionCustomForTesting(1); got != "" {
		t.Fatalf("an abandoned draft must not be recorded, got %q", got)
	}
	if !m.QuestionAnsweredForTesting(0) {
		t.Fatal("backing out of one question must not clear another's answer")
	}

	m = pressQuestion(t, m, key("enter"), key("h"), key("i"), key("enter"))
	if got := m.QuestionCustomForTesting(1); got != "hi" {
		t.Fatalf("enter must commit the text, got %q", got)
	}
	if view := plainForm(m); !strings.Contains(view, "“hi”") {
		t.Fatalf("the committed answer must show on its row:\n%s", view)
	}
}

// The chat exit is the last numbered row, below the rule, and it is neither an
// answer nor a decline: the tool is told the user wants to talk, and the run
// ends on that (settled 2026-07-25).
func TestQuestionModal_ChatAboutThisEndsTheTurn(t *testing.T) {
	m := tui.NewModelForTesting()
	m.MarkReadyAndTrustedForTesting()
	m.SetDimensionsForTesting(100, 40)
	m.SetThinkingForTesting(true)
	m, handle := m.DeliverAskUserForTesting(oneQuestion("Which database?", false, opts("Postgres", "SQLite")...))

	view := plainForm(m)
	rule := strings.Index(view, "─────")
	chat := strings.Index(view, "3. Chat about this")
	if rule < 0 || chat < 0 || rule > chat {
		t.Fatalf("the chat exit must be numbered last, below the rule:\n%s", view)
	}

	m = pressQuestion(t, m, key("3"))
	answers, err, ok := handle.Answered()
	if !ok {
		t.Fatal("the chat exit must unblock the tool call")
	}
	if !errors.Is(err, agent.ErrAskUserReplyInChat) {
		t.Fatalf("expected the reply-in-chat signal, got %v", err)
	}
	if answers != nil {
		t.Fatalf("nothing is answered when the user leaves for the chat: %+v", answers)
	}
	if m.HasPendingAskUserForTesting() {
		t.Fatal("the form must close so the user can type")
	}
}

// Even a question with nothing to pick from has rows worth reaching, so esc out
// of the field lands on the list rather than declining outright.
func TestQuestionModal_ChatExitIsReachableFromAFreeTextQuestion(t *testing.T) {
	m := tui.NewModelForTesting()
	m.MarkReadyAndTrustedForTesting()
	m.SetDimensionsForTesting(100, 40)
	m.SetThinkingForTesting(true)
	m, handle := m.DeliverAskUserForTesting(oneQuestion("What should we call it?", true))

	if !m.QuestionEditingForTesting() {
		t.Fatal("a question with no options opens in the field")
	}
	m = pressQuestion(t, m, key("esc"))
	if !m.HasPendingAskUserForTesting() || m.QuestionEditingForTesting() {
		t.Fatal("esc from the field must land on the list, not decline the form")
	}
	m = pressQuestion(t, m, key("2"))
	if _, err, ok := handle.Answered(); !ok || !errors.Is(err, agent.ErrAskUserReplyInChat) {
		t.Fatalf("the chat exit must be reachable, got (%v, %v)", err, ok)
	}
}

// Descriptions reflow at the width left beside the label column; they are never
// clipped mid-word onto one line.
func TestQuestionModal_DescriptionsWrapAtNarrowWidth(t *testing.T) {
	desc := "Refine terminal UI behavior, selection, rendering, keybindings and status display"
	m := tui.NewModelForTesting()
	m.MarkReadyAndTrustedForTesting()
	m.SetDimensionsForTesting(60, 40)
	m.SetPendingAskUserFormForTesting(oneQuestion("Which part?", false,
		agent.AskUserOption{Label: "TUI polish", Description: desc},
		agent.AskUserOption{Label: "Parser"},
	))
	m = m.RecalcLayoutForTesting()

	var wrapped []string
	for _, line := range strings.Split(plainForm(m), "\n") {
		if trimmed := strings.TrimSpace(line); strings.HasPrefix(desc, trimmed) || strings.HasSuffix(desc, trimmed) {
			if trimmed != "" {
				wrapped = append(wrapped, trimmed)
			}
		}
	}
	if len(wrapped) < 2 {
		t.Fatalf("the description must reflow onto several lines at 60 columns:\n%s", plainForm(m))
	}
	if !strings.HasPrefix(desc, wrapped[0]) || !strings.HasSuffix(desc, wrapped[len(wrapped)-1]) {
		t.Fatalf("the description was cut instead of wrapped: %q", wrapped)
	}
	if got := lipgloss.Width(m.ViewQuestionForTesting()); got > 60-4 {
		t.Fatalf("the block is %d columns wide inside a 60-column terminal", got)
	}
}

// A list too long for the page scrolls under the rule and the chat exit: the
// way out of the form must not be the first thing to scroll away.
func TestQuestionModal_LongListScrollsWithTheChatExitPinned(t *testing.T) {
	m := tui.NewModelForTesting()
	m.MarkReadyAndTrustedForTesting()
	m.SetDimensionsForTesting(80, 26)
	m.SetPendingAskUserFormForTesting(oneQuestion("Pick one", true,
		opts("first", "second", "third", "fourth", "fifth", "sixth", "seventh", "last")...))
	m = m.RecalcLayoutForTesting()

	view := plainForm(m)
	if !strings.Contains(view, "more (↑ ↓ to scroll)") {
		t.Fatalf("expected the list to be windowed at 26 rows:\n%s", view)
	}
	if !strings.Contains(view, "Chat about this") || !strings.Contains(view, "─────") {
		t.Fatalf("the rule and the chat exit must stay pinned:\n%s", view)
	}
	if strings.Contains(view, "last") {
		t.Fatalf("an option far from the cursor should have scrolled away:\n%s", view)
	}

	for range 7 {
		m = pressQuestion(t, m, key("down"))
	}
	view = plainForm(m)
	if !strings.Contains(view, "last") {
		t.Fatalf("the cursor row must be scrolled into view:\n%s", view)
	}
	if !strings.Contains(view, "Chat about this") {
		t.Fatalf("the chat exit must still be pinned after scrolling:\n%s", view)
	}
	if got := lipgloss.Height(m.RecalcLayoutForTesting().ViewForTesting()); got > 26 {
		t.Fatalf("view is %d lines on a 26-row terminal", got)
	}
}

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

// The free-text entry is still an answer to the question, so it is the last
// numbered *option* — above the rule. Only the chat exit, which answers
// nothing, sits below it.
func TestQuestionModal_CustomEntryIsTheLastOptionAboveTheRule(t *testing.T) {
	m := openForm(t, oneQuestion("Which database?", true, opts("Postgres", "SQLite")...))
	view := plainForm(m)

	own := strings.Index(view, "Type something.")
	sep := strings.Index(view, "─────")
	chat := strings.Index(view, "Chat about this")
	if own < 0 || sep < 0 || chat < 0 {
		t.Fatalf("expected the free-text entry, the rule and the chat exit:\n%s", view)
	}
	if !(own < sep && sep < chat) {
		t.Fatalf("expected the order option / rule / chat exit:\n%s", view)
	}
	if !strings.Contains(view, "3. Type something.") || !strings.Contains(view, "4. Chat about this") {
		t.Fatalf("every row is numbered, the client's rows included:\n%s", view)
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
	for _, size := range []struct{ w, h int }{{120, 40}, {100, 30}, {90, 22}, {80, 24}, {60, 24}} {
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
