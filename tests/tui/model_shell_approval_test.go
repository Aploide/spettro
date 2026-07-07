package tui_test

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"spettro/internal/tui"
)

func TestShellApprovalOptions_ContainExpectedChoices(t *testing.T) {
	options := tui.ShellApprovalOptionsForTesting()
	if len(options) < 3 {
		t.Fatalf("expected at least 3 approval options, got %d", len(options))
	}
	joined := strings.Join(options, "\n")
	for _, want := range []string{"Allow once", "Allow always", "Deny"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("approval options missing %q: %v", want, options)
		}
	}
}

func TestUpdateShellApproval_Option3EntersAlternativeTextMode(t *testing.T) {
	m := tui.NewModelForTesting()
	m.SetPendingShellApprovalForTesting(3)

	gotModel, _ := m.UpdateShellApprovalForTesting(tea.KeyPressMsg{Code: 'y', Text: "y"})
	got := gotModel.(tui.Model)
	if !got.HasPendingShellApprovalForTesting() {
		t.Fatal("pending approval should not resolve when typing alternative text")
	}
	if strings.TrimSpace(got.TextareaValueForTesting()) != "y" {
		t.Fatalf("expected typed text to stay in textarea, got %q", got.TextareaValueForTesting())
	}
}

func TestUpdateShellApproval_DownMovesCursor(t *testing.T) {
	m := tui.NewModelForTesting()
	m.SetPendingShellApprovalForTesting(0)

	gotModel, _ := m.UpdateShellApprovalForTesting(tea.KeyPressMsg{Code: tea.KeyDown})
	got := gotModel.(tui.Model)
	if got.ApprovalCursorForTesting() != 1 {
		t.Fatalf("expected cursor 1 after down, got %d", got.ApprovalCursorForTesting())
	}
}
