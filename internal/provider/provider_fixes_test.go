package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"charm.land/fantasy"
	fantasyanthropic "charm.land/fantasy/providers/anthropic"

	"spettro/internal/models"
)

func thinkingBudgetOf(t *testing.T, call fantasy.Call) int64 {
	t.Helper()
	opts, ok := call.ProviderOptions[fantasyanthropic.Name].(*fantasyanthropic.ProviderOptions)
	if !ok || opts.Thinking == nil {
		return 0
	}
	return opts.Thinking.BudgetTokens
}

// The thinking budget must fit under the output cap the manager resolved
// (the model's limit and the room left in the window), never raise it: a
// max_tokens above the model's limit is a hard 400 that no retry fixes.
func TestThinkingBudgetFitsTheResolvedOutputCap(t *testing.T) {
	cases := []struct {
		level ThinkingLevel
		cap   int
	}{
		{ThinkingMax, 64000},   // sonnet 4.5 at /think max
		{ThinkingXHigh, 32000}, // opus 4.1 at x-high
		{ThinkingHigh, 11476},  // 150k prompt on a 200k window
		{ThinkingHigh, 4096},   // no room left: the minimum cap
	}
	for _, c := range cases {
		call := buildFantasyCall("anthropic", models.APIAnthropic, "claude-sonnet-4-5", Request{Prompt: "hi", Thinking: c.level, MaxTokens: c.cap})
		if got := sentMaxOutput(call); got != c.cap {
			t.Errorf("%s at cap %d: max_tokens = %d", c.level, c.cap, got)
		}
		b := thinkingBudgetOf(t, call)
		if b < 1024 || b >= int64(c.cap) {
			t.Errorf("%s at cap %d: budget_tokens = %d, want 1024 <= budget < cap", c.level, c.cap, b)
		}
	}
	// A cap too small for the minimum budget sends no thinking at all.
	call := buildFantasyCall("anthropic", models.APIAnthropic, "claude-sonnet-4-5", Request{Prompt: "hi", Thinking: ThinkingHigh, MaxTokens: 1500})
	if b := thinkingBudgetOf(t, call); b != 0 || sentMaxOutput(call) != 1500 {
		t.Errorf("tiny cap: budget=%d max_tokens=%d", b, sentMaxOutput(call))
	}
}

// After a fallback or model switch mid tool loop, the in-progress tool-use
// turn has no thinking block this model can replay; Anthropic rejects a
// thinking-enabled request whose final assistant turn does not start with
// one, so thinking is left off for that request.
func TestThinkingOffWhenToolTurnCannotReplayItsThinking(t *testing.T) {
	msgs := []Message{
		{Role: RoleUser, Content: "go"},
		{Role: RoleAssistant, ToolCalls: []NativeTool{{ID: "t1", Name: "file-read", Args: json.RawMessage(`{}`)}},
			Reasoning: []ReasoningBlock{{Provider: "anthropic", Model: "claude-opus-4-5", Text: "hm", Signature: "sig"}}},
		{Role: RoleUser, ToolResults: []ToolResult{{ID: "t1", Name: "file-read", Output: "ok"}}},
	}
	req := Request{Messages: msgs, Thinking: ThinkingMedium, MaxTokens: 64000}
	if b := thinkingBudgetOf(t, buildFantasyCall("anthropic", models.APIAnthropic, "claude-opus-4-5", req)); b == 0 {
		t.Fatal("the producing model must keep thinking on")
	}
	if b := thinkingBudgetOf(t, buildFantasyCall("anthropic", models.APIAnthropic, "claude-sonnet-4-5", req)); b != 0 {
		t.Fatalf("thinking enabled (budget %d) on a tool turn with no replayable thinking block", b)
	}
	// Once the loop is past that turn (a new user message), thinking is back.
	next := req
	next.Messages = append(append([]Message{}, msgs...), Message{Role: RoleAssistant, Content: "done"}, Message{Role: RoleUser, Content: "again"})
	if b := thinkingBudgetOf(t, buildFantasyCall("anthropic", models.APIAnthropic, "claude-sonnet-4-5", next)); b == 0 {
		t.Fatal("thinking must be on for a fresh user turn")
	}
	if !isThinkingLevelError(errors.New("messages.1.content.0.type: Expected `thinking` or `redacted_thinking`, but found `tool_use`. When `thinking` is enabled, a final `assistant` message must start with a thinking block")) {
		t.Fatal("the missing-thinking-block rejection must step thinking down")
	}
}

