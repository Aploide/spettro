package tui

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// eyeArtRow is a row of the acting art distinctive enough to find in a frame.
func eyeArtRow() string { return eyesClosed[6] }

func TestEyesIntroWaitsForTrustThenAdvances(t *testing.T) {
	m := footerModel(120, 40)
	m.showTrust = true
	next, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m = next.(Model)
	if m.eyeIntroStarted || m.needsAnimation() {
		t.Fatalf("the intro must wait while the trust prompt is visible")
	}
	closed := ansi.Strip(m.renderMessages())
	if !strings.Contains(closed, eyesClosed[5]) {
		t.Fatalf("initial transcript must render the closed eye frame")
	}
	next, _ = m.Update(tea.KeyPressMsg{Code: '1'})
	m = next.(Model)
	if !m.eyeIntroStarted || !m.needsAnimation() {
		t.Fatalf("accepting trust must start the intro animation")
	}
	for range 3 {
		next, _ = m.Update(tickMsg(time.Now()))
		m = next.(Model)
	}
	if m.eyeIntroFrame != 3 {
		t.Fatalf("startup ticks advanced to frame %d, want 3", m.eyeIntroFrame)
	}
	if opened := ansi.Strip(m.renderMessages()); opened == closed {
		t.Fatalf("startup ticks must repaint the intro animation")
	}
	if strings.Join(eyeFrameArt(10), "\n") != strings.Join(eyeFrameArt(15), "\n") {
		t.Fatalf("aggressive eyes should hold visibly before the normal frame")
	}
	if strings.Join(eyeFrameArt(15), "\n") == strings.Join(eyeFrameArt(24), "\n") {
		t.Fatalf("the animation should finish on the normal eyes")
	}
}

func TestIdleEyesBlinkStopsOnFirstUserMessage(t *testing.T) {
	m := footerModel(120, 40)
	m.ready = true
	m.eyeIntroStarted = true
	m.eyeIntroFrame = eyeIntroFrames - 1
	next, _ := m.Update(tickMsg(time.Now()))
	m = next.(Model)
	if !m.idleEyesArmed || m.eyeIntroFrame != eyeIntroFrames {
		t.Fatalf("finishing the intro should arm the idle blink")
	}
	next, _ = m.Update(idleEyesTickMsg{})
	m = next.(Model)
	if m.idleEyesFrame != 1 || m.eyeBannerFrame() != eyeIntroFrames+1 {
		t.Fatalf("the idle blink did not advance its first frame")
	}
	m.messages = append(m.messages, ChatMessage{Role: RoleUser, Content: "first message"})
	m.refreshViewport()
	if m.eyeBannerFrame() != eyeIntroFrames {
		t.Fatalf("the logo should return to normal after the first user message")
	}
	next, _ = m.Update(idleEyesTickMsg{})
	m = next.(Model)
	if m.idleEyesArmed || m.idleEyesFrame != 0 {
		t.Fatalf("the idle blink should stop after the first user message")
	}
}

// The logo opens the scrollback instead of sitting above it: it is the first
// thing renderMessages emits, both with and without a transcript, and it is
// not part of the frame's fixed chrome any more.
func TestEyesBannerOpensTheScrollback(t *testing.T) {
	m := footerModel(120, 40)

	empty := m.renderMessages()
	if !strings.HasPrefix(ansi.Strip(empty), strings.Repeat(" ", (m.transcriptWidth()-eyeArtWidth)/2)+eyesClosed[0]) {
		t.Fatalf("an empty transcript must still open with the logo:\n%q", ansi.Strip(empty))
	}
	if !strings.Contains(empty, "no messages yet") {
		t.Fatalf("the empty-state hint belongs under the logo, not instead of it:\n%s", empty)
	}

	m.messages = append(m.messages, ChatMessage{Role: RoleUser, Content: "hello"})
	body := ansi.Strip(m.renderMessages())
	if idx := strings.Index(body, eyeArtRow()); idx < 0 {
		t.Fatalf("the logo must lead the transcript:\n%s", body)
	} else if idx > strings.Index(body, "hello") {
		t.Fatalf("the logo must come before the first message:\n%s", body)
	}
}

