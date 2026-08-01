package tui_test

import (
	"testing"

	tea "charm.land/bubbletea/v2"

	"spettro/internal/tui"
)

// A left press followed by motion must enter drag-selection, and the release
// must finalize it (clearing the drag state) while mouse capture stays on.
func TestDragSelection_Lifecycle(t *testing.T) {
	m := tui.NewModelForTesting()
	m.SetDimensionsForTesting(120, 30)
	m.MarkReadyAndTrustedForTesting()

	next, _ := m.UpdateForTesting(tea.MouseClickMsg{Button: tea.MouseLeft, X: 5, Y: 8})
	model := next.(tui.Model)
	if model.TextSelectionDraggingForTesting() {
		t.Fatal("a bare click must not start a drag selection")
	}

	next, _ = model.UpdateForTesting(tea.MouseMotionMsg{Button: tea.MouseLeft, X: 20, Y: 10})
	model = next.(tui.Model)
	if !model.TextSelectionDraggingForTesting() {
		t.Fatal("expected motion after a left press to start a drag selection")
	}

	next, _ = model.UpdateForTesting(tea.MouseReleaseMsg{Button: tea.MouseLeft, X: 20, Y: 10})
	model = next.(tui.Model)
	if model.TextSelectionDraggingForTesting() {
		t.Fatal("expected release to end the drag selection")
	}
	if model.MouseCaptureOffForTesting() {
		t.Fatal("drag selection must not disable mouse capture")
	}
}

// Motion without a preceding left press (e.g. hover reporting) must not start
// a selection.
func TestDragSelection_MotionWithoutPressIgnored(t *testing.T) {
	m := tui.NewModelForTesting()
	m.SetDimensionsForTesting(120, 30)
	m.MarkReadyAndTrustedForTesting()

	next, _ := m.UpdateForTesting(tea.MouseMotionMsg{X: 20, Y: 10})
	model := next.(tui.Model)
	if model.TextSelectionDraggingForTesting() {
		t.Fatal("motion without a left press must not start a selection")
	}
}

// Scrolling moves content underneath a screen-space selection, so the wheel
// must drop an armed selection instead of copying stale coordinates.
func TestDragSelection_WheelCancels(t *testing.T) {
	m := tui.NewModelForTesting()
	m.SetDimensionsForTesting(120, 30)
	m.MarkReadyAndTrustedForTesting()

	next, _ := m.UpdateForTesting(tea.MouseClickMsg{Button: tea.MouseLeft, X: 5, Y: 8})
	model := next.(tui.Model)
	next, _ = model.UpdateForTesting(tea.MouseMotionMsg{Button: tea.MouseLeft, X: 20, Y: 10})
	model = next.(tui.Model)
	next, _ = model.UpdateForTesting(tea.MouseWheelMsg{Button: tea.MouseWheelDown})
	model = next.(tui.Model)
	if model.TextSelectionDraggingForTesting() {
		t.Fatal("expected wheel scroll to cancel the drag selection")
	}
}

func TestExtractSelection_StreamSemantics(t *testing.T) {
	frame := "alpha beta\ngamma delta\nepsilon zeta"

	// Single line, mid-word to mid-word (end cell inclusive).
	if got := tui.ExtractSelectionForTesting(frame, 6, 0, 9, 0); got != "beta" {
		t.Fatalf("single-line selection = %q, want %q", got, "beta")
	}

	// Multi-line: first line from anchor to EOL, last line from col 0 to end.
	want := "beta\ngamma"
	if got := tui.ExtractSelectionForTesting(frame, 6, 0, 4, 1); got != want {
		t.Fatalf("multi-line selection = %q, want %q", got, want)
	}

	// Backwards drag (end before start) must normalize to reading order.
	if got := tui.ExtractSelectionForTesting(frame, 4, 1, 6, 0); got != want {
		t.Fatalf("backwards selection = %q, want %q", got, want)
	}
}
