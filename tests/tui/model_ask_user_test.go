package tui_test

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"spettro/internal/agent"
	"spettro/internal/tui"
)

func TestAskUserOptions_IncludeFreeResponseChoice(t *testing.T) {
	options := tui.AskUserOptionsForTesting(agent.AskUserRequest{
		Options:           []string{"Option A", "Option B"},
		AllowFreeResponse: true,
	})
	if len(options) != 3 {
		t.Fatalf("expected 3 options, got %d", len(options))
	}
	if got := options[len(options)-1]; !strings.Contains(got, "own answer") {
		t.Fatalf("expected trailing free-response option, got %q", got)
	}
}

func TestUpdateAskUserQuestion_DownMovesCursor(t *testing.T) {
	m := tui.NewModelForTesting()
	m.SetPendingAskUserForTesting(agent.AskUserRequest{
		Question: "Which option?",
		Options:  []string{"Option A", "Option B"},
	}, false)

	gotModel, _ := m.UpdateAskUserQuestionForTesting(tea.KeyPressMsg{Code: tea.KeyDown})
	got := gotModel.(tui.Model)
	if got.QuestionCursorForTesting() != 1 {
		t.Fatalf("expected cursor 1 after down, got %d", got.QuestionCursorForTesting())
	}
}

func TestUpdateAskUserQuestion_EnterFreeformKeepsTypedText(t *testing.T) {
	m := tui.NewModelForTesting()
	m.SetPendingAskUserForTesting(agent.AskUserRequest{
		Question:          "What should we do?",
		AllowFreeResponse: true,
	}, true)

	gotModel, _ := m.UpdateAskUserQuestionForTesting(tea.KeyPressMsg{Code: 'y', Text: "y"})
	got := gotModel.(tui.Model)
	if !got.HasPendingAskUserForTesting() {
		t.Fatal("pending question should remain while typing a freeform response")
	}
	if !got.QuestionFreeformForTesting() {
		t.Fatal("expected freeform mode to stay active")
	}
	if strings.TrimSpace(got.TextareaValueForTesting()) != "y" {
		t.Fatalf("expected typed text to stay in textarea, got %q", got.TextareaValueForTesting())
	}
}

// The agent's recommended answer is annotated on the row itself, so it stays
// readable after the user arrows away from it — the cursor alone used to be
// the only signal.
func TestRenderAskUserPrompt_RecommendedBadgeSurvivesCursorMove(t *testing.T) {
	req := agent.AskUserRequest{
		Question:      "Which database?",
		Options:       []string{"Postgres", "SQLite"},
		DefaultOption: "SQLite",
	}
	m := tui.NewModelForTesting()
	m.SetDimensionsForTesting(100, 40)
	m.SetPendingAskUserForTesting(req, false)

	if got := m.QuestionCursorForTesting(); got != 1 {
		t.Fatalf("expected the recommended option to be focused, cursor = %d", got)
	}
	if view := m.RenderAskUserPromptForTesting(); !strings.Contains(view, "recommended") {
		t.Fatalf("recommended marker missing from the picker:\n%s", view)
	}

	gotModel, _ := m.UpdateAskUserQuestionForTesting(tea.KeyPressMsg{Code: tea.KeyUp})
	moved := gotModel.(tui.Model)
	if moved.QuestionCursorForTesting() != 0 {
		t.Fatalf("expected the cursor to move off the recommended option")
	}
	if view := moved.RenderAskUserPromptForTesting(); !strings.Contains(view, "recommended") {
		t.Fatalf("recommended marker lost once the cursor moved:\n%s", view)
	}
}

// The free-text entry is a client affordance, not one of the agent's answers:
// it stays last and is drawn below a separator.
func TestRenderAskUserPrompt_CustomEntryIsLastAndSeparated(t *testing.T) {
	m := tui.NewModelForTesting()
	m.SetDimensionsForTesting(100, 40)
	m.SetPendingAskUserForTesting(agent.AskUserRequest{
		Question:          "Which database?",
		Options:           []string{"Postgres", "SQLite"},
		AllowFreeResponse: true,
	}, false)

	view := m.RenderAskUserPromptForTesting()
	if !strings.Contains(view, "─") {
		t.Fatalf("expected a separator above the free-text entry:\n%s", view)
	}
	sep := strings.Index(view, "─")
	own := strings.Index(view, "own answer")
	if own < 0 || own < sep {
		t.Fatalf("free-text entry must be rendered last, below the separator:\n%s", view)
	}
}

// With no options to separate from, the free-text entry stands alone: no
// stray divider above the only row.
func TestRenderAskUserPrompt_NoSeparatorWithoutAgentOptions(t *testing.T) {
	m := tui.NewModelForTesting()
	m.SetDimensionsForTesting(100, 40)
	m.SetPendingAskUserForTesting(agent.AskUserRequest{
		Question:          "What should we call it?",
		AllowFreeResponse: true,
	}, true)

	if view := m.RenderAskUserPromptForTesting(); strings.Contains(view, "─────") {
		t.Fatalf("unexpected separator in a freeform-only prompt:\n%s", view)
	}
}

