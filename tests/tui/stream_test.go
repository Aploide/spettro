package tui_test

import (
	"strings"
	"testing"

	"spettro/internal/agent"
	"spettro/internal/tui"
)

// TestStreaming_ThinkingAndAnswerRenderLive verifies that thinking and answer
// deltas accumulate into live transient messages while a run is in progress.
func TestStreaming_ThinkingAndAnswerRenderLive(t *testing.T) {
	m := tui.NewModelForTesting()
	m.SetThinkingForTesting(true)
	m.SetStreamChForTesting()

	feed := func(kind, delta string, reset bool) {
		updated, _ := m.UpdateForTesting(tui.StreamChunkMsgForTesting(kind, delta, reset))
		m = updated.(tui.Model)
	}

	feed(agent.StreamKindThinking, "let me think ", false)
	feed(agent.StreamKindThinking, "about it", false)
	feed(agent.StreamKindAnswer, "Here ", false)
	feed(agent.StreamKindAnswer, "is the answer", false)

	msgs := m.MessagesForTesting()
	var think, answer string
	for _, msg := range msgs {
		switch msg.Kind {
		case "thinking-stream":
			think = msg.Content
		case "answer-stream":
			answer = msg.Content
		}
	}
	if think != "let me think about it" {
		t.Fatalf("live thinking = %q, want %q", think, "let me think about it")
	}
	if answer != "Here is the answer" {
		t.Fatalf("live answer = %q, want %q", answer, "Here is the answer")
	}
}

// TestStreaming_CompletionReplacesDrafts verifies that the transient live-stream
// messages are removed at completion and the streamed reasoning is folded into
// the authoritative final message.
func TestStreaming_CompletionReplacesDrafts(t *testing.T) {
	m := tui.NewModelForTesting()
	m.SetThinkingForTesting(true)
	m.SetStreamChForTesting()

	feed := func(kind, delta string, reset bool) {
		updated, _ := m.UpdateForTesting(tui.StreamChunkMsgForTesting(kind, delta, reset))
		m = updated.(tui.Model)
	}
	feed(agent.StreamKindThinking, "reasoning trace", false)
	feed(agent.StreamKindAnswer, "draft answer", false)

	updated, _ := m.UpdateForTesting(tui.AgentDoneMsgForTesting("final answer"))
	m = updated.(tui.Model)

	for _, msg := range m.MessagesForTesting() {
		if msg.Kind == "thinking-stream" || msg.Kind == "answer-stream" {
			t.Fatalf("transient %q message survived completion", msg.Kind)
		}
	}

	final := m.MessagesForTesting()[len(m.MessagesForTesting())-1]
	if !strings.Contains(final.Content, "final answer") {
		t.Fatalf("final content = %q, want it to contain %q", final.Content, "final answer")
	}
	if final.Thinking != "reasoning trace" {
		t.Fatalf("final thinking = %q, want %q", final.Thinking, "reasoning trace")
	}
}

// TestStreaming_AnswerResetClearsDraft verifies that an answer Reset discards the
// current answer draft (used when a step turns out to be a tool call).
func TestStreaming_AnswerResetClearsDraft(t *testing.T) {
	m := tui.NewModelForTesting()
	m.SetThinkingForTesting(true)
	m.SetStreamChForTesting()

	feed := func(kind, delta string, reset bool) {
		updated, _ := m.UpdateForTesting(tui.StreamChunkMsgForTesting(kind, delta, reset))
		m = updated.(tui.Model)
	}
	feed(agent.StreamKindAnswer, "stale preamble", false)
	feed(agent.StreamKindAnswer, "", true) // reset, no new text

	for _, msg := range m.MessagesForTesting() {
		if msg.Kind == "answer-stream" {
			t.Fatalf("answer draft survived reset: %q", msg.Content)
		}
	}
}
