package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
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
