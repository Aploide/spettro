package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"spettro/internal/models"
)

// Anthropic buffers a tool input until each parameter is fully generated,
// so a large file write is a long gap with no content event — only "ping"
// keep-alives. Those count as activity: the stream must not be cut off.
func TestStreamIdleWatchdogCountsKeepAlives(t *testing.T) {
	events := anthropicThinkingToolStream("tool_use", `{"path":"a.go","content":"x"}`, 30)
	// events[5] is the tool_use content_block_start, events[6] its input.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		flush := func() { w.(http.Flusher).Flush() }
		for i, ev := range events {
			if i == 6 {
				deadline := time.Now().Add(600 * time.Millisecond)
				for time.Now().Before(deadline) {
					fmt.Fprint(w, anthropicEvent("ping", map[string]any{}))
					flush()
					time.Sleep(40 * time.Millisecond)
				}
			}
			fmt.Fprint(w, ev)
			flush()
		}
	}))
	t.Cleanup(srv.Close)
	pm := NewManager()
	pm.SetCatalog(models.Catalog{Providers: map[string]models.CatalogProvider{
		"anth-test": {Name: "Anth", API: models.APIAnthropic, BaseURL: srv.URL, Models: map[string]models.CatalogModel{
			"claude-sonnet-4-5": {Name: "Sonnet", ToolCall: true},
		}},
	}})
	pm.SetAPIKeys(map[string]string{"anth-test": "k"})

	req := Request{Messages: []Message{{Role: RoleUser, Content: "go"}}, StreamIdleTimeout: 200 * time.Millisecond}
	resp, err := pm.Send(context.Background(), "anth-test", "claude-sonnet-4-5", req)
	if err != nil {
		t.Fatalf("a stream kept alive by pings failed: %v", err)
	}
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].ArgsError != "" {
		t.Fatalf("tool calls = %+v", resp.ToolCalls)
	}
}

func TestStreamTimeouts(t *testing.T) {
	t.Setenv(streamIdleEnv, "")
	cases := []struct {
		name        string
		provider    string
		req         Request
		first, idle time.Duration
	}{
		{"defaults", "groq", Request{}, streamFirstChunkTimeout, DefaultStreamIdleTimeout},
		{"thinking may be silent", "anthropic", Request{Thinking: ThinkingMedium}, streamFirstChunkTimeout, streamReasoningIdleTimeout},
		{"thinking off", "anthropic", Request{Thinking: ThinkingOff}, streamFirstChunkTimeout, DefaultStreamIdleTimeout},
		{"openai responses reasons silently by default", "openai", Request{}, streamFirstChunkTimeout, streamReasoningIdleTimeout},
		{"local servers may process a prompt for long", "http://localhost:8080", Request{localEndpoint: true}, streamLocalFirstChunkTimeout, DefaultStreamIdleTimeout},
		{"explicit value wins", "openai", Request{StreamIdleTimeout: time.Second, localEndpoint: true}, time.Second, time.Second},
	}
	for _, tc := range cases {
		first, idle := streamTimeouts(tc.provider, tc.req)
		if first != tc.first || idle != tc.idle {
			t.Errorf("%s: got (%s, %s), want (%s, %s)", tc.name, first, idle, tc.first, tc.idle)
		}
	}

	t.Setenv(streamIdleEnv, "900")
	if first, idle := streamTimeouts("groq", Request{}); first != 900*time.Second || idle != 900*time.Second {
		t.Errorf("env seconds: got (%s, %s)", first, idle)
	}
	t.Setenv(streamIdleEnv, "20m")
	if first, idle := streamTimeouts("groq", Request{}); first != 20*time.Minute || idle != 20*time.Minute {
		t.Errorf("env duration: got (%s, %s)", first, idle)
	}
	t.Setenv(streamIdleEnv, "0")
	if first, idle := streamTimeouts("groq", Request{}); first != 0 || idle != 0 {
		t.Errorf("env 0 must disable the watchdog: got (%s, %s)", first, idle)
	}
	t.Setenv(streamIdleEnv, "soon")
	if first, _ := streamTimeouts("groq", Request{}); first != streamFirstChunkTimeout {
		t.Errorf("an unparsable override must be ignored: got %s", first)
	}
}

// A 64k default output cap next to a ~150k-token prompt on a 200k model
// would be rejected (prompt + max_tokens > window): the cap shrinks to fit.
func TestAnthropicOutputCapFitsTheContextWindow(t *testing.T) {
	pm, bodies := newAnthropicServer(t, anthropicThinkingToolStream("end_turn", `{}`, 5))
	big := strings.Repeat("abcd", 150000) // ~150k tokens
	req := Request{Messages: []Message{{Role: RoleUser, Content: big}}, ContextWindow: 200000}
	if _, err := pm.Send(context.Background(), "anth-test", "claude-sonnet-4-5", req); err != nil {
		t.Fatal(err)
	}
	got, _ := (*bodies)[0]["max_tokens"].(float64)
	if got >= 64000 || got < minOutputTokens {
		t.Fatalf("max_tokens = %v, want a cap that leaves the prompt its room", got)
	}
	if int(got)+EstimateRequestTokens(req) > 200000 {
		t.Fatalf("max_tokens %v + prompt exceeds the window", got)
	}
}

// OpenAI-style gateways reject prompt + max_tokens > window. A model whose
// known output limit equals its window (gpt-5 family on a 128k gateway
// entry) must not be sent that limit: the server default is used instead.
func TestCompatAutoOutputCapIsDroppedWhenItCannotFit(t *testing.T) {
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		bodies = append(bodies, body)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, sseChunk(map[string]any{"role": "assistant", "content": "ok"}, "stop", nil))
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)
	pm := NewManager()
	pm.AddLocalModels([]Model{{Provider: srv.URL, Name: "gpt-5.3-codex-spark", Local: true, Context: 128000}})
	if _, err := pm.Send(context.Background(), srv.URL, "gpt-5.3-codex-spark", streamReq()); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"max_tokens", "max_completion_tokens"} {
		if v, ok := bodies[0][k]; ok {
			t.Fatalf("%s = %v sent; the known limit cannot fit next to the prompt", k, v)
		}
	}
}
