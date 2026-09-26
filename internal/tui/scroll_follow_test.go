package tui

import (
	"strings"
	"testing"
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
