package agent_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"spettro/internal/provider"
)

// scriptedServer returns an httptest.Server that serves a fixed sequence of
// OpenAI-compatible chat completion responses, one per request.
func scriptedServer(t *testing.T, responses []string) *httptest.Server {
	t.Helper()
	var idx atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		i := int(idx.Add(1)) - 1
		if i >= len(responses) {
			t.Errorf("unexpected extra request #%d (only %d scripted)", i+1, len(responses))
			http.Error(w, "no more scripted responses", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		resp := map[string]any{
			"id":     "chatcmpl-test",
			"object": "chat.completion",
			"choices": []map[string]any{
				{
					"index": 0,
					"message": map[string]any{
						"role":    "assistant",
						"content": responses[i],
					},
					"finish_reason": "stop",
				},
			},
			"usage": map[string]any{
				"prompt_tokens":     10,
				"completion_tokens": 20,
				"total_tokens":      30,
			},
		}
		json.NewEncoder(w).Encode(resp) //nolint:errcheck
	}))
	t.Cleanup(srv.Close)
	return srv
}

// testProvider returns a Manager and provider name pointing at srv.
// The Manager treats http:// URLs as OpenAI-compatible local servers.
func testProvider(srv *httptest.Server) (*provider.Manager, string) {
	return provider.NewManager(), srv.URL
}

// scriptedManager creates a provider.Manager wired to a local HTTP server
// that serves a scripted sequence of LLM responses in order.
// Returns (manager, providerName, modelName).
func scriptedManager(t *testing.T, responses []string) (*provider.Manager, string, string) {
	t.Helper()
	var idx atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		i := int(idx.Add(1)) - 1
		if i >= len(responses) {
			t.Errorf("unexpected extra request #%d (only %d scripted)", i+1, len(responses))
			http.Error(w, "no more scripted responses", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		resp := map[string]any{
			"id":     "chatcmpl-test",
			"object": "chat.completion",
			"choices": []map[string]any{
				{"index": 0, "message": map[string]any{"role": "assistant", "content": responses[i]}, "finish_reason": "stop"},
			},
			"usage": map[string]any{"total_tokens": 30},
		}
		json.NewEncoder(w).Encode(resp) //nolint:errcheck
	}))
	t.Cleanup(srv.Close)

	pm := provider.NewManager()
	pm.AddLocalModels([]provider.Model{{Provider: srv.URL, Name: "test-model", Local: true}})
	return pm, srv.URL, "test-model"
}
