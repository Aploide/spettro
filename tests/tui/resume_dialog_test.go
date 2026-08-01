package tui_test

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"spettro/internal/session"
	"spettro/internal/tui"
)

func TestViewResume_IsHeightBoundedAndPreviewsDoNotWrap(t *testing.T) {
	m := tui.NewModelForTesting()
	m.SetDimensionsForTesting(110, 22)
	m.SetShowResumeForTesting(true)

	items := make([]session.Summary, 0, 24)
	for i := range 24 {
		items = append(items, session.Summary{
			ID:        "s",
			StartedAt: time.Date(2026, 3, 16, 3, i%60, 0, 0, time.UTC),
			UpdatedAt: time.Now(),
			Preview:   strings.Repeat("very long preview entry ", 8),
		})
	}
	m.SetResumeItemsForTesting(items)

	view := m.ViewForTesting()
	if got := lipgloss.Height(view); got > 22 {
		t.Fatalf("expected resume dialog to stay within terminal height, got %d", got)
	}
	if strings.Contains(view, "very long preview entry very long preview entry very long preview entry very long preview entry") {
		t.Fatalf("expected long previews to be truncated to one row, got: %s", view)
	}
}

func TestResumeDialog_MouseWheelMovesSelection(t *testing.T) {
	m := tui.NewModelForTesting()
	m.SetDimensionsForTesting(110, 22)
	m.SetShowResumeForTesting(true)
	m.SetResumeItemsForTesting([]session.Summary{
		{ID: "1", StartedAt: time.Now()},
		{ID: "2", StartedAt: time.Now()},
		{ID: "3", StartedAt: time.Now()},
	})

	nextModel, _ := m.UpdateForTesting(tea.MouseWheelMsg{Button: tea.MouseWheelDown})
	next := nextModel.(tui.Model)
	if next.ResumeCursorForTesting() != 1 {
		t.Fatalf("expected resume cursor to move on wheel down, got %d", next.ResumeCursorForTesting())
	}
}
