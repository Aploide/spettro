//go:build !spettro_bubblesviewport

package tui

import (
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// anchorTranscript fills m with n assistant messages whose every word names
// the message ("m07w"), so any row of a message's block says which message
// it belongs to, however the block wraps.
func anchorTranscript(m *Model, n int) {
	for i := 0; i < n; i++ {
		m.messages = append(m.messages, ChatMessage{Role: RoleAssistant,
			Content: strings.TrimSpace(strings.Repeat(fmt.Sprintf("m%02dw ", i), 120))})
	}
}

// topMessage is the message the top transcript row belongs to, or "" for a
// row of no message (the logo, a separator).
func topMessage(m Model) string {
	top := ansi.Strip(strings.SplitN(m.vp.View(), "\n", 2)[0])
	for _, word := range strings.Fields(top) {
		if len(word) == 4 && word[0] == 'm' && word[3] == 'w' {
			return word
		}
	}
	return ""
}

// scrolledToMessage scrolls m up until the top row is the third row of
// message "m<idx>w".
func scrolledToMessage(t *testing.T, m *Model, idx int) {
	t.Helper()
	want := fmt.Sprintf("m%02dw", idx)
	m.vp.GotoBottom()
	for !m.vp.AtTop() && topMessage(*m) != want {
		m.vp.ScrollUp(1)
	}
	for !m.vp.AtTop() && topMessage(*m) == want {
		m.vp.ScrollUp(1)
	}
	m.vp.ScrollDown(3)
	if topMessage(*m) != want {
		t.Fatalf("could not put message %s at the top row:\n%s", want, ansi.Strip(m.vp.View()))
	}
}

// A user reading the scrollback keeps reading the same message when the
// terminal is resized: the transcript is rewrapped (more or fewer rows above
// the place they were reading), and the view used to keep the row number,
// showing some other message, or clamp to the bottom and start following
// the stream again.
func TestResizeKeepsTheScrolledUpPlace(t *testing.T) {
	for _, size := range [][2]int{{70, 30}, {200, 50}, {45, 14}, {120, 40}} {
		m := footerModel(120, 40)
		anchorTranscript(&m, 30)
		settledRefresh(&m)
		scrolledToMessage(t, &m, 12)
		nm, _ := m.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		m = nm.(Model)
		settledRefresh(&m)
		if got := topMessage(m); got != "m12w" {
			t.Errorf("after a resize to %dx%d the top row shows %q, want m12w:\n%s", size[0], size[1], got, ansi.Strip(m.vp.View()))
		}
		if m.vp.AtBottom() {
			t.Errorf("after a resize to %dx%d the scrolled-up view jumped to the bottom", size[0], size[1])
		}
	}
}

// Toggling the side panel narrows or widens the transcript; the message
// being read stays at the top.
func TestSidePanelToggleKeepsTheScrolledUpPlace(t *testing.T) {
	m := footerModel(160, 40)
	anchorTranscript(&m, 30)
	settledRefresh(&m)
	scrolledToMessage(t, &m, 9)
	for i := 0; i < 2; i++ {
		nm, _ := m.Update(tea.KeyPressMsg{Code: 'b', Mod: tea.ModCtrl})
		m = nm.(Model)
		settledRefresh(&m)
		if got := topMessage(m); got != "m09w" {
			t.Fatalf("after ctrl+b #%d (panel width %d) the top row shows %q, want m09w", i+1, m.sidePanelWidth(), got)
		}
	}
}

// A block growing at its end (the streamed answer the user is reading while
// it arrives) does not move the view, and new messages below do not either.
func TestGrowingBlockDoesNotMoveTheScrolledUpView(t *testing.T) {
	m := footerModel(100, 30)
	anchorTranscript(&m, 20)
	m.messages[19].Content = strings.TrimSpace(strings.Repeat("m19w ", 800))
	settledRefresh(&m)
	scrolledToMessage(t, &m, 19)
	before := ansi.Strip(m.vp.View())
	m.messages[19].Content += strings.Repeat(" m19w", 80)
	m.refreshViewport()
	m.messages = append(m.messages, ChatMessage{Role: RoleAssistant, Content: "new"})
	m.refreshViewport()
	if got := ansi.Strip(m.vp.View()); got != before {
		t.Fatalf("the scrolled-up view moved while its block grew:\n--- before ---\n%s\n--- after ---\n%s", before, got)
	}
}
