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
	msg := runStructuredCompact(context.Background(), pm, url, "m", history, compact.Params{Window: 200000, Force: true, Focus: "the flag"})
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
	msg := runStructuredCompact(context.Background(), pm, url, "m", history, compact.Params{Window: 200000, Force: true})
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

// shortCarried is a carried history too small for a summary to free anything
// worth a model call.
func shortCarried(n int) []provider.Message {
	msgs := []provider.Message{{Role: provider.RoleUser, Content: "Task:\nsay hi"}}
	for i := range n {
		msgs = append(msgs,
			provider.Message{Role: provider.RoleAssistant, Content: fmt.Sprintf("reply %d", i)},
			provider.Message{Role: provider.RoleUser, Content: fmt.Sprintf("Task:\nmessage %d", i)},
		)
	}
	return msgs
}

// Auto-compaction with the pressure coming from outside the history (system
// prompt, tool schemas) makes no summarizer call, shows no banner, and does
// not fire again until the history changes.
func TestAutoCompactSkipsSummarizerWhenNothingToGain(t *testing.T) {
	pm, url, prompts := summarizerServer(t, "summary")
	m := NewModelForTesting()
	m.cfg.AutoCompactEnabled = true
	m.convHistory = shortCarried(4)
	m.messages = []ChatMessage{{Role: RoleUser, Content: "a"}, {Role: RoleAssistant, Content: "b"}, {Role: RoleUser, Content: "c"}}
	m.contextTokens = resolveGoalContextWindow(m)
	if m.autoCompactIfNeeded() == nil {
		t.Fatal("the trigger should fire at a full window")
	}

	msg := runStructuredCompact(context.Background(), pm, url, "m", m.convHistory, m.autoCompactParams(m.convHistory, ""))
	if msg.err != nil || !msg.noop {
		t.Fatalf("want a no-op, got err=%v noop=%v", msg.err, msg.noop)
	}
	if len(*prompts) != 0 {
		t.Fatalf("auto-compaction called the summarizer %d times for nothing", len(*prompts))
	}
	m.thinking, m.autoCompactInFlight = true, true
	next, _ := m.update(msg)
	m = next.(Model)
	if m.banner != "" {
		t.Fatalf("an automatic no-op should be silent, banner %q", m.banner)
	}
	if m.autoCompactIfNeeded() != nil {
		t.Fatal("auto-compaction re-fired on an unchanged history")
	}
	m.convHistory = append(m.convHistory, provider.Message{Role: provider.RoleAssistant, Content: "more"})
	if m.autoCompactIfNeeded() == nil {
		t.Fatal("auto-compaction should try again once the history grew")
	}
}

// Auto-compaction goes cheapest first: large spooled tool outputs are stubbed
// without a summarizer call when that is enough.
func TestAutoCompactPrunesBeforeSummarizing(t *testing.T) {
	pm, url, prompts := summarizerServer(t, "summary")
	history := []provider.Message{{Role: provider.RoleUser, Content: "Task:\nread everything"}}
	for i := range 17 {
		id := fmt.Sprintf("r%d", i)
		history = append(history,
			provider.Message{Role: provider.RoleAssistant, ToolCalls: []provider.NativeTool{{ID: id, Name: "file-read", Args: json.RawMessage(`{"path":"big.go"}`)}}},
			provider.Message{Role: provider.RoleUser, ToolResults: []provider.ToolResult{{ID: id, Name: "file-read", Output: strings.Repeat("x", 40000), SpoolID: fmt.Sprintf("spool:%d", i)}}},
		)
	}
	m := NewModelForTesting()
	m.cfg.AutoCompactEnabled = true
	m.convHistory = history
	m.contextTokens = compact.EstimateHistoryTokens("", history) + 2000
	if !m.evaluateCompact().ShouldAutoCompact {
		t.Fatalf("test history (%d tokens) should trigger auto-compaction", m.contextTokens)
	}
	msg := runStructuredCompact(context.Background(), pm, url, "m", history, m.autoCompactParams(history, ""))
	if msg.err != nil || msg.noop || msg.messages == nil {
		t.Fatalf("want a pruned history, got err=%v noop=%v", msg.err, msg.noop)
	}
	if len(*prompts) != 0 {
		t.Fatal("pruning was enough; the summarizer should not have been called")
	}
	if len(msg.messages) != len(history) || !strings.Contains(msg.summary, "stubs") {
		t.Fatalf("pruning should keep every turn: %d -> %d, summary %q", len(history), len(msg.messages), msg.summary)
	}
	if err := compact.ValidatePairing(msg.messages); err != nil {
		t.Fatal(err)
	}
}