func TestUpdateAskUserQuestion_EscDeclines(t *testing.T) {
	m := tui.NewModelForTesting()
	m.SetPendingAskUserForTesting(agent.AskUserRequest{
		Question: "Which option?",
		Options:  []string{"Option A", "Option B"},
	}, false)

	gotModel, _ := m.UpdateAskUserQuestionForTesting(tea.KeyPressMsg{Code: tea.KeyEscape})
	got := gotModel.(tui.Model)
	if got.HasPendingAskUserForTesting() {
		t.Fatal("esc on the option list must decline the question")
	}
	if got.BannerForTesting() != "question declined" {
		t.Fatalf("expected the decline banner, got %q", got.BannerForTesting())
	}
}

// esc out of the free-text entry returns to the agent's options rather than
// declining outright, as long as there are options to return to.
func TestUpdateAskUserQuestion_EscFromFreeformReturnsToOptions(t *testing.T) {
	m := tui.NewModelForTesting()
	m.SetPendingAskUserForTesting(agent.AskUserRequest{
		Question:          "Which option?",
		Options:           []string{"Option A", "Option B"},
		AllowFreeResponse: true,
	}, true)

	gotModel, _ := m.UpdateAskUserQuestionForTesting(tea.KeyPressMsg{Code: tea.KeyEscape})
	got := gotModel.(tui.Model)
	if !got.HasPendingAskUserForTesting() {
		t.Fatal("esc from freeform must keep the question pending")
	}
	if got.QuestionFreeformForTesting() {
		t.Fatal("esc from freeform must return to the option list")
	}
}

func TestUpdateAskUserQuestion_EnterOptionResolvesPrompt(t *testing.T) {
	m := tui.NewModelForTesting()
	m.SetPendingAskUserForTesting(agent.AskUserRequest{
		Question: "Which option?",
		Options:  []string{"Option A", "Option B"},
	}, false)

	gotModel, _ := m.UpdateAskUserQuestionForTesting(tea.KeyPressMsg{Code: tea.KeyEnter})
	got := gotModel.(tui.Model)
	if got.HasPendingAskUserForTesting() {
		t.Fatal("expected question to resolve after selecting an option")
	}
	if got.BannerForTesting() != "answer sent" {
		t.Fatalf("expected success banner, got %q", got.BannerForTesting())
	}
}

