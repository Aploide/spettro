package tui

import (
	"testing"

	"spettro/internal/config"
)

// TestQueuedPromptRunsAfterAgentDone is a regression test: a prompt typed
// while an agent run is in flight gets queued, and once that run reports
// agentDoneMsg the queued prompt must actually start running (not just have
// its start-run tea.Cmd fired into the void while the model itself keeps
// stale state). Previously the continuation branch in update() discarded the
// updated Model returned by maybeRunNextQueuedPrompt, so m.thinking flipped
// back to true only in a throwaway copy: the real model stayed "idle" and
// the queued request's own completion message would later be dropped by the
// `if !m.thinking { break }` guard.
func TestQueuedPromptRunsAfterAgentDone(t *testing.T) {
	m := NewModelForTesting()
	m.manifest = config.DefaultAgentManifest()
	m.mode = "ask"
	m.thinking = true

	m.queuePrompt("second request", "second request", nil, nil)
	if got := m.PendingPromptCountForTesting(); got != 1 {
		t.Fatalf("pending prompts = %d, want 1", got)
	}

	out, cmd := m.update(agentDoneMsg{content: "first response"})
	nm := out.(Model)

	if cmd == nil {
		t.Fatalf("expected a non-nil cmd to start the queued prompt")
	}
	if got := nm.PendingPromptCountForTesting(); got != 0 {
		t.Fatalf("pending prompts after continuation = %d, want 0", got)
	}
	if !nm.ThinkingForTesting() {
		t.Fatalf("model should be thinking again: the queued prompt's run must have started")
	}
	msgs := nm.MessagesForTesting()
	if len(msgs) == 0 || msgs[len(msgs)-1].Content != "second request" {
		t.Fatalf("expected the queued request to be appended as the latest message, got %+v", msgs)
	}
}
