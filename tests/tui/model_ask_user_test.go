package tui_test

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

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
