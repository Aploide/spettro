package tui

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"spettro/internal/agent"
)

func longTranscript(m *Model, n int) {
	for i := 0; i < n; i++ {
		m.messages = append(m.messages, ChatMessage{Role: RoleAssistant, Content: strings.Repeat("line\n", 5)})
	}
}

// Streaming output keeps the viewport pinned to the bottom while the user is
// following it.
func TestRefreshFollowsWhenAtBottom(t *testing.T) {
	m := footerModel(120, 30)
	longTranscript(&m, 40)
	m.refreshViewport()
	if !m.vp.AtBottom() {
		t.Fatal("viewport should start at the bottom")
	}
	m.messages = append(m.messages, ChatMessage{Role: RoleAssistant, Content: "more\nmore\nmore"})
	m.refreshViewport()
	if !m.vp.AtBottom() {
		t.Fatal("a viewport at the bottom must follow new output")
	}
}

// A user who scrolled up keeps their position while output streams in.
func TestRefreshKeepsPositionWhenScrolledUp(t *testing.T) {
	m := footerModel(120, 30)
	longTranscript(&m, 40)
	m.refreshViewport()
	m.vp.ScrollUp(20)
	offset := m.vp.YOffset()
	m.messages = append(m.messages, ChatMessage{Role: RoleAssistant, Content: "streamed\nstreamed\nstreamed"})
	m.refreshViewport()
	if m.vp.AtBottom() || m.vp.YOffset() != offset {
		t.Fatalf("scrolled-up viewport moved: offset %d -> %d", offset, m.vp.YOffset())
	}
	// Submitting input always brings the conversation back into view.
	m.scrollToBottom()
	if !m.vp.AtBottom() {
		t.Fatal("scrollToBottom must jump to the latest output")
	}
}

// The working indicator appearing takes a row from the transcript; a view
// that was following the latest output keeps following it, so the next
// streamed token is on screen without the user scrolling.
func TestFollowSurvivesTheWorkingIndicator(t *testing.T) {
	m := footerModel(120, 30)
	longTranscript(&m, 40)
	m.refreshViewport()
	height := m.vp.Height()
	m.thinking = true
	m.agentStartAt = time.Now()
	m.SetStreamChForTesting()
	nm, _ := m.Update(tickMsg(time.Now()))
	m = nm.(Model)
	if m.vp.Height() >= height {
		t.Fatalf("the indicator did not take a transcript row (height %d -> %d)", height, m.vp.Height())
	}
	if !m.vp.AtBottom() {
		t.Fatal("the view stopped following when the indicator appeared")
	}
	nm, _ = m.Update(streamChunkMsg{chunk: agent.StreamChunk{Kind: agent.StreamKindAnswer, Delta: "MARKERXYZ"}})
	m = nm.(Model)
	if !strings.Contains(m.vp.View(), "MARKERXYZ") {
		t.Fatalf("the streamed token is not on screen:\n%s", m.vp.View())
	}
}

// Wheel and page keys scroll the new viewport as they did the old one: up
// stops following, back to the bottom resumes it.
func TestWheelAndPageKeysScroll(t *testing.T) {
	m := footerModel(120, 30)
	longTranscript(&m, 40)
	m.refreshViewport()
	bottom := m.vp.YOffset()
	nm, _ := m.Update(tea.MouseWheelMsg{X: 5, Y: 5, Button: tea.MouseWheelUp})
	m = nm.(Model)
	if m.vp.YOffset() != bottom-3 {
		t.Fatalf("wheel up moved %d rows, want 3", bottom-m.vp.YOffset())
	}
	nm, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyPgUp})
	m = nm.(Model)
	if m.vp.YOffset() != max(bottom-3-m.vp.Height(), 0) {
		t.Fatalf("pgup left the offset at %d", m.vp.YOffset())
	}
	m.messages = append(m.messages, ChatMessage{Role: RoleAssistant, Content: "while scrolled up"})
	m.refreshViewport()
	if m.vp.AtBottom() {
		t.Fatal("a scrolled-up view followed new output")
	}
	for !m.vp.AtBottom() {
		nm, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyPgDown})
		m = nm.(Model)
	}
	m.messages = append(m.messages, ChatMessage{Role: RoleAssistant, Content: "after returning"})
	m.refreshViewport()
	if !m.vp.AtBottom() {
		t.Fatal("following did not resume at the bottom")
	}
}
