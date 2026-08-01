package agent_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"spettro/internal/agent"
	"spettro/internal/config"
)

// capturedRequest is one decoded chat-completions request body.
type capturedRequest struct {
	Messages []capturedMessage `json:"messages"`
}

type capturedMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// capturingServer serves scripted responses AND records every request body,
// so tests can assert the exact prompt shape the provider would cache.
func capturingServer(t *testing.T, responses []string) (*httptest.Server, func() []capturedRequest) {
	t.Helper()
	var mu sync.Mutex
	var reqs []capturedRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body capturedRequest
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		i := len(reqs)
		reqs = append(reqs, body)
		mu.Unlock()
		if i >= len(responses) {
			t.Errorf("unexpected extra request #%d (only %d scripted)", i+1, len(responses))
			http.Error(w, "no more scripted responses", http.StatusInternalServerError)
			return
		}
		msg, finish := scriptedMessage(t, responses[i])
		w.Header().Set("Content-Type", "application/json")
		resp := map[string]any{
			"id":     "chatcmpl-test",
			"object": "chat.completion",
			"choices": []map[string]any{
				{"index": 0, "message": msg, "finish_reason": finish},
			},
			"usage": map[string]any{"total_tokens": 30},
		}
		json.NewEncoder(w).Encode(resp) //nolint:errcheck
	}))
	t.Cleanup(srv.Close)
	return srv, func() []capturedRequest {
		mu.Lock()
		defer mu.Unlock()
		out := make([]capturedRequest, len(reqs))
		copy(out, reqs)
		return out
	}
}

// TestSystemPromptStableAcrossSteps pins the core prompt-cache invariant inside
// a run: the system prompt must be byte-identical on every step. Any per-step
// variation (the old "Current step: N" counter) invalidates the provider cache
// on every call.
func TestSystemPromptStableAcrossSteps(t *testing.T) {
	p1 := agent.BuildLoopPromptForTesting("You are an assistant.", "do the thing", "", "", 1)
	p7 := agent.BuildLoopPromptForTesting("You are an assistant.", "do the thing", "", "", 7)
	if p1 != p7 {
		t.Fatalf("prompt must not vary with the step counter:\n--- step 1 ---\n%s\n--- step 7 ---\n%s", p1, p7)
	}
}

// TestToolLoopRequestsExtendStablePrefix asserts the cache property end to end
// for a multi-step tool run: every request must be a strict extension of the
// previous one — same system message, previous messages byte-identical, new
// turns only appended. If this holds, an Anthropic-style prefix cache hits on
// every step after the first.
func TestToolLoopRequestsExtendStablePrefix(t *testing.T) {
	dir := t.TempDir()
	srv, getReqs := capturingServer(t, []string{
		`TOOL_CALL {"tool":"repo-search","args":{"query":"a"}}`,
		`TOOL_CALL {"tool":"repo-search","args":{"query":"b"}}`,
		"FINAL\nDone.",
	})

	pm, providerName := testProvider(srv)
	c := agent.LLMCoder{
		ProviderManager: pm,
		ProviderName:    func() string { return providerName },
		ModelName:       func() string { return "fake-model" },
		CWD:             dir,
	}
	if _, err := c.Execute(context.Background(), "Search.", config.PermissionYOLO, true); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	reqs := getReqs()
	if len(reqs) != 3 {
		t.Fatalf("expected 3 requests, got %d", len(reqs))
	}
	for i := 1; i < len(reqs); i++ {
		prev, cur := reqs[i-1].Messages, reqs[i].Messages
		if len(cur) <= len(prev) {
			t.Fatalf("request %d must extend request %d: %d -> %d messages", i+1, i, len(prev), len(cur))
		}
		for j := range prev {
			if prev[j].Role != cur[j].Role || string(prev[j].Content) != string(cur[j].Content) {
				t.Fatalf("request %d mutated message %d (role %s): prefix must stay byte-identical for prompt caching\nbefore: %s\nafter:  %s",
					i+1, j, prev[j].Role, prev[j].Content, cur[j].Content)
			}
		}
	}
}

// TestRunCarriesStructuredHistoryAcrossTurns verifies the cross-turn contract:
// RunResult.Messages from turn 1, passed back as LLMAgent.Messages, is resent
// verbatim as the prefix of turn 2's request — no flattening into a
// "Conversation so far" blob, no discarded assistant output.
func TestRunCarriesStructuredHistoryAcrossTurns(t *testing.T) {
	dir := t.TempDir()
	spec := config.AgentSpec{ID: "ask", Mode: "ask"}

	srv1, getReqs1 := capturingServer(t, []string{"answer one"})
	pm1, provider1 := testProvider(srv1)
	turn1 := agent.LLMAgent{
		Spec:            spec,
		ProviderManager: pm1,
		ProviderName:    func() string { return provider1 },
		ModelName:       func() string { return "fake-model" },
		CWD:             dir,
	}
	res1, err := turn1.Run(context.Background(), "first question")
	if err != nil {
		t.Fatalf("turn 1: %v", err)
	}
	if len(res1.Messages) != 2 {
		t.Fatalf("turn 1 should return [user, assistant], got %d messages", len(res1.Messages))
	}
	if res1.Messages[1].Role != "assistant" || res1.Messages[1].Content != "answer one" {
		t.Fatalf("turn 1 history must end with the assistant answer, got %+v", res1.Messages[1])
	}

	srv2, getReqs2 := capturingServer(t, []string{"answer two"})
	pm2, provider2 := testProvider(srv2)
	turn2 := agent.LLMAgent{
		Spec:            spec,
		ProviderManager: pm2,
		ProviderName:    func() string { return provider2 },
		ModelName:       func() string { return "fake-model" },
		CWD:             dir,
		Messages:        res1.Messages,
	}
	res2, err := turn2.Run(context.Background(), "second question")
	if err != nil {
		t.Fatalf("turn 2: %v", err)
	}
	if len(res2.Messages) != 4 {
		t.Fatalf("turn 2 should return 4 messages (2 carried + user + assistant), got %d", len(res2.Messages))
	}

	req1 := getReqs1()[0].Messages
	req2 := getReqs2()[0].Messages
	// Turn 2's request must re-send turn 1's messages verbatim after the system
	// message — that byte-stability is what makes the provider cache hit.
	if len(req2) != len(req1)+2 {
		t.Fatalf("turn 2 request should be turn 1 + [assistant, user]: %d -> %d messages", len(req1), len(req2))
	}
	for j := range req1 {
		if req1[j].Role != req2[j].Role || string(req1[j].Content) != string(req2[j].Content) {
			t.Fatalf("turn 2 mutated carried message %d (role %s):\nturn1: %s\nturn2: %s",
				j, req1[j].Role, req1[j].Content, req2[j].Content)
		}
	}
	var lastUser string
	if err := json.Unmarshal(req2[len(req2)-1].Content, &lastUser); err != nil {
		t.Fatalf("last message content: %v", err)
	}
	if !strings.Contains(lastUser, "second question") {
		t.Fatalf("current task missing from final user turn: %q", lastUser)
	}
	for _, m := range req2 {
		if strings.Contains(string(m.Content), "Conversation so far") {
			t.Fatalf("structured turns must not fall back to the flattened transcript: %s", m.Content)
		}
	}
}
