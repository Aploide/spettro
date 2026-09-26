package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"spettro/internal/models"
)

// --- OpenAI-compatible SSE fixtures ---------------------------------------

// sseChunk renders one chat.completion.chunk event.
func sseChunk(delta map[string]any, finish string, usage map[string]any) string {
	choice := map[string]any{"index": 0, "delta": delta}
	if finish != "" {
		choice["finish_reason"] = finish
	}
	ev := map[string]any{"id": "chatcmpl-test", "object": "chat.completion.chunk", "model": "m", "choices": []any{choice}}
	if usage != nil {
		ev["usage"] = usage
	}
	raw, _ := json.Marshal(ev)
	return "data: " + string(raw) + "\n\n"
}

// newCompatStreamServer serves every request with the given SSE events and
// records the request bodies.
func newCompatStreamServer(t *testing.T, events ...string) (*Manager, string, *[]map[string]any) {
	t.Helper()
	var mu sync.Mutex
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		bodies = append(bodies, body)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		for _, ev := range events {
			fmt.Fprint(w, ev)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)
	pm := NewManager()
	pm.AddLocalModels([]Model{{Provider: srv.URL, Name: "m", Local: true}})
	return pm, srv.URL, &bodies
}

func toolCallDelta(id, name, args string) map[string]any {
	return map[string]any{
		"role": "assistant",
		"tool_calls": []any{map[string]any{
			"index": 0, "id": id, "type": "function",
			"function": map[string]any{"name": name, "arguments": args},
		}},
	}
}

func streamReq() Request {
	return Request{
		Messages: []Message{{Role: RoleUser, Content: "go"}},
		OnStream: func(StreamEvent) {},
	}
}

// A tool call cut at the output limit on the OpenAI-compatible stream is
// never emitted by the adapter (its JSON never becomes valid); it must still
// surface — not vanish — carrying a truncation error instead of "{}" args.
func TestStreamTruncatedToolCallReportsTruncation(t *testing.T) {
	pm, url, _ := newCompatStreamServer(t,
		sseChunk(toolCallDelta("call_1", "file-write", `{"path":"a.go","content":"package main\nfunc`), "", nil),
		sseChunk(map[string]any{}, "length", map[string]any{"prompt_tokens": 10, "completion_tokens": 50, "total_tokens": 60}),
	)
	resp, err := pm.Send(context.Background(), url, "m", streamReq())
	if err != nil {
		t.Fatal(err)
	}
	if resp.FinishReason != FinishLength || !resp.Truncated() {
		t.Fatalf("finish = %q, want length", resp.FinishReason)
	}
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("want the truncated call surfaced, got %+v", resp.ToolCalls)
	}
	tc := resp.ToolCalls[0]
	if tc.Name != "file-write" || string(tc.Args) != "{}" {
		t.Fatalf("unexpected call %+v", tc)
	}
	if !strings.Contains(tc.ArgsError, "truncated at the output token limit") {
		t.Fatalf("ArgsError = %q, want truncation message", tc.ArgsError)
	}
}

// A complete but slightly malformed call (raw newline inside a string,
// trailing comma) is repaired instead of being dropped or emptied.
func TestStreamMalformedToolCallIsRepaired(t *testing.T) {
	pm, url, _ := newCompatStreamServer(t,
		sseChunk(toolCallDelta("call_1", "file-write", "{\"path\":\"a.go\",\"content\":\"line1\nline2\",}"), "", nil),
		sseChunk(map[string]any{}, "tool_calls", map[string]any{"prompt_tokens": 10, "completion_tokens": 20, "total_tokens": 30}),
	)
	resp, err := pm.Send(context.Background(), url, "m", streamReq())
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].ArgsError != "" {
		t.Fatalf("want one repaired call, got %+v", resp.ToolCalls)
	}
	var args struct{ Path, Content string }
	if err := json.Unmarshal(resp.ToolCalls[0].Args, &args); err != nil {
		t.Fatal(err)
	}
	if args.Path != "a.go" || args.Content != "line1\nline2" {
		t.Fatalf("repaired args = %+v", args)
	}
}

