package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"spettro/internal/models"
)

// newCapturingCompatServer is an OpenAI-compatible chat completions server
// that records each request body.
func newCapturingCompatServer(t *testing.T) (*httptest.Server, func() map[string]any) {
	t.Helper()
	var mu sync.Mutex
	var last map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		last = body
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "c", "object": "chat.completion",
			"choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": "ok"}, "finish_reason": "stop"}},
			"usage":   map[string]any{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
		})
	}))
	t.Cleanup(srv.Close)
	return srv, func() map[string]any {
		mu.Lock()
		defer mu.Unlock()
		return last
	}
}

// The user's thinking_level reaches the wire as reasoning_effort on the
// Spettro Subscription and on OpenAI-compatible providers, going through
// ConfiguredThinking exactly as the TUI, headless and ACP hosts do. The
// subscription's model list need not flag reasoning for it to apply.
func TestConfiguredThinkingSendsReasoningEffort(t *testing.T) {
	srv, lastBody := newCapturingCompatServer(t)
	pm := NewManager()
	pm.SetAPIKeys(map[string]string{spettroProviderID: "k", "compat": "k"})
	pm.SetCatalog(models.Catalog{Providers: map[string]models.CatalogProvider{
		"compat": {API: models.APIOpenAI, BaseURL: srv.URL, Models: map[string]models.CatalogModel{
			"thinker": {Reasoning: true},
			"plain":   {},
		}},
	}})
	pm.SetSpettro(srv.URL, []Model{{Provider: spettroProviderID, Name: "flash", ToolCall: true}})

	cases := []struct {
		provider, model, level string
		want                   any // nil: no reasoning_effort field
	}{
		{spettroProviderID, "flash", "low", "low"},
		{spettroProviderID, "flash", "medium", "medium"},
		{spettroProviderID, "flash", "high", "high"},
		{spettroProviderID, "flash", "", nil},
		{"compat", "thinker", "low", "low"},
		{"compat", "thinker", "high", "high"},
		// A model the catalog marks as non-reasoning gets no parameter.
		{"compat", "plain", "high", nil},
	}
	for _, tc := range cases {
		level := pm.ConfiguredThinking(tc.provider, tc.model, tc.level)
		if _, err := pm.Send(context.Background(), tc.provider, tc.model, Request{Prompt: "hi", Thinking: level}); err != nil {
			t.Fatalf("%s/%s %q: %v", tc.provider, tc.model, tc.level, err)
		}
		body := lastBody()
		got, has := body["reasoning_effort"]
		switch {
		case tc.want == nil && has:
			t.Errorf("%s/%s %q: reasoning_effort = %v, want none", tc.provider, tc.model, tc.level, got)
		case tc.want != nil && got != tc.want:
			t.Errorf("%s/%s %q: reasoning_effort = %v, want %v", tc.provider, tc.model, tc.level, got, tc.want)
		}
	}
}

// newEffortRejectingServer answers any request carrying reasoning_effort
// with a 400 whose message is msg, and counts the requests it gets.
func newEffortRejectingServer(t *testing.T, msg string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "application/json")
		if _, has := body["reasoning_effort"]; has {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": msg, "type": "invalid_request_error"}})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "c", "object": "chat.completion",
			"choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": "ok"}, "finish_reason": "stop"}},
			"usage":   map[string]any{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
		})
	}))
	t.Cleanup(srv.Close)
	return srv, &n
}

// A Spettro model the plan lists as reasoning:false gets no thinking
// parameter, like a catalog model marked non-reasoning.
func TestConfiguredThinkingSkipsSpettroNonReasoning(t *testing.T) {
	pm := NewManager()
	pm.SetSpettro("https://inference.example/v1", []Model{
		{Provider: spettroProviderID, Name: "chat", NoReasoning: true},
		{Provider: spettroProviderID, Name: "flash"},
	})
	if got := pm.ConfiguredThinking(spettroProviderID, "chat", "high"); got != "" {
		t.Errorf("reasoning:false model: got %q, want none", got)
	}
	if got := pm.ConfiguredThinking(spettroProviderID, "flash", "high"); got != ThinkingHigh {
		t.Errorf("unflagged model: got %q, want high", got)
	}
}

// Once a model has rejected reasoning_effort, later sends start from the
// level the ladder settled on instead of walking it again on every call.
func TestSendRemembersRejectedReasoningEffort(t *testing.T) {
	srv, n := newEffortRejectingServer(t, "Unsupported parameter: 'reasoning_effort' is not supported with this model.")
	pm := NewManager()
	pm.SetAPIKeys(map[string]string{spettroProviderID: "k"})
	pm.SetSpettro(srv.URL, []Model{{Provider: spettroProviderID, Name: "flash", ToolCall: true}})

	resp, err := pm.Send(context.Background(), spettroProviderID, "flash", Request{Prompt: "hi", Thinking: ThinkingHigh})
	if err != nil {
		t.Fatalf("first send: %v", err)
	}
	if resp.Thinking != "" || n.Load() < 2 {
		t.Fatalf("first send: thinking %q after %d requests; want the ladder walked down to none", resp.Thinking, n.Load())
	}
	for i := range 2 {
		n.Store(0)
		resp, err := pm.Send(context.Background(), spettroProviderID, "flash", Request{Prompt: "hi", Thinking: ThinkingHigh})
		if err != nil {
			t.Fatalf("send %d: %v", i+2, err)
		}
		if got := n.Load(); got != 1 || resp.Thinking != "" {
			t.Errorf("send %d: %d requests, thinking %q; want 1 request without reasoning_effort", i+2, got, resp.Thinking)
		}
	}
}

// The proxy may rewrap an upstream's rejection as a bare 400 that no longer
// names reasoning_effort. For a Spettro model the plan does not flag as
// reasoning (it got the parameter on trust), Send retries once without it and
// remembers that; a model the plan flags as reasoning gets the error.
func TestSendDropsThinkingOnBare400ForUnflaggedSpettroModel(t *testing.T) {
	srv, n := newEffortRejectingServer(t, "upstream provider returned 400 Bad Request")
	pm := NewManager()
	pm.SetAPIKeys(map[string]string{spettroProviderID: "k"})
	pm.SetSpettro(srv.URL, []Model{
		{Provider: spettroProviderID, Name: "flash", ToolCall: true},
		{Provider: spettroProviderID, Name: "thinker", ToolCall: true, Reasoning: true},
	})

	if _, err := pm.Send(context.Background(), spettroProviderID, "flash", Request{Prompt: "hi", Thinking: ThinkingHigh}); err != nil {
		t.Fatalf("unflagged model: %v", err)
	}
	n.Store(0)
	if _, err := pm.Send(context.Background(), spettroProviderID, "flash", Request{Prompt: "hi", Thinking: ThinkingHigh}); err != nil {
		t.Fatalf("unflagged model, second send: %v", err)
	}
	if got := n.Load(); got != 1 {
		t.Errorf("unflagged model, second send: %d requests, want 1", got)
	}
	if _, err := pm.Send(context.Background(), spettroProviderID, "thinker", Request{Prompt: "hi", Thinking: ThinkingHigh}); err == nil {
		t.Error("reasoning model: a bare 400 must surface, not silently drop thinking")
	}
}