// newCompatCatalogServer is an OpenAI-compatible streaming server reached
// through a catalog provider (not a local endpoint).
func newCompatCatalogServer(t *testing.T, events ...string) *Manager {
	t.Helper()
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		for _, ev := range events {
			fmt.Fprint(w, ev)
		}
	}))
	t.Cleanup(srv.Close)
	pm := NewManager()
	pm.SetCatalog(models.Catalog{Providers: map[string]models.CatalogProvider{
		"compat-test": {Name: "Compat", API: models.APIOpenAI, BaseURL: srv.URL, Models: map[string]models.CatalogModel{
			"m": {Name: "M", ToolCall: true},
		}},
	}})
	pm.SetAPIKeys(map[string]string{"compat-test": "k"})
	return pm
}

// A stream that ends cleanly before the model finished (a proxy closing the
// response mid-generation) must not be accepted as a complete reply.
func TestStreamWithoutFinishIsATransientError(t *testing.T) {
	pm := newCompatCatalogServer(t, sseChunk(map[string]any{"role": "assistant", "content": "Here is the first half of the ans"}, "", nil))
	_, err := pm.Send(context.Background(), "compat-test", "m", streamReq())
	if err == nil {
		t.Fatal("a stream cut before its finish event was accepted")
	}
	if ClassifyRetry(err) != RetryTransient {
		t.Fatalf("class = %v, want transient", ClassifyRetry(err))
	}
	events := anthropicThinkingToolStream("tool_use", `{"path":"a.go","content":"pack`, 30)[:7]
	apm, _ := newAnthropicServer(t, events)
	_, err = apm.Send(context.Background(), "anth-test", "claude-sonnet-4-5", Request{Messages: []Message{{Role: RoleUser, Content: "go"}}})
	if err == nil || ClassifyRetry(err) != RetryTransient {
		t.Fatalf("anthropic stream cut mid tool_use: err=%v", err)
	}
}

// Anthropic streams tool input in ToolCallInput, not Delta: a call that
// never completes must keep its partial input (and so its truncation
// diagnosis) instead of running with {}.
func TestIncompleteToolCallNeverRunsWithEmptyArgs(t *testing.T) {
	calls, finish := finalizeToolCalls([]rawToolCall{{id: "1", name: "file-write", input: "", complete: false}}, FinishToolCalls, 64000, 20)
	if len(calls) != 1 || calls[0].ArgsError == "" {
		t.Fatalf("incomplete call with no input: %+v (finish %q)", calls, finish)
	}
}

// A short call missing its closing brace is malformed, not truncated at
// the output limit: the model must get the parse error, not "split the
// content into smaller pieces".
func TestMissingBraceIsNotReportedAsTruncation(t *testing.T) {
	raw := []rawToolCall{{id: "1", name: "file-read", input: `{"path": "main.go"`, complete: false}}
	calls, finish := finalizeToolCalls(raw, FinishToolCalls, 64000, 20)
	if finish == FinishLength {
		t.Fatal("a 20-token reply was reported as hitting the 64000-token limit")
	}
	if len(calls) != 1 || strings.Contains(calls[0].ArgsError, "truncated") || !strings.Contains(calls[0].ArgsError, "not valid JSON") {
		t.Fatalf("ArgsError = %q", calls[0].ArgsError)
	}
	// Near the cap it is still truncation.
	if _, finish := finalizeToolCalls(raw, FinishToolCalls, 64000, 63990); finish != FinishLength {
		t.Fatalf("finish = %q at the cap, want length", finish)
	}
}

// A quota or billing 429 is not a rate limit: no amount of waiting fixes it.
func TestQuota429IsFatal(t *testing.T) {
	for _, msg := range []string{
		"You exceeded your current quota, please check your plan and billing details. insufficient_quota",
		"Your credit balance is too low to access the Anthropic API.",
	} {
		err := &fantasy.ProviderError{StatusCode: http.StatusTooManyRequests, Message: msg}
		if c := ClassifyRetry(err); c != RetryNever {
			t.Errorf("%q: class = %v, want fatal", msg, c)
		}
	}
	if c := ClassifyRetry(&fantasy.ProviderError{StatusCode: http.StatusTooManyRequests, Message: "Rate limit reached for requests"}); c != RetryTransient {
		t.Errorf("plain rate limit: class = %v, want transient", c)
	}
}

// The truncation message recommends appending over re-quoting the tail.
func TestTruncatedArgsErrorRecommendsAppend(t *testing.T) {
	msg := TruncatedArgsError(8192)
	if !strings.Contains(msg, "append") {
		t.Fatalf("message = %q", msg)
	}
}
