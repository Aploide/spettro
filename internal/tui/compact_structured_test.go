package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"spettro/internal/compact"
	"spettro/internal/provider"
)

// summarizerServer is an OpenAI-compatible endpoint that answers every
// request with summary and records the prompts it was sent.
func summarizerServer(t *testing.T, summary string) (*provider.Manager, string, *[]string) {
	t.Helper()
	var prompts []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []struct {
				Content any `json:"content"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		prompts = append(prompts, fmt.Sprint(body.Messages))
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "x", "object": "chat.completion",
			"choices": []any{map[string]any{"index": 0, "finish_reason": "stop", "message": map[string]any{"role": "assistant", "content": summary}}},
		})
	}))
	t.Cleanup(srv.Close)
	pm := provider.NewManager()
	pm.AddLocalModels([]provider.Model{{Provider: srv.URL, Name: "m", Local: true}})
	return pm, srv.URL, &prompts
}

// carriedHistory is a two-turn structured conversation with tool use: the
// shape the TUI carries between runs.
func carriedHistory() []provider.Message {
	msgs := []provider.Message{{Role: provider.RoleUser, Content: "Task:\nfirst task: add a --json flag", SessionContext: "env"}}
	for turn := range 2 {
		if turn > 0 {
			msgs = append(msgs, provider.Message{Role: provider.RoleUser, Content: "Task:\nsecond task: document the flag"})
		}
		for i := range 4 {
			id := fmt.Sprintf("c%d_%d", turn, i)
			msgs = append(msgs,
				provider.Message{Role: provider.RoleAssistant, ToolCalls: []provider.NativeTool{{ID: id, Name: "file-read", Args: json.RawMessage(`{"path":"main.go"}`)}}},
				provider.Message{Role: provider.RoleUser, ToolResults: []provider.ToolResult{{ID: id, Name: "file-read", Output: strings.Repeat("package main\n", 200)}}},
			)
		}
		msgs = append(msgs, provider.Message{Role: provider.RoleAssistant, Content: fmt.Sprintf("turn %d done", turn)})
	}
	return msgs
}

// /compact on a session with structured history compacts that history (not
// the flat transcript): the first task and the latest request survive
// verbatim, the pairing stays valid, and the model routing carries it on.
func TestStructuredCompactKeepsTaskAndPairing(t *testing.T) {
	pm, url, prompts := summarizerServer(t, "## Goal\nadd and document --json")
	history := carriedHistory()
	msg := runStructuredCompact(context.Background(), pm, url, "m", history, 200000, "the flag")
	if msg.err != nil || msg.noop {
		t.Fatalf("compaction failed: err=%v noop=%v", msg.err, msg.noop)
	}
	if len(*prompts) != 1 || !strings.Contains((*prompts)[0], "the flag") || !strings.Contains((*prompts)[0], "## Files modified") {
		t.Fatalf("summarizer not called with the instructions and focus: %q", *prompts)
	}
	out := msg.messages
	if err := compact.ValidatePairing(out); err != nil {
		t.Fatalf("invalid pairing: %v", err)
	}
	if out[0].Content != history[0].Content || out[0].SessionContext != "env" {
		t.Fatal("the first task was not kept verbatim")
	}
	var latest bool
	for _, m := range out {
		if m.Content == "Task:\nsecond task: document the flag" {
			latest = true
		}
	}
	if !latest {
		t.Fatal("the latest user request was summarized away")
	}
	if !strings.Contains(msg.summary, "add and document --json") {
		t.Fatalf("summary = %q", msg.summary)
	}

	m := NewModelForTesting()
	m.thinking = true
	m.convHistory = history
	m.messages = []ChatMessage{{Role: RoleUser, Content: "x"}}
	next, _ := m.update(msg)
	m = next.(Model)
	if len(m.convHistory) != len(out) || m.convHistory[0].Content != history[0].Content {
		t.Fatal("the compacted structured history was not carried on")
	}
}

func TestStructuredCompactShortHistoryIsNoop(t *testing.T) {
	pm, url, _ := summarizerServer(t, "a long summary that would not save anything at all")
	history := []provider.Message{
		{Role: provider.RoleUser, Content: "hi"},
		{Role: provider.RoleAssistant, Content: "hello"},
		{Role: provider.RoleUser, Content: "and?"},
		{Role: provider.RoleAssistant, Content: "done"},
	}
	msg := runStructuredCompact(context.Background(), pm, url, "m", history, 200000, "")
	if msg.err != nil || !msg.noop {
		t.Fatalf("want a no-op, got err=%v noop=%v", msg.err, msg.noop)
	}
	m := NewModelForTesting()
	m.thinking = true
	m.convHistory = history
	next, _ := m.update(msg)
	if got := next.(Model).convHistory; len(got) != len(history) {
		t.Fatal("a no-op compaction must keep the history")
	}
}