// An unrepairable call is reported precisely and never executed with {}.
func TestStreamUnparseableToolCallReportsParseError(t *testing.T) {
	pm, url, _ := newCompatStreamServer(t,
		sseChunk(toolCallDelta("call_1", "grep", `{"pattern": foo bar}`), "", nil),
		sseChunk(map[string]any{}, "tool_calls", map[string]any{"prompt_tokens": 10, "completion_tokens": 20, "total_tokens": 30}),
	)
	resp, err := pm.Send(context.Background(), url, "m", streamReq())
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("got %+v", resp.ToolCalls)
	}
	msg := resp.ToolCalls[0].ArgsError
	if !strings.Contains(msg, "not valid JSON") || !strings.Contains(msg, "at byte") || !strings.Contains(msg, "foo bar") {
		t.Fatalf("ArgsError = %q, want a precise parse error", msg)
	}
}

// reasoning_content is captured with the producing provider/model, and
// replayed on the next request of the same model.
func TestStreamCompatReasoningRoundTrip(t *testing.T) {
	pm, url, bodies := newCompatStreamServer(t,
		sseChunk(map[string]any{"role": "assistant", "reasoning_content": "think hard"}, "", nil),
		sseChunk(map[string]any{"content": "answer"}, "", nil),
		sseChunk(map[string]any{}, "stop", map[string]any{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15}),
	)
	resp, err := pm.Send(context.Background(), url, "m", streamReq())
	if err != nil {
		t.Fatal(err)
	}
	if resp.Content != "answer" || resp.FinishReason != FinishStop {
		t.Fatalf("resp = %+v", resp)
	}
	if len(resp.Reasoning) != 1 || resp.Reasoning[0].Text != "think hard" || resp.Reasoning[0].Provider != url || resp.Reasoning[0].Model != "m" {
		t.Fatalf("reasoning = %+v", resp.Reasoning)
	}
	next := streamReq()
	next.Messages = append(next.Messages,
		Message{Role: RoleAssistant, Content: resp.Content, Reasoning: resp.Reasoning},
		Message{Role: RoleUser, Content: "more"})
	if _, err := pm.Send(context.Background(), url, "m", next); err != nil {
		t.Fatal(err)
	}
	msgs, _ := (*bodies)[1]["messages"].([]any)
	found := false
	for _, m := range msgs {
		if mm, _ := m.(map[string]any); mm["role"] == "assistant" && mm["reasoning_content"] == "think hard" {
			found = true
		}
	}
	if !found {
		t.Fatalf("reasoning_content not replayed: %v", msgs)
	}
}

// A stream that goes silent fails with the retryable ErrStreamIdle instead
// of hanging the run.
func TestStreamIdleTimeout(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, sseChunk(map[string]any{"role": "assistant", "content": "hel"}, "", nil))
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() { close(release); srv.Close() })
	pm := NewManager()
	pm.AddLocalModels([]Model{{Provider: srv.URL, Name: "m", Local: true}})

	req := streamReq()
	req.StreamIdleTimeout = 150 * time.Millisecond
	start := time.Now()
	_, err := pm.Send(context.Background(), srv.URL, "m", req)
	if !errors.Is(err, ErrStreamIdle) {
		t.Fatalf("err = %v, want ErrStreamIdle", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("idle stream took %s to fail", time.Since(start))
	}
	if ClassifyRetry(err) != RetryTransient {
		t.Fatalf("stream idle must be retryable")
	}
}

// With SetStreamAll a caller that wants no live tokens still streams, so the
// idle watchdog protects sub-agent and headless requests too.
func TestStreamAllCoversRequestsWithoutOnStream(t *testing.T) {
	pm, url, bodies := newCompatStreamServer(t,
		sseChunk(map[string]any{"role": "assistant", "content": "ok"}, "stop", map[string]any{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2}),
	)
	pm.SetStreamAll(true)
	resp, err := pm.Send(context.Background(), url, "m", Request{Messages: []Message{{Role: RoleUser, Content: "go"}}})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Content != "ok" {
		t.Fatalf("content = %q", resp.Content)
	}
	if stream, _ := (*bodies)[0]["stream"].(bool); !stream {
		t.Fatal("request did not stream")
	}
}