// The question block grows with the option list, so the layout must reserve
// its real height: an unbounded picker used to push the input box past the
// bottom of the terminal.
func TestAskUserPrompt_StaysInsideTerminalBounds(t *testing.T) {
	options := []string{
		"Blue", "Green", "Red", "Yellow", "Purple", "Cyan", "Magenta", "Orange",
	}
	for _, size := range []struct{ w, h int }{{120, 40}, {100, 30}, {100, 24}, {80, 24}} {
		m := tui.NewModelForTesting()
		m.MarkReadyAndTrustedForTesting()
		m.SetDimensionsForTesting(size.w, size.h)
		m.SetPendingAskUserForTesting(agent.AskUserRequest{
			Question:          "What's your favorite color? " + strings.Repeat("padding ", 12),
			Context:           "Just testing the ask-user tool as requested, with a context line long enough to wrap on a narrow terminal",
			Options:           options,
			DefaultOption:     "Green",
			AllowFreeResponse: true,
		}, false)

		laidOut := m.RecalcLayoutForTesting()
		view := laidOut.ViewForTesting()
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

// Scrolling the cursor past the visible window keeps it on screen.
func TestAskUserPrompt_WindowFollowsCursor(t *testing.T) {
	m := tui.NewModelForTesting()
	m.MarkReadyAndTrustedForTesting()
	m.SetDimensionsForTesting(80, 24)
	m.SetPendingAskUserForTesting(agent.AskUserRequest{
		Question: "Pick one",
		Options:  []string{"first", "second", "third", "fourth", "fifth", "sixth", "seventh", "last"},
	}, false)

	var model tea.Model = m
	for range 7 {
		model, _ = model.(tui.Model).UpdateAskUserQuestionForTesting(tea.KeyPressMsg{Code: tea.KeyDown})
	}
	laidOut := model.(tui.Model).RecalcLayoutForTesting()
	view := laidOut.ViewForTesting()
	if !strings.Contains(view, "last") {
		t.Fatalf("cursor row scrolled out of view:\n%s", view)
	}
	if got := lipgloss.Height(view); got > 24 {
		t.Fatalf("view is %d lines, overflowing:\n%s", got, view)
	}
}

// A second question arriving while the user is answering the first must not
// replace it: both tool calls are blocked on a reply, so dropping either one
// strands a run. They are asked in arrival order instead.
func TestAskUser_SecondQuestionQueuesInsteadOfReplacing(t *testing.T) {
	m := tui.NewModelForTesting()
	m.SetDimensionsForTesting(100, 40)
	m.SetThinkingForTesting(true)

	m, first := m.DeliverAskUserForTesting(agent.AskUserRequest{
		Question: "Which database?",
		Options:  []string{"Postgres", "SQLite"},
	})
	m, second := m.DeliverAskUserForTesting(agent.AskUserRequest{
		Question: "Which cache?",
		Options:  []string{"Redis", "Memcached"},
	})

	if got := m.PendingQuestionTextForTesting(); got != "Which database?" {
		t.Fatalf("the first question must stay on screen, got %q", got)
	}
	if got := m.QuestionQueueLenForTesting(); got != 1 {
		t.Fatalf("expected the second question to be queued, queue = %d", got)
	}
	if _, _, ok := second.Answered(); ok {
		t.Fatal("the queued question must not be answered before it is asked")
	}
	if view := m.RenderAskUserPromptForTesting(); !strings.Contains(view, "more question") {
		t.Fatalf("the prompt must say another question is waiting:\n%s", view)
	}

	// Answering the first promotes the second, and each answer reaches its own
	// tool call.
	gotModel, _ := m.UpdateAskUserQuestionForTesting(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = gotModel.(tui.Model)
	answer, err, ok := first.Answered()
	if !ok || err != nil || answer != "Postgres" {
		t.Fatalf("first question answered with (%q, %v, %v)", answer, err, ok)
	}
	if got := m.PendingQuestionTextForTesting(); got != "Which cache?" {
		t.Fatalf("the queued question must be asked next, got %q", got)
	}
	if got := m.QuestionQueueLenForTesting(); got != 0 {
		t.Fatalf("queue should be drained, got %d", got)
	}

	gotModel, _ = m.UpdateAskUserQuestionForTesting(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = gotModel.(tui.Model)
	answer, err, ok = second.Answered()
	if !ok || err != nil || answer != "Redis" {
		t.Fatalf("second question answered with (%q, %v, %v)", answer, err, ok)
	}
	if m.HasPendingAskUserForTesting() {
		t.Fatal("no question should remain pending")
	}
}

// The half-typed answer belongs to the question on screen: a question arriving
// mid-sentence must not reset the input.
func TestAskUser_QueuedQuestionKeepsTypedAnswerIntact(t *testing.T) {
	m := tui.NewModelForTesting()
	m.SetDimensionsForTesting(100, 40)
	m.SetThinkingForTesting(true)

	m, _ = m.DeliverAskUserForTesting(agent.AskUserRequest{
		Question:          "What should we call it?",
		AllowFreeResponse: true,
	})
	gotModel, _ := m.UpdateAskUserQuestionForTesting(tea.KeyPressMsg{Code: 'h', Text: "h"})
	m = gotModel.(tui.Model)

	m, _ = m.DeliverAskUserForTesting(agent.AskUserRequest{Question: "And the cache?"})

	if !m.QuestionFreeformForTesting() {
		t.Fatal("the user must stay in the answer they were typing")
	}
	if got := strings.TrimSpace(m.TextareaValueForTesting()); got != "h" {
		t.Fatalf("typed answer was clobbered by the incoming question, got %q", got)
	}
	if got := m.PendingQuestionTextForTesting(); got != "What should we call it?" {
		t.Fatalf("the question being answered must stay on screen, got %q", got)
	}
}

// Interrupting the run must unblock every waiting question, not just the one
// on screen.
func TestAskUser_StopAgentAnswersQueuedQuestions(t *testing.T) {
	m := tui.NewModelForTesting()
	m.SetDimensionsForTesting(100, 40)
	m.SetThinkingForTesting(true)

	m, first := m.DeliverAskUserForTesting(agent.AskUserRequest{Question: "One?", Options: []string{"a", "b"}})
	m, second := m.DeliverAskUserForTesting(agent.AskUserRequest{Question: "Two?", Options: []string{"a", "b"}})

	m.StopAgentForTesting()

	if _, err, ok := first.Answered(); !ok || err == nil {
		t.Fatal("the on-screen question must be cancelled")
	}
	if _, err, ok := second.Answered(); !ok || err == nil {
		t.Fatal("a queued question must be cancelled too, or its tool call hangs forever")
	}
	if m.QuestionQueueLenForTesting() != 0 {
		t.Fatal("the queue must be cleared on interrupt")
	}
}

// A question that lands after the run has ended cannot be answered by anyone,
// so it must be failed rather than silently dropped.
func TestAskUser_QuestionAfterRunEndsIsFailed(t *testing.T) {
	m := tui.NewModelForTesting()
	m.SetDimensionsForTesting(100, 40)
	m.SetThinkingForTesting(false)

	m, late := m.DeliverAskUserForTesting(agent.AskUserRequest{Question: "Too late?"})

	if _, err, ok := late.Answered(); !ok || err == nil {
		t.Fatal("a question arriving after the run must be answered with an error")
	}
	if m.HasPendingAskUserForTesting() || m.QuestionQueueLenForTesting() != 0 {
		t.Fatal("nothing should be shown for a question the run can no longer use")
	}
}
