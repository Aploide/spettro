package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
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

// Found in a VHS screenshot at 80x24: ctrl+b said "activity panel enabled"
// and nothing appeared, as the panel needs 110x15. Below that size the
// banner says where the panel shows instead.
func TestSidePanelBannerOnASmallTerminal(t *testing.T) {
	for _, tc := range []struct {
		w, h int
		want string
	}{
		{80, 24, "activity panel on, shown from 110x15"},
		{120, 12, "activity panel on, shown from 110x15"},
		{120, 40, "activity panel enabled"},
	} {
		m := footerModel(tc.w, tc.h)
		m.messages = append(m.messages, ChatMessage{Role: RoleAssistant, Content: "hello"})
		nm, _ := m.Update(tea.KeyPressMsg{Code: 'b', Mod: tea.ModCtrl})
		m = nm.(Model)
		if m.banner != tc.want {
			t.Errorf("%dx%d: banner %q, want %q", tc.w, tc.h, m.banner, tc.want)
		}
	}
}

// Found in a VHS screenshot at 40x15: the delegation footer ended with
// "… 2 more · ctrl+b for all of them", but the panel cannot be drawn below
// 110x15, so ctrl+b showed nothing. Footers point at ctrl+b only where it
// works.
func TestFooterPointsAtThePanelOnlyWhereItFits(t *testing.T) {
	for _, tc := range []struct {
		w, h int
		want bool
	}{{40, 15, false}, {80, 24, false}, {120, 40, true}} {
		m := footerModel(tc.w, tc.h)
		footerWorkers(&m, 12)
		m = m.recalcLayout()
		plain := ansi.Strip(m.View().Content)
		if !strings.Contains(plain, "more") {
			t.Fatalf("%dx%d: expected a \"… N more\" row:\n%s", tc.w, tc.h, plain)
		}
		if got := strings.Contains(plain, "ctrl+b for"); got != tc.want {
			t.Errorf("%dx%d: ctrl+b hint shown = %v, want %v:\n%s", tc.w, tc.h, got, tc.want, plain)
		}
	}
}
