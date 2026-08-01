package provider_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"spettro/internal/provider"
)

// Every successful Manager.Send must accumulate the provider-reported usage —
// totals, per-model breakdown, request count, and the last-request snapshot
// that feeds the status-bar cache indicator.
func TestManagerSend_AccumulatesUsage(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":     "chatcmpl-test",
			"object": "chat.completion",
			"choices": []map[string]any{
				{
					"index":         0,
					"message":       map[string]any{"role": "assistant", "content": "hi"},
					"finish_reason": "stop",
				},
			},
			"usage": map[string]any{
				"prompt_tokens":         100,
				"completion_tokens":     20,
				"total_tokens":          120,
				"prompt_tokens_details": map[string]any{"cached_tokens": 80},
			},
		})
	}))
	t.Cleanup(server.Close)

	pm := provider.NewManager()
	pm.AddLocalModels([]provider.Model{{Provider: server.URL, Name: "test-model", Local: true}})

	for range 2 {
		if _, err := pm.Send(context.Background(), server.URL, "test-model", provider.Request{Prompt: "hello"}); err != nil {
			t.Fatalf("Send: %v", err)
		}
	}

	s := pm.UsageSnapshot()
	if s.Totals.Requests != 2 {
		t.Fatalf("expected 2 requests, got %d", s.Totals.Requests)
	}
	// cached_tokens is a subset of prompt_tokens: input should be 100-80 = 20 each.
	if s.Totals.InputTokens != 40 || s.Totals.CacheReadTokens != 160 || s.Totals.OutputTokens != 40 {
		t.Fatalf("unexpected totals: %+v", s.Totals)
	}
	if s.Last.CacheReadTokens != 80 || s.Last.InputTokens != 20 {
		t.Fatalf("unexpected last usage: %+v", s.Last)
	}
	key := server.URL + ":test-model"
	if bm, ok := s.ByModel[key]; !ok || bm.Requests != 2 {
		t.Fatalf("expected per-model entry for %q with 2 requests, got %+v", key, s.ByModel)
	}
	if got := s.Last.CacheHitRate(); got < 0.79 || got > 0.81 {
		t.Fatalf("expected ~0.8 hit rate, got %f", got)
	}

	// Restore/reset round-trip: /resume must bring counters back, /clear zero them.
	saved := pm.UsageSnapshot()
	pm.ResetUsage()
	if got := pm.UsageSnapshot(); got.Totals.Requests != 0 || len(got.ByModel) != 0 {
		t.Fatalf("expected zeroed usage after reset, got %+v", got)
	}
	pm.RestoreUsage(saved)
	if got := pm.UsageSnapshot(); got.Totals != saved.Totals || got.ByModel[key] != saved.ByModel[key] {
		t.Fatalf("expected restored usage, got %+v want %+v", got, saved)
	}
}

func TestUsageCacheHitRate_NoInputIsUnavailable(t *testing.T) {
	t.Parallel()
	if got := (provider.Usage{}).CacheHitRate(); got != -1 {
		t.Fatalf("expected -1 for empty usage, got %f", got)
	}
}
