package tui_test

import (
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

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
	if view := ansi.Strip(m.ViewForTesting()); !strings.Contains(view, "You have not answered all questions") {
		t.Fatalf("the review page must warn about unanswered questions:\n%s", view)
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

// --- multi-select ---

// multiSelect is the mockup's question: checkboxes, a free-text entry, and the
// Submit row that is the only way to finish it.
func multiSelect() agent.AskUserForm {
	return agent.AskUserForm{Questions: []agent.AskUserQuestion{{
		Header:      "Features",
		Question:    "Which features?",
		Options:     opts("Search", "Preview", "Notes"),
		MultiSelect: true,
		AllowCustom: true,
	}}}
}

// openMultiSelect delivers a form through the agent path, so the tests below
// can assert what the blocked tool call did or did not receive.
func openMultiSelect(t *testing.T, form agent.AskUserForm) (tui.Model, tui.AskUserHandleForTesting) {
	t.Helper()
	m := tui.NewModelForTesting()
	m.MarkReadyAndTrustedForTesting()
	m.SetDimensionsForTesting(100, 40)
	m.SetThinkingForTesting(true)
	return m.DeliverAskUserForTesting(form)
}

// A multi-select question must be able to come back with more than one answer,
// and it gets a Submit tab because picking an option cannot mean "and I am
// done". Selections come back in option order however they were ticked: the
// review page and the tool result must not shuffle with the user's clicks.
func TestQuestionModal_MultiSelectAccumulatesAnswersInOptionOrder(t *testing.T) {
	m, handle := openMultiSelect(t, multiSelect())

	m = pressQuestion(t, m, key("down"), key("down"), key(" "), key("up"), key("up"), key(" "))
	if _, _, ok := handle.Answered(); ok {
		t.Fatal("a multi-select question must not submit on a toggle")
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
		t.Fatalf("want the answers in option order, got %+v", answers[0].Selected)
	}
}

// The two question shapes are answered differently, so they must not look
// alike: a multi-select boxes every answer row, a single-select boxes none.
// Submit sits under the last option unnumbered, and the numbering carries on
// past it to the chat exit.
func TestQuestionModal_MultiSelectRendersCheckboxesAndSubmitRow(t *testing.T) {
	restore := tui.UseASCIIGlyphsForTesting(false)
	defer restore()

	m := openForm(t, multiSelect())
	m = pressQuestion(t, m, key(" "))
	view := plainForm(m)
	for _, want := range []string{
		"❯ 1. ● Search",
		"  2. ○ Preview",
		"  3. ○ Notes",
		"  4. ○ Type something.",
		"       Submit",
		"  5. Chat about this",
	} {
		if !strings.Contains(view, want) {
			t.Fatalf("want a row reading %q:\n%s", want, view)
		}
	}

	single := plainForm(openForm(t, oneQuestion("Which one?", true, opts("Search", "Preview")...)))
	if strings.ContainsAny(single, "○") || strings.Contains(single, "Submit") {
		t.Fatalf("a single-select question takes neither checkboxes nor a Submit row:\n%s", single)
	}
}

// Every key that touches an option row toggles it — nothing on the page except
// Submit finishes the question, so a user who meant to tick three boxes can
// never answer with one by reflex.
func TestQuestionModal_MultiSelectKeysToggleAndOnlySubmitCommits(t *testing.T) {
	m, handle := openMultiSelect(t, multiSelect())

	// enter, space and the digits all toggle the same box off and on.
	for _, keys := range [][]tea.KeyPressMsg{{key("enter")}, {key(" ")}, {key("1")}} {
		m = pressQuestion(t, m, keys...)
		if !m.QuestionAnsweredForTesting(0) {
			t.Fatalf("%v should have ticked the focused box", keys)
		}
		m = pressQuestion(t, m, keys...)
		if m.QuestionAnsweredForTesting(0) {
			t.Fatalf("%v should have unticked it again", keys)
		}
	}
	if _, _, ok := handle.Answered(); ok {
		t.Fatal("toggling must never send the form")
	}

	// The Submit row records the question and leaves the tab where it was.
	m = pressQuestion(t, m, key("1"), key("down"), key("down"), key("down"), key("down"), key("enter"))
	if _, _, ok := handle.Answered(); ok {
		t.Fatal("the Submit row records the question; the Submit tab sends the form")
	}
	if got := m.QuestionTabForTesting(); got != 0 {
		t.Fatalf("recording an answer must not move the tab, got %d", got)
	}
}

// Submitting a multi-select question with nothing ticked is an answer — "none
// of these" — and the model has to be able to tell it from a question the user
// never opened.
func TestQuestionModal_MultiSelectEmptySubmitIsNotASkip(t *testing.T) {
	form := multiSelect()
	form.Questions = append(form.Questions, agent.AskUserQuestion{
		Header: "Rollout", Question: "How should I ship it?", Options: opts("Now", "Later"),
	})
	m, handle := openMultiSelect(t, form)

	// Down to the Submit row (three options, the free-text entry), commit it,
	// then send the form with the second question untouched.
	m = pressQuestion(t, m, key("down"), key("down"), key("down"), key("down"), key("enter"))
	if !m.QuestionAnsweredForTesting(0) {
		t.Fatal("an empty commit still answers the question")
	}
	m = pressQuestion(t, m, key("tab"), key("tab"), key("enter"))

	answers, err, ok := handle.Answered()
	if !ok || err != nil {
		t.Fatalf("expected the form to submit, got (%v, %v)", err, ok)
	}
	if len(answers[0].Selected) != 0 {
		t.Fatalf("nothing was ticked: %+v", answers[0])
	}
	if answers[0].Skipped {
		t.Fatal("an explicit empty answer must not come back as a skip")
	}
	if !answers[1].Skipped {
		t.Fatal("the question the user never opened is the skipped one")
	}
}

// The free-text entry is one more thing in the selection set, not an answer
// that replaces the boxes: its checkbox follows the text, and space takes it
// back out.
func TestQuestionModal_MultiSelectCustomTextJoinsTheSelection(t *testing.T) {
	restore := tui.UseASCIIGlyphsForTesting(false)
	defer restore()
	m, handle := openMultiSelect(t, multiSelect())

	m = pressQuestion(t, m, key(" "), key("4"))
	if !m.QuestionEditingForTesting() {
		t.Fatal("the free-text row opens the inline field")
	}
	m.SetTextareaValueForTesting("keyboard macros")
	m = pressQuestion(t, m, key("enter"))
	if view := plainForm(m); !strings.Contains(view, "● Type something.") {
		t.Fatalf("a typed answer ticks its box:\n%s", view)
	}

	m = pressQuestion(t, m, key("tab"), key("enter"))
	answers, err, ok := handle.Answered()
	if !ok || err != nil {
		t.Fatalf("expected the form to submit, got (%v, %v)", err, ok)
	}
	if len(answers[0].Selected) != 1 || answers[0].Selected[0] != "Search" {
		t.Fatalf("typing an answer must not clear the ticked options: %+v", answers[0])
	}
	if answers[0].Custom != "keyboard macros" {
		t.Fatalf("the typed answer must come back verbatim: %+v", answers[0])
	}
}

// Space on a free-text row that already holds an answer unticks it. It is the
// only way back out of that box, since enter reopens the field to edit it.
func TestQuestionModal_MultiSelectSpaceClearsCustomText(t *testing.T) {
	m := openForm(t, multiSelect())
	m = pressQuestion(t, m, key("4"))
	m.SetTextareaValueForTesting("keyboard macros")
	m = pressQuestion(t, m, key("enter"))
	if got := m.QuestionCustomForTesting(0); got != "keyboard macros" {
		t.Fatalf("expected the typed answer to be recorded, got %q", got)
	}

	m = pressQuestion(t, m, key(" "))
	if got := m.QuestionCustomForTesting(0); got != "" {
		t.Fatalf("space must untick the typed answer, got %q", got)
	}
	if m.QuestionEditingForTesting() {
		t.Fatal("unticking must not reopen the field")
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

// The way to finish a multi-select question is pinned along with the way out of
// the form: an option list that scrolled Submit away would leave the user with
// nothing on the page that answers the question.
func TestQuestionModal_LongMultiSelectPinsSubmit(t *testing.T) {
	form := agent.AskUserForm{Questions: []agent.AskUserQuestion{{
		Header:      "Features",
		Question:    "Which features?",
		Options:     opts("first", "second", "third", "fourth", "fifth", "sixth", "seventh", "last"),
		MultiSelect: true,
		AllowCustom: true,
	}}}
	m := tui.NewModelForTesting()
	m.MarkReadyAndTrustedForTesting()
	m.SetDimensionsForTesting(80, 30)
	m.SetPendingAskUserFormForTesting(form)
	m = m.RecalcLayoutForTesting()

	view := plainForm(m)
	if !strings.Contains(view, "more (↑ ↓ to scroll)") {
		t.Fatalf("expected the list to be windowed at 30 rows:\n%s", view)
	}
	for _, want := range []string{"Submit", "Chat about this", "─────"} {
		if !strings.Contains(view, want) {
			t.Fatalf("%q must stay pinned under the windowed list:\n%s", want, view)
		}
	}
	if got := lipgloss.Height(m.ViewForTesting()); got > 30 {
		t.Fatalf("view is %d lines on a 30-row terminal", got)
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

// --- the review page ---

// reviewForm is the mockup's form: three questions whose answers the review
// page lists, the last one multi-select so the joining rule is exercised.
func reviewForm() agent.AskUserForm {
	return agent.AskUserForm{Questions: []agent.AskUserQuestion{
		{Header: "Focus", Question: "Which part of Spettro would you like to work on next?",
			Options: opts("ACP extensions", "TUI polish")},
		{Header: "Layout", Question: "How should I present the layout for a new TUI panel?",
			Options: opts("Stacked", "Split"), AllowCustom: true},
		{Header: "Checks", Question: "Which checks should run before commits?",
			Options: opts("Unit tests", "gofmt check", "go vet"), MultiSelect: true},
	}}
}

// answerReviewForm fills every question and stops on the review page.
func answerReviewForm(t *testing.T, m tui.Model) tui.Model {
	t.Helper()
	return pressQuestion(t, m,
		key("enter"),             // Focus: ACP extensions
		key("tab"), key("enter"), // Layout: Stacked
		key("tab"), key(" "), key("down"), key(" "), // Checks: two ticked
		key("down"), key("down"), key("enter"), // the multi-select Submit row
		key("tab"))
}

// The review page lists what will be sent in the mockup's shape: the question,
// then its answer under it. Multi-select answers are comma-joined in option
// order, and the user's own words are shown verbatim.
func TestQuestionModal_ReviewListsEveryAnswer(t *testing.T) {
	m := answerReviewForm(t, openForm(t, reviewForm()))
	view := plainForm(m)

	if !strings.Contains(view, "Review your answers") {
		t.Fatalf("expected the review page:\n%s", view)
	}
	for _, want := range []string{
		"● Which part of Spettro would you like to work on next?",
		"→ ACP extensions",
		"● How should I present the layout for a new TUI panel?",
		"→ Stacked",
		"→ Unit tests, gofmt check",
	} {
		if !strings.Contains(view, want) {
			t.Fatalf("the review page is missing %q:\n%s", want, view)
		}
	}
	if strings.Contains(view, "You have not answered") {
		t.Fatalf("a complete form has nothing to warn about:\n%s", view)
	}

	// The user's own words are the answer when they typed one.
	custom := pressQuestion(t, openForm(t, reviewForm()),
		key("enter"),                                           // Focus
		key("tab"), key("3"), key("h"), key("i"), key("enter"), // Layout, typed
		key("tab"), key(" "), key("down"), key("down"), key("down"), key("enter"), // Checks
		key("tab"))
	if got := plainForm(custom); !strings.Contains(got, "→ “hi”") {
		t.Fatalf("a typed answer must be shown verbatim:\n%s", got)
	}
}

// Settled 2026-07-25: on an incomplete form the warning *replaces* the answer
// list. The page says one thing at a time; the tab strip is where the missing
// answer is located.
func TestQuestionModal_ReviewWarningReplacesTheList(t *testing.T) {
	m := pressQuestion(t, openForm(t, reviewForm()), key("enter"), key("tab"), key("tab"), key("tab"))
	view := plainForm(m)

	if !strings.Contains(view, "⚠ You have not answered all questions") {
		t.Fatalf("expected the incomplete warning:\n%s", view)
	}
	if strings.Contains(view, "→ ACP extensions") || strings.Contains(view, "● Which part of Spettro") {
		t.Fatalf("the warning replaces the summary, it does not join it:\n%s", view)
	}
	// The actions are still there — an incomplete form can still be sent.
	for _, want := range []string{"Ready to submit your answers?", "1. Submit answers", "2. Cancel"} {
		if !strings.Contains(view, want) {
			t.Fatalf("%q must stay on an incomplete review page:\n%s", want, view)
		}
	}
}

// The review page has to fit the input box like every other page of the form.
// What it drops as the terminal shrinks is the two headings — the strip and the
// rows already say what they said — never the rows themselves.
func TestQuestionModal_ReviewFitsShortTerminals(t *testing.T) {
	for _, size := range []struct{ w, h int }{{120, 40}, {100, 30}, {100, 24}, {80, 26}, {62, 24}} {
		for _, complete := range []bool{false, true} {
			m := openPreview(t, size.w, size.h, reviewForm())
			if complete {
				m = answerReviewForm(t, m)
			} else {
				m = pressQuestion(t, m, key("tab"), key("tab"), key("tab"))
			}
			m = m.RecalcLayoutForTesting()

			block := plainForm(m)
			if got := lipgloss.Width(m.ViewQuestionForTesting()); got > size.w-4 {
				t.Fatalf("%dx%d: the review page is %d columns wide:\n%s", size.w, size.h, got, block)
			}
			if got := lipgloss.Height(m.ViewQuestionForTesting()); got > size.h {
				t.Fatalf("%dx%d: the review page is %d lines:\n%s", size.w, size.h, got, block)
			}
			for _, want := range []string{"1. Submit answers", "2. Cancel"} {
				if !strings.Contains(block, want) {
					t.Fatalf("%dx%d: %q must survive at any size:\n%s", size.w, size.h, want, block)
				}
			}
			if !complete && !strings.Contains(block, "You have not answered all questions") {
				t.Fatalf("%dx%d: the warning must survive at any size:\n%s", size.w, size.h, block)
			}
		}
	}
}

// Submit answers delivers the form; unanswered questions come back skipped, and
// a multi-select question submitted empty comes back answered — the model has
// to be able to tell those apart.
func TestQuestionModal_ReviewSubmitDeliversSkipsAndEmptyAnswers(t *testing.T) {
	form := agent.AskUserForm{Questions: []agent.AskUserQuestion{
		{Header: "Focus", Question: "Where first?", Options: opts("Parser", "Renderer")},
		{Header: "Extras", Question: "Anything else?", Options: opts("Docs", "Tests"), MultiSelect: true},
		{Header: "Rollout", Question: "How to ship?", Options: opts("Now", "Later")},
	}}
	m := tui.NewModelForTesting()
	m.MarkReadyAndTrustedForTesting()
	m.SetDimensionsForTesting(100, 40)
	m.SetThinkingForTesting(true)
	m, handle := m.DeliverAskUserForTesting(form)

	m = pressQuestion(t, m,
		key("enter"),                                       // Focus: Parser
		key("tab"), key("down"), key("down"), key("enter"), // Extras: Submit with nothing ticked
		key("tab"), key("tab"), // past Rollout to the review page
		key("enter")) // Submit answers

	answers, err, ok := handle.Answered()
	if !ok || err != nil {
		t.Fatalf("expected the form to submit, got (%v, %v)", err, ok)
	}
	if len(answers) != 3 {
		t.Fatalf("expected one answer per question, got %d", len(answers))
	}
	if answers[0].Skipped || len(answers[0].Selected) != 1 {
		t.Fatalf("the answered question came back wrong: %+v", answers[0])
	}
	if answers[1].Skipped || len(answers[1].Selected) != 0 {
		t.Fatalf("an empty multi-select answer is an answer, not a skip: %+v", answers[1])
	}
	if !answers[2].Skipped {
		t.Fatalf("the untouched question must come back skipped: %+v", answers[2])
	}
}

// Cancel and esc are the same thing: back to the question the user came from,
// with every answer intact. Neither declines the form — that is esc from a
// question page, where what would be refused is on screen.
func TestQuestionModal_ReviewCancelReturnsToTheOriginatingTab(t *testing.T) {
	m := tui.NewModelForTesting()
	m.MarkReadyAndTrustedForTesting()
	m.SetDimensionsForTesting(100, 40)
	m.SetThinkingForTesting(true)
	m, handle := m.DeliverAskUserForTesting(reviewForm())

	// Answer the first question, then walk to the review page from the last.
	m = pressQuestion(t, m, key("enter"), key("tab"), key("tab"), key("tab"))
	if got := m.QuestionTabForTesting(); got != 3 {
		t.Fatalf("expected the review page, tab = %d", got)
	}

	m = pressQuestion(t, m, key("down"), key("enter")) // Cancel
	if got := m.QuestionTabForTesting(); got != 2 {
		t.Fatalf("Cancel must return to the tab the user came from, tab = %d", got)
	}
	if _, _, ok := handle.Answered(); ok {
		t.Fatal("Cancel must not send the form")
	}
	if !m.QuestionAnsweredForTesting(0) {
		t.Fatal("Cancel must not cost the user an answer")
	}

	m = pressQuestion(t, m, key("tab"), key("esc"))
	if got := m.QuestionTabForTesting(); got != 2 {
		t.Fatalf("esc on the review page must go back, not decline, tab = %d", got)
	}
	if !m.HasPendingAskUserForTesting() {
		t.Fatal("esc on the review page must not decline the form")
	}
}

// ctrl+d sends the form from wherever the user is, under the same rules the
// review page applies.
func TestQuestionModal_CtrlDSubmitsFromAnyTab(t *testing.T) {
	m := tui.NewModelForTesting()
	m.MarkReadyAndTrustedForTesting()
	m.SetDimensionsForTesting(100, 40)
	m.SetThinkingForTesting(true)
	m, handle := m.DeliverAskUserForTesting(twoQuestions())

	m = pressQuestion(t, m, key("enter"), key("ctrl+d"))
	answers, err, ok := handle.Answered()
	if !ok || err != nil {
		t.Fatalf("ctrl+d must submit from a question page, got (%v, %v)", err, ok)
	}
	if len(answers[0].Selected) != 1 || !answers[1].Skipped {
		t.Fatalf("ctrl+d must apply the partial-submit rules: %+v", answers)
	}
	if m.HasPendingAskUserForTesting() {
		t.Fatal("the form should be closed after ctrl+d")
	}
}

// bubbles binds ctrl+d to delete-forward inside a textarea, so a field with the
// keyboard keeps it: sending the form out from under someone mid-sentence is
// the one thing that keypress must not do.
func TestQuestionModal_CtrlDIsDeleteForwardInATextField(t *testing.T) {
	m := tui.NewModelForTesting()
	m.MarkReadyAndTrustedForTesting()
	m.SetDimensionsForTesting(100, 40)
	m.SetThinkingForTesting(true)
	m, handle := m.DeliverAskUserForTesting(twoQuestions())

	// The custom-answer field on the second question.
	m = pressQuestion(t, m, key("tab"), key("3"), key("h"), key("i"), key("ctrl+d"))
	if _, _, ok := handle.Answered(); ok {
		t.Fatal("ctrl+d must not submit while the custom-answer field has the keyboard")
	}
	if !m.QuestionEditingForTesting() {
		t.Fatal("ctrl+d must leave the field open")
	}

	// The note field, which shares the same textarea.
	m = pressQuestion(t, m, key("esc"), key("n"), key("h"), key("ctrl+d"))
	if _, _, ok := handle.Answered(); ok {
		t.Fatal("ctrl+d must not submit while the note field has the keyboard")
	}
	if !m.QuestionNotesEditingForTesting() {
		t.Fatal("ctrl+d must leave the note field open")
	}
}

// The strip's glyphs and the review page read the same helper, so a tab can
// never say a question is answered while the page counts it missing.
func TestQuestionModal_StripAndReviewAgreeOnAnsweredState(t *testing.T) {
	// Each case answers one more question than the last. A fresh form per case:
	// the state is behind a pointer the model shares, so replaying prefixes on
	// one model would not be the same thing at all.
	prefixes := [][]tea.KeyPressMsg{
		{},
		{key("enter")},
		{key("enter"), key("tab"), key("enter")},
		{key("enter"), key("tab"), key("enter"),
			key("tab"), key(" "), key("down"), key("down"), key("down"), key("enter")},
	}
	for want, keys := range prefixes {
		m := pressQuestion(t, openForm(t, reviewForm()), keys...)

		answered := 0
		for i := range 3 {
			if m.QuestionAnsweredForTesting(i) {
				answered++
			}
		}
		if answered != want {
			t.Fatalf("case %d: %d questions read as answered", want, answered)
		}
		strip := ansi.Strip(m.QuestionStripForTesting())
		if got := strings.Count(strip, "●"); got != answered {
			t.Fatalf("the strip shows %d answered chips, the form counts %d: %q", got, answered, strip)
		}

		review := plainForm(pressQuestion(t, m, key("tab"), key("tab"), key("tab"), key("tab")))
		warned := strings.Contains(review, "You have not answered")
		if warned == (answered == 3) {
			t.Fatalf("review page and strip disagree (answered=%d):\n%s", answered, review)
		}
	}
}

// --- the preview pane ---

// previewSketch is the kind of thing a preview carries: a preformatted layout
// drawing, whose alignment is the whole point of showing it.
const previewSketch = "┌────────────┬────────────┐\n" +
	"│ conversat. │ details    │\n" +
	"│ ▸ item 1   │ name: foo  │\n" +
	"└────────────┴────────────┘"

// previewForm is a mixed question: the first option carries a preview, the
// second does not, so the pane has to appear and collapse as the cursor moves.
func previewForm(preview string) agent.AskUserForm {
	return oneQuestion("How should the panel sit?", false,
		agent.AskUserOption{Label: "Split vertical", Preview: preview},
		agent.AskUserOption{Label: "Stacked"},
	)
}

func openPreview(t *testing.T, w, h int, form agent.AskUserForm) tui.Model {
	t.Helper()
	m := tui.NewModelForTesting()
	m.MarkReadyAndTrustedForTesting()
	m.SetDimensionsForTesting(w, h)
	m.SetPendingAskUserFormForTesting(form)
	return m.RecalcLayoutForTesting()
}

// linesWith returns the rendered lines holding sub, which is how the tests
// below tell a pane *beside* the list from one under it.
func linesWith(view, sub string) []string {
	var out []string
	for _, line := range strings.Split(view, "\n") {
		if strings.Contains(line, sub) {
			out = append(out, line)
		}
	}
	return out
}

// The pane sits beside the option list, bordered like the mention picker: the
// preview is context for the choice, so it has to be readable while the choice
// is being made.
func TestQuestionModal_PreviewRendersBesideTheOptionList(t *testing.T) {
	m := openPreview(t, 110, 40, previewForm(previewSketch))
	view := plainForm(m)

	if !strings.Contains(view, "╭") || !strings.Contains(view, "╰") {
		t.Fatalf("expected a bordered preview pane:\n%s", view)
	}
	for _, line := range strings.Split(previewSketch, "\n") {
		if !strings.Contains(view, line) {
			t.Fatalf("the preview must be shown verbatim, %q is missing:\n%s", line, view)
		}
	}
	// Beside, not under: the focused row and the pane share a terminal line.
	rows := linesWith(view, "Split vertical")
	if len(rows) != 1 || !strings.ContainsAny(rows[0], "╭│") {
		t.Fatalf("the pane must render on the same lines as the option list:\n%s", view)
	}
}

// A question where only some options carry a preview must not leave a hole
// where the pane was: it collapses and the list takes the width back.
func TestQuestionModal_PreviewCollapsesForAnOptionWithout(t *testing.T) {
	m := openPreview(t, 110, 40, previewForm(previewSketch))
	withPane := plainForm(m)

	m = pressQuestion(t, m, key("down"))
	collapsed := plainForm(m)
	if strings.Contains(collapsed, "╭") || strings.Contains(collapsed, "conversat.") {
		t.Fatalf("the pane must collapse when the focused option has no preview:\n%s", collapsed)
	}
	// The rule above the chat exit spans the answer list, so its length is what
	// the list reclaimed.
	narrow, wide := linesWith(withPane, "──────")[0], linesWith(collapsed, "──────")[0]
	if len(strings.TrimSpace(wide)) <= len(strings.TrimSpace(narrow)) {
		t.Fatalf("the list must reflow to the full width once the pane is gone:\n%s", collapsed)
	}
}

// Preview text is preformatted. Wrapping it would destroy the alignment it
// exists to show, so it is clipped with a marker and capped in height with a
// footer that says how much was left out.
func TestQuestionModal_PreviewIsClippedAndCappedNeverWrapped(t *testing.T) {
	long := strings.Repeat("x", 400)
	preview := long
	for i := range 60 {
		preview += "\n" + strconv.Itoa(i)
	}
	m := openPreview(t, 110, 40, previewForm(preview))
	view := plainForm(m)

	if strings.Contains(view, strings.Repeat("x", 200)) {
		t.Fatalf("a long preview line must be clipped, not shown whole:\n%s", view)
	}
	clipped := linesWith(view, "xxx")
	if len(clipped) != 1 {
		t.Fatalf("the long line must stay on one line, got %d:\n%s", len(clipped), view)
	}
	if !strings.Contains(clipped[0], "›") {
		t.Fatalf("a clipped line must say so: %q", clipped[0])
	}
	if !strings.Contains(view, "more lines") {
		t.Fatalf("a preview taller than the pane needs a truncation footer:\n%s", view)
	}
}

// The preview is model output: markdown in it is text, not formatting, and
// escape sequences in it never reach the terminal.
func TestQuestionModal_PreviewIsNeitherRenderedNorTrusted(t *testing.T) {
	m := openPreview(t, 110, 40, previewForm("# heading **bold**\n\x1b[31mred\x1b[0m\x1b[2J\x07 tail"))

	raw := m.ViewQuestionForTesting()
	for _, seq := range []string{"\x1b[2J", "\x07", "\x1b[31m"} {
		if strings.Contains(raw, seq) {
			t.Fatalf("preview escape sequence %q reached the terminal:\n%q", seq, raw)
		}
	}
	view := plainForm(m)
	if !strings.Contains(view, "# heading **bold**") {
		t.Fatalf("preview markdown must be shown as text, not rendered:\n%s", view)
	}
	if !strings.Contains(view, "red") || !strings.Contains(view, "tail") {
		t.Fatalf("stripping must keep the text it was wrapped around:\n%s", view)
	}
}

// Narrow terminals drop the side-by-side layout rather than squeezing both
// columns, and drop the pane entirely once even the stacked one costs the
// answers too much.
func TestQuestionModal_NarrowTerminalStacksThenHidesThePreview(t *testing.T) {
	stacked := plainForm(openPreview(t, 62, 40, previewForm(previewSketch)))
	if !strings.Contains(stacked, "╭") {
		t.Fatalf("expected the pane to survive at 62 columns:\n%s", stacked)
	}
	if rows := linesWith(stacked, "Split vertical"); strings.Contains(rows[0], "│") {
		t.Fatalf("at 62 columns the pane belongs under the list, not beside it:\n%s", stacked)
	}
	if !strings.Contains(stacked, "│ conversat. │ details    │") {
		t.Fatalf("the stacked pane must keep the preview's own alignment:\n%s", stacked)
	}

	tiny := plainForm(openPreview(t, 46, 24, previewForm(previewSketch)))
	if strings.Contains(tiny, "╭") {
		t.Fatalf("a terminal this small must drop the pane:\n%s", tiny)
	}
	for _, want := range []string{"Split vertical", "Stacked", "Chat about this"} {
		if !strings.Contains(tiny, want) {
			t.Fatalf("dropping the pane must leave the option list intact, %q missing:\n%s", want, tiny)
		}
	}
}

// The pane is part of the block the layout reserved room for, so it obeys the
// same bounds as the rest of it — at every width where the columns split
// differently, and with the note field open on top of it.
func TestQuestionModal_PreviewStaysInsideTheInputBox(t *testing.T) {
	form := previewForm(previewSketch + "\n" + strings.Repeat("wide "+strings.Repeat("z", 40)+"\n", 30))
	for _, size := range []struct{ w, h int }{{200, 60}, {120, 40}, {100, 30}, {80, 26}, {66, 24}, {55, 30}, {46, 24}} {
		for _, keys := range [][]tea.KeyPressMsg{{}, {key("down")}, {key("n"), key("a")}} {
			m := pressQuestion(t, openPreview(t, size.w, size.h, form), keys...).RecalcLayoutForTesting()
			if got := lipgloss.Width(m.ViewQuestionForTesting()); got > size.w-4 {
				t.Fatalf("%dx%d: the block is %d columns wide inside a %d-column box:\n%s",
					size.w, size.h, got, size.w-4, plainForm(m))
			}
			if got := lipgloss.Height(m.ViewForTesting()); got > size.h {
				t.Fatalf("%dx%d: view is %d lines:\n%s", size.w, size.h, got, plainForm(m))
			}
		}
	}
}

// The pane redraws on every keystroke, so the sanitising pass is memoised: the
// same focused option hands back the same lines rather than re-reading a
// preview that may be thousands of lines long.
func TestQuestionModal_PreviewIsCachedUntilTheCursorMoves(t *testing.T) {
	m := openPreview(t, 110, 40, previewForm(strings.Repeat("sketch line\n", 4000)))
	_ = m.ViewQuestionForTesting()
	first := m.QuestionPreviewCacheForTesting()
	if len(first) == 0 {
		t.Fatal("expected the preview to be cached after a render")
	}
	_ = m.ViewQuestionForTesting()
	if second := m.QuestionPreviewCacheForTesting(); &second[0] != &first[0] {
		t.Fatal("a second render of the same option must reuse the cached preview")
	}

	start := time.Now()
	for range 40 {
		m = pressQuestion(t, m, key("down"), key("up"))
		_ = m.ViewQuestionForTesting()
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("cursor movement over a full-height preview took %s", elapsed)
	}
}

// --- notes ---

// `n` annotates the question, not the option under the cursor, and the note
// rides along with whatever answer the question comes back with.
func TestQuestionModal_NoteAttachesToTheQuestion(t *testing.T) {
	m, handle := openMultiSelect(t, multiSelect())

	m = pressQuestion(t, m, key("n"))
	if !m.QuestionNotesEditingForTesting() {
		t.Fatal("n must open the note field")
	}
	m = pressQuestion(t, m, key("h"), key("i"), key("enter"))
	if m.QuestionNotesEditingForTesting() {
		t.Fatal("enter must close the note field")
	}
	if got := m.QuestionNoteForTesting(0); got != "hi" {
		t.Fatalf("note not kept, got %q", got)
	}
	if view := plainForm(m); !strings.Contains(view, "Notes: “hi”") {
		t.Fatalf("the hint must show that a note exists:\n%s", view)
	}

	// Tick the first option, walk down past the free-text row to Submit, record.
	m = pressQuestion(t, m, key(" "), key("down"), key("down"), key("down"), key("down"), key("enter"))
	m = pressQuestion(t, m, key("tab"), key("enter"))
	answers, err, ok := handle.Answered()
	if !ok || err != nil {
		t.Fatalf("expected the form to submit, got (%v, %v)", err, ok)
	}
	if len(answers) != 1 || answers[0].Notes != "hi" {
		t.Fatalf("the note must ride along with the answer: %+v", answers)
	}
}

// esc closes the field keeping what was typed: a note is a scratch annotation,
// so there is nothing to confirm and nothing to lose by backing out of it.
func TestQuestionModal_EscKeepsTheNote(t *testing.T) {
	m := openForm(t, twoQuestions())
	m = pressQuestion(t, m, key("n"), key("h"), key("i"), key("esc"))
	if m.QuestionNotesEditingForTesting() {
		t.Fatal("esc must close the note field")
	}
	if got := m.QuestionNoteForTesting(0); got != "hi" {
		t.Fatalf("esc must keep the text, got %q", got)
	}
	if !m.HasPendingAskUserForTesting() {
		t.Fatal("esc out of the note field must not decline the form")
	}
}

// A note is an annotation, not a decision: a question carrying only one is
// still unanswered, on the tab strip and in what the model is told.
func TestQuestionModal_NoteAloneDoesNotAnswerTheQuestion(t *testing.T) {
	m := tui.NewModelForTesting()
	m.MarkReadyAndTrustedForTesting()
	m.SetDimensionsForTesting(100, 40)
	m.SetThinkingForTesting(true)
	m, handle := m.DeliverAskUserForTesting(twoQuestions())

	m = pressQuestion(t, m, key("n"), key("h"), key("i"), key("enter"))
	if m.QuestionAnsweredForTesting(0) {
		t.Fatal("a note must not mark the question answered")
	}
	if strip := ansi.Strip(m.QuestionStripForTesting()); !strings.Contains(strip, "○ Focus area") {
		t.Fatalf("the tab must still read as unanswered: %q", strip)
	}

	m = pressQuestion(t, m, key("tab"), key("tab"), key("enter"))
	answers, err, ok := handle.Answered()
	if !ok || err != nil {
		t.Fatalf("expected the form to submit, got (%v, %v)", err, ok)
	}
	if !answers[0].Skipped || answers[0].Notes != "hi" {
		t.Fatalf("the question must come back skipped, note attached: %+v", answers[0])
	}
}

// The note field comes off the form's line budget rather than being added to
// it: the block's height is what the layout reserved for the input box, so a
// field that grew the page would push the box off the bottom of the terminal.
func TestQuestionModal_NoteFieldStaysInsideTheBlockBudget(t *testing.T) {
	for _, size := range []struct{ w, h int }{{110, 40}, {80, 26}, {62, 24}, {46, 24}} {
		m := openPreview(t, size.w, size.h, previewForm(previewSketch))
		m = pressQuestion(t, m, key("n"), key("a"))
		m = m.RecalcLayoutForTesting()

		if got := lipgloss.Height(m.ViewForTesting()); got > size.h {
			t.Fatalf("%dx%d: the note field pushed the view to %d lines:\n%s",
				size.w, size.h, got, m.ViewForTesting())
		}
		if !strings.Contains(plainForm(m), "Notes:") {
			t.Fatalf("%dx%d: the field being typed in must stay on screen:\n%s",
				size.w, size.h, plainForm(m))
		}
	}
}

// A single-question form submits on its answer. A note is not one, so writing
// one must not send the form out from under the user.
func TestQuestionModal_NoteDoesNotSubmitASinglePageForm(t *testing.T) {
	m := tui.NewModelForTesting()
	m.SetDimensionsForTesting(100, 40)
	m.SetThinkingForTesting(true)
	m, handle := m.DeliverAskUserForTesting(oneQuestion("Which option?", false, opts("Option A", "Option B")...))

	m = pressQuestion(t, m, key("n"), key("h"), key("enter"))
	if _, _, ok := handle.Answered(); ok {
		t.Fatal("writing a note must not submit the form")
	}
	if !m.HasPendingAskUserForTesting() {
		t.Fatal("the form must stay open after a note")
	}
}

// --- the remote and Telegram surfaces ---

// The remote event carries the whole form, versioned, with the flat v1 fields
// describing the question those surfaces are being asked to answer right now.
func TestQuestionModal_RemoteEventCarriesTheWholeForm(t *testing.T) {
	m := openForm(t, reviewForm())

	payload := m.QuestionRemotePayloadForTesting()
	if payload["version"] != agent.RemoteAskUserVersion {
		t.Fatalf("the event must be versioned: %v", payload["version"])
	}
	questions, _ := payload["questions"].([]map[string]any)
	if len(questions) != 3 {
		t.Fatalf("expected every question in the event, got %d", len(questions))
	}
	if payload["active"] != 0 {
		t.Fatalf("active = %v, want the question on screen", payload["active"])
	}

	// Those surfaces answer one question at a time, so the flat fields follow
	// the user through the form.
	m = pressQuestion(t, m, key("tab"))
	payload = m.QuestionRemotePayloadForTesting()
	if payload["active"] != 1 {
		t.Fatalf("active = %v, want the tab the user moved to", payload["active"])
	}
	if q, _ := payload["question"].(string); !strings.Contains(q, "layout for a new TUI panel") {
		t.Fatalf("the flat question must describe the active tab: %q", q)
	}

	// The review page is a page of the TUI, not a question anyone can be asked.
	m = pressQuestion(t, m, key("tab"), key("tab"))
	if got := m.QuestionRemotePayloadForTesting(); got != nil {
		t.Fatalf("nothing to publish from the review page, got %+v", got)
	}
}

// A form is one interaction in the chat too: the relay says which question it
// is showing rather than sending three unexplained prompts.
func TestQuestionModal_TelegramHeadingNumbersTheForm(t *testing.T) {
	one := tui.TelegramQuestionHeadingForTesting(map[string]any{"count": 1, "active": 0})
	if one != "❓ Spettro is asking:" {
		t.Fatalf("a single question needs no numbering: %q", one)
	}
	many := tui.TelegramQuestionHeadingForTesting(map[string]any{"count": 3, "active": 1})
	if many != "❓ Spettro is asking (question 2 of 3):" {
		t.Fatalf("heading = %q", many)
	}
	// A v1 event has neither field and must still read correctly.
	legacy := tui.TelegramQuestionHeadingForTesting(map[string]any{})
	if legacy != "❓ Spettro is asking:" {
		t.Fatalf("legacy heading = %q", legacy)
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