// A transient failure of a streaming request is left to the caller's retry
// policy: it must not be doubled by the non-streaming fallback request.
func TestStreamTransientFailureIsNotResentWithoutStreaming(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		http.Error(w, `{"error":{"message":"upstream overloaded"}}`, http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)
	pm := NewManager()
	pm.AddLocalModels([]Model{{Provider: srv.URL, Name: "m", Local: true}})
	_, err := pm.Send(context.Background(), srv.URL, "m", streamReq())
	if err == nil {
		t.Fatal("expected an error")
	}
	mu.Lock()
	defer mu.Unlock()
	// The OpenAI SDK's own retries are disabled by fantasy, so one request
	// means no extra non-streaming resend.
	if calls != 1 {
		t.Fatalf("server saw %d requests, want 1", calls)
	}
}

// The input budget counts tool results (most of a coding session's
// context), while the output cap is no longer mistaken for an input limit.
func TestInputBudgetCountsToolResultsAndIsSeparateFromMaxTokens(t *testing.T) {
	pm, url, _ := newCompatStreamServer(t,
		sseChunk(map[string]any{"role": "assistant", "content": "ok"}, "stop", map[string]any{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2}),
	)
	big := strings.Repeat("x", 40000) // ~10k tokens, only in a tool result
	req := streamReq()
	req.Messages = []Message{
		{Role: RoleUser, Content: "go"},
		{Role: RoleAssistant, ToolCalls: []NativeTool{{ID: "c1", Name: "file-read", Args: json.RawMessage(`{}`)}}},
		{Role: RoleUser, ToolResults: []ToolResult{{ID: "c1", Name: "file-read", Output: big}}},
	}
	req.MaxTokens = 100 // an output cap: must not reject a 10k-token prompt
	if _, err := pm.Send(context.Background(), url, "m", req); err != nil {
		t.Fatalf("output cap rejected the prompt: %v", err)
	}
	req.InputBudget = 5000
	if _, err := pm.Send(context.Background(), url, "m", req); err == nil || !strings.Contains(err.Error(), "token budget exceeded") {
		t.Fatalf("err = %v, want the input budget to count the tool result", err)
	}
}

// --- Anthropic SSE fixtures ------------------------------------------------

func anthropicEvent(typ string, data map[string]any) string {
	data["type"] = typ
	raw, _ := json.Marshal(data)
	return "event: " + typ + "\ndata: " + string(raw) + "\n\n"
}

// anthropicThinkingToolStream is a reply with a signed thinking block and a
// tool_use block, ending with stopReason. partialInput is streamed as the
// tool input.
func anthropicThinkingToolStream(stopReason, partialInput string, outputTokens int) []string {
	return []string{
		anthropicEvent("message_start", map[string]any{"message": map[string]any{
			"id": "msg_1", "type": "message", "role": "assistant", "model": "claude-sonnet-4-5",
			"content": []any{}, "stop_reason": nil, "stop_sequence": nil,
			"usage": map[string]any{"input_tokens": 10, "output_tokens": 1},
		}}),
		anthropicEvent("content_block_start", map[string]any{"index": 0, "content_block": map[string]any{"type": "thinking", "thinking": "", "signature": ""}}),
		anthropicEvent("content_block_delta", map[string]any{"index": 0, "delta": map[string]any{"type": "thinking_delta", "thinking": "Let me think."}}),
		anthropicEvent("content_block_delta", map[string]any{"index": 0, "delta": map[string]any{"type": "signature_delta", "signature": "sig-abc"}}),
		anthropicEvent("content_block_stop", map[string]any{"index": 0}),
		anthropicEvent("content_block_start", map[string]any{"index": 1, "content_block": map[string]any{"type": "tool_use", "id": "toolu_1", "name": "file-write", "input": map[string]any{}}}),
		anthropicEvent("content_block_delta", map[string]any{"index": 1, "delta": map[string]any{"type": "input_json_delta", "partial_json": partialInput}}),
		anthropicEvent("content_block_stop", map[string]any{"index": 1}),
		anthropicEvent("message_delta", map[string]any{"delta": map[string]any{"stop_reason": stopReason, "stop_sequence": nil}, "usage": map[string]any{"output_tokens": outputTokens}}),
		anthropicEvent("message_stop", map[string]any{}),
	}
}

