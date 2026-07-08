package tui_test

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"spettro/internal/tui"
)

// Selecting "/jobs kill" from the completion menu must complete into the
// input and wait for the job ID, not execute (which would just error).
func TestUpdateMain_JobsKillCompletionWaitsForArgument(t *testing.T) {
	m := tui.NewModelForTesting()
	m.SetTextareaValueForTesting("/jobs")
	m.SetCommandItemsForTesting([]string{"/jobs kill"})

	gotModel, _ := m.UpdateMainForTesting(tea.KeyPressMsg{Code: tea.KeyEnter})
	got := gotModel.(tui.Model)

	if got.TextareaValueForTesting() != "/jobs kill " {
		t.Fatalf("expected completion into input, got %q", got.TextareaValueForTesting())
	}
	for _, msg := range got.MessagesForTesting() {
		if strings.Contains(msg.Content, "usage:") {
			t.Fatalf("command executed instead of waiting for argument: %+v", msg)
		}
	}
}

func TestHandleCommand_JobsListEmpty(t *testing.T) {
	m := tui.NewModelForTesting()
	m.SetTextareaValueForTesting("/jobs")
	m.SetCommandItemsForTesting([]string{"/jobs"})

	gotModel, _ := m.UpdateMainForTesting(tea.KeyPressMsg{Code: tea.KeyEnter})
	got := gotModel.(tui.Model)

	messages := got.MessagesForTesting()
	if len(messages) == 0 || !strings.Contains(messages[len(messages)-1].Content, "no background jobs") {
		t.Fatalf("expected empty jobs listing, got: %+v", messages)
	}
}
