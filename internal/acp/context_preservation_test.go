package acp

// Regression test for the context-loss bug: a failed or interrupted prompt
// turn must NOT wipe the session's context. Before the fix, runToolLoop
// returned no messages on error and the bridge only persisted history and
// transcript on success, so a single provider failure or user cancel
// restarted the conversation from scratch.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	acpsdk "github.com/coder/acp-go-sdk"

	"spettro/internal/config"
	"spettro/internal/provider"
	"spettro/internal/session"
)

// newFailingServer answers the first request with a good response and every
// later request with a 500 that never recovers (retries and fallback offers
// exhaust, failing the run).
func newFailingServer(t *testing.T, firstResponse string) *httptest.Server {
	t.Helper()
	idx := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		i := idx
		idx++
		if i > 0 {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		chunk := map[string]any{
			"id": "chatcmpl-test", "object": "chat.completion.chunk", "model": "fake-model",
			"choices": []map[string]any{
				{"index": 0, "delta": map[string]any{"role": "assistant", "content": firstResponse}},
			},
		}
		done := map[string]any{
			"id": "chatcmpl-test", "object": "chat.completion.chunk", "model": "fake-model",
			"choices": []map[string]any{
				{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"},
			},
			"usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 20, "total_tokens": 30},
		}
		for _, ev := range []map[string]any{chunk, done} {
			raw, _ := json.Marshal(ev)
			fmt.Fprintf(w, "data: %s\n\n", raw)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestPrompt_FailedTurnPreservesContext(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	srv := newFailingServer(t, "first answer")

	if err := os.MkdirAll(filepath.Join(home, ".spettro"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfgJSON := `{"active_provider":` + strconvQuote(srv.URL) + `,"active_model":"fake-model","permission":"yolo"}`
	if err := os.WriteFile(filepath.Join(home, ".spettro", "config.json"), []byte(cfgJSON), 0o644); err != nil {
		t.Fatal(err)
	}

	pm := provider.NewManager()
	pm.AddLocalModels([]provider.Model{{Provider: srv.URL, Name: "fake-model", Local: true}})

	manifest := config.AgentManifest{Agents: []config.AgentSpec{{
		ID:           "coding",
		Mode:         "worker",
		AllowedTools: []string{"comment"},
		Permission:   config.PermissionYOLO,
		Enabled:      true,
	}}}

	globalDir := t.TempDir()
	b := newBridge(Options{
		CWD:       t.TempDir(),
		GlobalDir: globalDir,
		Providers: pm,
		Manifest:  manifest,
	})
	pr, pw := io.Pipe()
	t.Cleanup(func() { _ = pw.Close() })
	b.conn = acpsdk.NewAgentSideConnection(b, io.Discard, pr)

	s := &acpSession{
		id:                "sess-fail",
		cwd:               t.TempDir(),
		agentID:           "coding",
		manifest:          manifest,
		mediaDir:          t.TempDir(),
		startedAt:         time.Now(),
		commandsAnnounced: true,
	}
	b.sessions[s.id] = s

	prompt := func(text string) error {
		_, err := b.Prompt(context.Background(), acpsdk.PromptRequest{
			SessionId: acpsdk.SessionId(s.id),
			Prompt:    []acpsdk.ContentBlock{acpsdk.TextBlock(text)},
		})
		return err
	}

	// Turn 1 succeeds and establishes context.
	if err := prompt("remember the codeword PINEAPPLE"); err != nil {
		t.Fatalf("first turn: %v", err)
	}
	historyAfterTurn1 := len(s.history)
	if historyAfterTurn1 == 0 {
		t.Fatal("first turn must establish structured history")
	}

	// Turn 2 fails hard (server only returns 500s now).
	if err := prompt("now do a second thing"); err == nil {
		t.Fatal("second turn must fail against the broken server")
	}

	// The failed turn must not have wiped the structured history: it should
	// be at least as long as before (the new user task was appended).
	if len(s.history) < historyAfterTurn1 {
		t.Fatalf("failed turn shrank history: %d → %d", historyAfterTurn1, len(s.history))
	}
	// And the first turn's content must still be in there.
	found := false
	for _, m := range s.history {
		if strings.Contains(m.Content, "PINEAPPLE") {
			found = true
			break
		}
	}
	if !found {
		t.Error("first turn's content lost from structured history after failed turn")
	}

	// The flat transcript must have recorded both user turns (this is what
	// session/load and the TUI's /resume replay).
	var userTurns, assistantNotes int
	for _, m := range s.transcript {
		if m.Role == "user" {
			userTurns++
		}
		if m.Role == "assistant" && strings.Contains(m.Content, "turn failed") {
			assistantNotes++
		}
	}
	if userTurns != 2 {
		t.Errorf("transcript must record both user turns, got %d", userTurns)
	}
	if assistantNotes != 1 {
		t.Errorf("transcript must note the failed turn, got %d notes", assistantNotes)
	}

	// The failure must have been persisted to the on-disk store too, so a
	// later session/load (or editor reconnect) still sees the conversation.
	state, err := session.Load(globalDir, s.id)
	if err != nil {
		t.Fatalf("session must be persisted after a failed turn: %v", err)
	}
	if len(state.Messages) < 3 {
		t.Errorf("persisted transcript too short: %d messages", len(state.Messages))
	}
}
