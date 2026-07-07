package tui_test

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"spettro/internal/tui"
)

func TestUpdateMain_SecondEnterExecutesSelectedCommand(t *testing.T) {
	m := tui.NewModelForTesting()
	m.SetTextareaValueForTesting("/help ")
	m.SetCommandItemsForTesting([]string{"/help"})

	gotModel, _ := m.UpdateMainForTesting(tea.KeyPressMsg{Code: tea.KeyEnter})
	got := gotModel.(tui.Model)

	messages := got.MessagesForTesting()
	if len(messages) == 0 || !strings.Contains(messages[len(messages)-1].Content, "commands:") {
		t.Fatalf("expected /help to execute on second enter; got messages: %+v", messages)
	}
	if strings.TrimSpace(got.TextareaValueForTesting()) != "" {
		t.Fatalf("expected input cleared after execution, got %q", got.TextareaValueForTesting())
	}
}

func TestUpdateMain_UpArrowRecallsPreviousInput(t *testing.T) {
	m := tui.NewModelForTesting()
	m.SetInputHistoryForTesting([]string{"first prompt", "second prompt"})

	gotModel, _ := m.UpdateMainForTesting(tea.KeyPressMsg{Code: tea.KeyUp})
	got := gotModel.(tui.Model)

	if got.TextareaValueForTesting() != "second prompt" {
		t.Fatalf("expected latest prompt recalled, got %q", got.TextareaValueForTesting())
	}

	gotModel, _ = got.UpdateMainForTesting(tea.KeyPressMsg{Code: tea.KeyUp})
	got = gotModel.(tui.Model)

	if got.TextareaValueForTesting() != "first prompt" {
		t.Fatalf("expected older prompt after second up, got %q", got.TextareaValueForTesting())
	}
}

func TestUpdateMain_DownArrowRestoresDraftAfterHistory(t *testing.T) {
	m := tui.NewModelForTesting()
	m.SetTextareaValueForTesting("draft")
	m.SetInputHistoryForTesting([]string{"first prompt", "second prompt"})

	gotModel, _ := m.UpdateMainForTesting(tea.KeyPressMsg{Code: tea.KeyUp})
	got := gotModel.(tui.Model)
	gotModel, _ = got.UpdateMainForTesting(tea.KeyPressMsg{Code: tea.KeyDown})
	got = gotModel.(tui.Model)

	if got.TextareaValueForTesting() != "draft" {
		t.Fatalf("expected original draft restored, got %q", got.TextareaValueForTesting())
	}
	if got.HistoryBrowsingForTesting() {
		t.Fatal("expected history browsing mode to exit")
	}
}