func newAnthropicServer(t *testing.T, events []string) (*Manager, *[]map[string]any) {
	t.Helper()
	var mu sync.Mutex
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		mu.Lock()
		bodies = append(bodies, body)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		for _, ev := range events {
			fmt.Fprint(w, ev)
		}
	}))
	t.Cleanup(srv.Close)
	pm := NewManager()
	pm.SetCatalog(models.Catalog{Providers: map[string]models.CatalogProvider{
		"anth-test": {Name: "Anth", API: models.APIAnthropic, BaseURL: srv.URL, Models: map[string]models.CatalogModel{
			"claude-sonnet-4-5": {Name: "Sonnet", Reasoning: true, ToolCall: true},
		}},
	}})
	pm.SetAPIKeys(map[string]string{"anth-test": "k"})
	return pm, &bodies
}

// Anthropic requests always stream, send the model's real output limit
// instead of fantasy's 4096 default, and capture the signed thinking block,
// which is replayed on the next request while thinking is enabled.
func TestAnthropicMaxTokensAndThinkingSignatureRoundTrip(t *testing.T) {
	pm, bodies := newAnthropicServer(t, anthropicThinkingToolStream("tool_use", `{"path":"a.go","content":"x"}`, 30))
	req := Request{Messages: []Message{{Role: RoleUser, Content: "go"}}, Thinking: ThinkingLow}
	resp, err := pm.Send(context.Background(), "anth-test", "claude-sonnet-4-5", req)
	if err != nil {
		t.Fatal(err)
	}
	if got := (*bodies)[0]["max_tokens"]; got != float64(64000) {
		t.Fatalf("max_tokens = %v, want 64000 (known Claude output limit)", got)
	}
	if stream, _ := (*bodies)[0]["stream"].(bool); !stream {
		t.Fatal("anthropic requests must stream even without OnStream")
	}
	if resp.FinishReason != FinishToolCalls || len(resp.ToolCalls) != 1 || resp.ToolCalls[0].ArgsError != "" {
		t.Fatalf("resp = %+v", resp)
	}
	if len(resp.Reasoning) != 1 || resp.Reasoning[0].Signature != "sig-abc" || resp.Reasoning[0].Text != "Let me think." {
		t.Fatalf("reasoning = %+v", resp.Reasoning)
	}

	next := req
	next.Messages = append(next.Messages,
		Message{Role: RoleAssistant, ToolCalls: resp.ToolCalls, Reasoning: resp.Reasoning},
		Message{Role: RoleUser, ToolResults: []ToolResult{{ID: resp.ToolCalls[0].ID, Name: "file-write", Output: "ok"}}})
	if _, err := pm.Send(context.Background(), "anth-test", "claude-sonnet-4-5", next); err != nil {
		t.Fatal(err)
	}
	msgs, _ := (*bodies)[1]["messages"].([]any)
	assistant, _ := msgs[1].(map[string]any)
	blocks, _ := assistant["content"].([]any)
	first, _ := blocks[0].(map[string]any)
	if first["type"] != "thinking" || first["signature"] != "sig-abc" {
		t.Fatalf("assistant turn must open with the signed thinking block, got %v", blocks)
	}
}

// A tool_use block cut by max_tokens reports truncation.
func TestAnthropicTruncatedToolUse(t *testing.T) {
	pm, _ := newAnthropicServer(t, anthropicThinkingToolStream("max_tokens", `{"path":"a.go","content":"pack`, 64000))
	resp, err := pm.Send(context.Background(), "anth-test", "claude-sonnet-4-5", Request{Messages: []Message{{Role: RoleUser, Content: "go"}}})
	if err != nil {
		t.Fatal(err)
	}
	if !resp.Truncated() {
		t.Fatalf("finish = %q, want length", resp.FinishReason)
	}
	if len(resp.ToolCalls) != 1 || !strings.Contains(resp.ToolCalls[0].ArgsError, "truncated") {
		t.Fatalf("tool calls = %+v", resp.ToolCalls)
	}
}
