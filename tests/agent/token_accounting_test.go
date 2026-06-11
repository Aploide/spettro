package agent_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"spettro/internal/agent"
	"spettro/internal/config"
)

// scriptedServerWithTokens serves OpenAI-compatible responses where each step
// reports a distinct total_tokens, so a test can distinguish the cumulative sum
// (cost) from the per-step max (context occupancy).
func scriptedServerWithTokens(t *testing.T, steps []struct {
	content string
	tokens  int
}) *httptest.Server {
	t.Helper()
	var idx int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		i := int(atomic.AddInt32(&idx, 1)) - 1
		if i >= len(steps) {
			t.Errorf("unexpected extra request #%d (only %d scripted)", i+1, len(steps))
			http.Error(w, "no more scripted responses", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		resp := map[string]any{
			"id":     "chatcmpl-test",
			"object": "chat.completion",
			"choices": []map[string]any{
				{"index": 0, "message": map[string]any{"role": "assistant", "content": steps[i].content}, "finish_reason": "stop"},
			},
			"usage": map[string]any{"total_tokens": steps[i].tokens},
		}
		json.NewEncoder(w).Encode(resp) //nolint:errcheck
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestRunTokenAccounting_SumVsOccupancy verifies EFF-3: TokensUsed accumulates
// every step's reported tokens (cost), while ContextTokens reports the LARGEST
// single step (occupancy), not the sum. A multi-step run whose prompts grow
// must not report inflated occupancy.
func TestRunTokenAccounting_SumVsOccupancy(t *testing.T) {
	dir := t.TempDir()

	// Each step's prompt re-embeds growing history, so the per-step totals
	// climb: 100, 250, 400. Sum = 750 (cost); max = 400 (occupancy).
	srv := scriptedServerWithTokens(t, []struct {
		content string
		tokens  int
	}{
		{`TOOL_CALL {"tool":"repo-search","args":{"query":"a"}}`, 100},
		{`TOOL_CALL {"tool":"repo-search","args":{"query":"b"}}`, 250},
		{"FINAL\nDone.", 400},
	})

	pm, providerName := testProvider(srv)
	c := agent.LLMCoder{
		ProviderManager: pm,
		ProviderName:    func() string { return providerName },
		ModelName:       func() string { return "fake-model" },
		CWD:             dir,
	}
	result, err := c.Execute(context.Background(), "Search.", config.PermissionYOLO, true)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.TokensUsed != 750 {
		t.Fatalf("TokensUsed (cost) = %d, want 750 (sum of all steps)", result.TokensUsed)
	}
	if result.ContextTokens != 400 {
		t.Fatalf("ContextTokens (occupancy) = %d, want 400 (max single step)", result.ContextTokens)
	}
	if result.ContextTokens >= result.TokensUsed {
		t.Fatalf("occupancy (%d) must be < cumulative cost (%d) for a multi-step run", result.ContextTokens, result.TokensUsed)
	}
}
