package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// Found in a VHS screenshot with the activity panel open: the column between
// the transcript and the panel held a single "│" on its first row, a stray
// tick at the end of the transcript's top rule. The gutter is blank on every
// row now; the panel has its own border.
func TestSidePanelGutterIsBlank(t *testing.T) {
	for _, width := range []int{110, 120, 200} {
		m := hugeTranscriptModel(width, 40)
		m.showSidePanel = true
		m = m.recalcLayout()
		m.refreshViewport()
		if m.sidePanelWidth() <= 0 {
			t.Fatalf("width %d: the panel is not drawn", width)
		}
		frame := m.View().Content
		assertFrameFits(t, "side panel", frame, width, 40)
		gutter := m.paneWidth()
		for i, row := range strings.Split(frame, "\n")[1:] {
			if cell := ansi.Strip(ansi.Cut(row, gutter, gutter+1)); cell != " " {
				t.Fatalf("width %d: gutter cell on body row %d is %q, want blank:\n%s", width, i, cell, ansi.Strip(frame))
			}
		}
	}
}
