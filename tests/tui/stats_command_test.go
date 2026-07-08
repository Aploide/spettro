package tui_test

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"spettro/internal/tui"
)

// /stats on a fresh session (no LLM requests yet) reports that plainly
// instead of a zero-filled table.
func TestHandleCommand_StatsEmptySession(t *testing.T) {
	m := tui.NewModelForTesting()
	m.SetTextareaValueForTesting("/stats")
	m.SetCommandItemsForTesting([]string{"/stats"})

	gotModel, _ := m.UpdateMainForTesting(tea.KeyPressMsg{Code: tea.KeyEnter})
	got := gotModel.(tui.Model)

	messages := got.MessagesForTesting()
	if len(messages) == 0 || !strings.Contains(messages[len(messages)-1].Content, "no LLM requests recorded") {
		t.Fatalf("expected empty stats message, got: %+v", messages)
	}
}
