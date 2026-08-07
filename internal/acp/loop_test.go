package acp

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	acpsdk "github.com/coder/acp-go-sdk"

	"spettro/internal/config"
	"spettro/internal/provider"
)

// newLoopTestBridge builds a bridge with a live agent-side connection whose
// outgoing notifications are captured in the returned buffer.
func newLoopTestBridge(t *testing.T) (*bridge, *acpSession, *syncBuffer) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())

	manifest := config.AgentManifest{Agents: []config.AgentSpec{{
		ID:      "coding",
		Mode:    "worker",
		Enabled: true,
	}}}
	b := newBridge(Options{
		CWD:       t.TempDir(),
		GlobalDir: t.TempDir(),
		Providers: provider.NewManager(),
		Manifest:  manifest,
	})
	out := &syncBuffer{}
	pr, pw := io.Pipe()
	t.Cleanup(func() { _ = pw.Close() })
	b.conn = acpsdk.NewAgentSideConnection(b, out, pr)

	s := &acpSession{
		id:                "sess-loop",
		cwd:               t.TempDir(),
		agentID:           "coding",
		manifest:          manifest,
		mediaDir:          t.TempDir(),
		startedAt:         time.Now(),
		commandsAnnounced: true,
	}
	b.sessions[s.id] = s
	return b, s, out
}

// promptLoop sends a /loop prompt turn and waits for the reply text to land
// on the wire.
func promptLoop(t *testing.T, b *bridge, s *acpSession, out *syncBuffer, input, want string) {
	t.Helper()
	resp, err := b.Prompt(context.Background(), acpsdk.PromptRequest{
		SessionId: acpsdk.SessionId(s.id),
		Prompt:    []acpsdk.ContentBlock{acpsdk.TextBlock(input)},
	})
	if err != nil {
		t.Fatalf("%q: prompt error: %v", input, err)
	}
	if resp.StopReason != acpsdk.StopReasonEndTurn {
		t.Fatalf("%q: stop reason = %v", input, resp.StopReason)
	}
	deadline := time.After(3 * time.Second)
	for !strings.Contains(out.String(), want) {
		select {
		case <-deadline:
			t.Fatalf("%q: reply %q never hit the wire; output:\n%s", input, want, out.String())
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// /loop argument validation resolves in one turn with a text reply, without
// ever starting an agent run.
func TestLoopCommand_Validation(t *testing.T) {
	b, s, out := newLoopTestBridge(t)
	// The wire form is JSON-encoded, so match a fragment without angle
	// brackets (they are escaped to \u003c on the wire).
	promptLoop(t, b, s, out, "/loop", "usage: /loop")
	promptLoop(t, b, s, out, "/loop 5m", "usage: /loop")
	promptLoop(t, b, s, out, "/loop nope check things", "invalid interval")
	promptLoop(t, b, s, out, "/loop 1s check things", "below the minimum")
	promptLoop(t, b, s, out, "/loop 5m /loop 5m hi", "cannot itself be /loop")
}

func TestLoopCommand_StatusAndStopWhenIdle(t *testing.T) {
	b, s, out := newLoopTestBridge(t)
	promptLoop(t, b, s, out, "/loop status", "no loop has run in this session")
	promptLoop(t, b, s, out, "/loop stop", "no loop is running")

	// A stored outcome summary is what a later /loop status reports.
	b.mu.Lock()
	s.lastLoop = "loop: check things\nevery: 5m0s\noutcome: stopped by user\niterations: 3, elapsed: 15m0s"
	b.mu.Unlock()
	promptLoop(t, b, s, out, "/loop status", "iterations: 3")
}