// The banner frame is stable until the startup animation advances, keeping
// repeated renders byte-identical.
func TestEyesBannerIsStatic(t *testing.T) {
	m := footerModel(120, 40)
	m.messages = append(m.messages, ChatMessage{Role: RoleUser, Content: "hello"})

	m.eyeFrame = 0
	first := m.renderMessages()
	m.eyeIntroFrame = 0
	m.thinking = true
	second := m.renderMessages()

	if first != second {
		t.Fatalf("the inline logo must not animate with the frame counter")
	}
}

// Nothing above the conversation pane draws the eyes any more, and once the
// transcript is long enough the logo scrolls out of the frame entirely.
func TestEyesAreNotInTheFixedFrame(t *testing.T) {
	m := footerModel(120, 30)
	m.messages = append(m.messages, ChatMessage{Role: RoleUser, Content: "hello"})
	m = m.recalcLayout()
	m.refreshViewport()

	frame := ansi.Strip(m.View().Content)
	lines := strings.Split(frame, "\n")
	if strings.Contains(lines[0], eyeArtRow()) || strings.Contains(lines[1], eyeArtRow()) {
		t.Fatalf("the logo must not sit between the header and the pane:\n%s", frame)
	}

	for i := range 40 {
		m.messages = append(m.messages, ChatMessage{Role: RoleAssistant, Content: strings.Repeat("filler ", 20) + string(rune('a'+i))})
	}
	m = m.recalcLayout()
	m.refreshViewport()
	if strings.Contains(ansi.Strip(m.View().Content), eyeArtRow()) {
		t.Fatalf("the logo should have scrolled away once the chat filled the pane")
	}
}

// The eight rows the overlay used to reserve now belong to the conversation.
func TestViewportGainsTheEyeRows(t *testing.T) {
	m := footerModel(120, 40)
	if got, want := m.vp.Height(), 40-1-2-6-1; got != want {
		t.Fatalf("viewport height = %d, want %d (no eye rows reserved)", got, want)
	}

	m.thinking = true
	m.agentStartAt = time.Now()
	m = m.recalcLayout()
	if got, want := m.vp.Height(), 40-1-2-6-1-1; got != want {
		t.Fatalf("viewport height while working = %d, want %d (one indicator row)", got, want)
	}
}

// The banner lives inside the viewport, which only repaints when its content
// is rebuilt — so every mode switch has to rebuild it or the logo keeps the
// previous mode's face and accent while the rest of the frame has moved on.
func TestEyesBannerFollowsAModeSwitch(t *testing.T) {
	m := footerModel(120, 40)
	m.messages = append(m.messages, ChatMessage{Role: RoleUser, Content: "hello"})
	m.refreshViewport()

	next, _ := m.updateMain(tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift})
	m = next.(Model)
	if !isPlanningEyeMode(m.mode) {
		t.Skipf("shift+tab landed on %q, which shares the acting art", m.mode)
	}

	frame := ansi.Strip(m.View().Content)
	if !strings.Contains(frame, eyesClosed[6]) {
		t.Fatalf("the shared logo must remain visible after the mode switch:\n%s", frame)
	}
}

// The art is cropped, never wrapped: a pane too narrow for the full grid still
// gets exactly eight rows, centred on the ink rather than on the blank gutter.
func TestEyesBannerFitsNarrowPanes(t *testing.T) {
	for _, width := range []int{30, 46, 80, 99, 120} {
		art := renderEyesStatic("coding", width)
		if got := lipgloss.Height(art); got != len(eyesNormal) {
			t.Fatalf("width %d: %d rows, want %d", width, got, len(eyesNormal))
		}
		if got := lipgloss.Width(art); got > width {
			t.Fatalf("width %d: art is %d columns wide", width, got)
		}
	}
}

// A fresh session is nothing but the logo, so the viewport must not scroll
// past it on a terminal too short to show the whole block.
func TestEyesBannerIsPinnedToTopOnAFreshSession(t *testing.T) {
	m := footerModel(120, 18)
	m.refreshViewport()

	if !strings.Contains(ansi.Strip(m.View().Content), eyesClosed[0]) {
		t.Fatalf("the top of the logo must be visible on a fresh session:\n%s", ansi.Strip(m.View().Content))
	}
}
