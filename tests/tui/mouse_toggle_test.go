package tui_test

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"spettro/internal/session"
	"spettro/internal/tui"
)

func TestCtrlT_TogglesMouseCapture(t *testing.T) {
	m := tui.NewModelForTesting()
	m.SetDimensionsForTesting(120, 30)
	m.MarkReadyAndTrustedForTesting()

	if m.MouseCaptureOffForTesting() {
		t.Fatal("expected mouse capture to be on by default")
	}

	next, _ := m.UpdateForTesting(tea.KeyMsg{Type: tea.KeyCtrlT})
	model := next.(tui.Model)
	if !model.MouseCaptureOffForTesting() {
		t.Fatal("expected ctrl+t to disable mouse capture")
	}
	if !strings.Contains(model.BannerForTesting(), "text-select") {
		t.Fatalf("expected banner to mention text-select mode, got %q", model.BannerForTesting())
	}

	next, _ = model.UpdateForTesting(tea.KeyMsg{Type: tea.KeyCtrlT})
	model = next.(tui.Model)
	if model.MouseCaptureOffForTesting() {
		t.Fatal("expected ctrl+t to re-enable mouse capture")
	}
}

func TestMouseEvents_IgnoredWhenCaptureOff(t *testing.T) {
	m := tui.NewModelForTesting()
	m.SetDimensionsForTesting(110, 22)
	m.SetShowResumeForTesting(true)
	m.SetResumeItemsForTesting([]session.Summary{
		{ID: "1", StartedAt: time.Now()},
		{ID: "2", StartedAt: time.Now()},
		{ID: "3", StartedAt: time.Now()},
	})

	off, _ := m.UpdateForTesting(tea.KeyMsg{Type: tea.KeyCtrlT})
	model := off.(tui.Model)
	if !model.MouseCaptureOffForTesting() {
		t.Fatal("expected mouse capture off after ctrl+t")
	}

	startCursor := model.ResumeCursorForTesting()
	next, _ := model.UpdateForTesting(tea.MouseMsg{Button: tea.MouseButtonWheelDown})
	after := next.(tui.Model)
	if after.ResumeCursorForTesting() != startCursor {
		t.Fatalf("expected mouse wheel to be ignored while capture is off, cursor moved %d→%d",
			startCursor, after.ResumeCursorForTesting())
	}
}
