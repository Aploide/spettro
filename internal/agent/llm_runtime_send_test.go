package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"spettro/internal/config"
	"spettro/internal/provider"
)

// loopReply scripts one response of the fake OpenAI-compatible server.
type loopReply struct {
	status    int               // HTTP status; 0 = 200
	header    map[string]string // extra response headers
	errMsg    string            // error body message for non-200 replies
	content   string
	reasoning string
	toolName  string
	toolArgs  string
	finish    string // default: "tool_calls" with a tool, else "stop"
	promptTok int
}

type loopServer struct {
	mu     sync.Mutex
	bodies []map[string]any
}

func (s *loopServer) requests() []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]map[string]any(nil), s.bodies...)
}

// newLoopServer serves the scripted replies in order (non-streaming chat
// completions) and records every request body.
func newLoopServer(t *testing.T, replies ...loopReply) (*provider.Manager, string, *loopServer) {
	t.Helper()
	ls := &loopServer{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		ls.mu.Lock()
		i := len(ls.bodies)
		ls.bodies = append(ls.bodies, body)
		ls.mu.Unlock()
		if i >= len(replies) {
			http.Error(w, `{"error":{"message":"no more scripted replies"}}`, http.StatusBadRequest)
			return
		}
		rep := replies[i]
		for k, v := range rep.header {
			w.Header().Set(k, v)
		}
		w.Header().Set("Content-Type", "application/json")
		if rep.status != 0 && rep.status != http.StatusOK {
			w.WriteHeader(rep.status)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": rep.errMsg}})
			return
		}
		msg := map[string]any{"role": "assistant", "content": rep.content}
		if rep.reasoning != "" {
			msg["reasoning_content"] = rep.reasoning
		}
		finish := rep.finish
		if rep.toolName != "" {
			msg["tool_calls"] = []any{map[string]any{
				"id": fmt.Sprintf("call_%d", i), "type": "function",
				"function": map[string]any{"name": rep.toolName, "arguments": rep.toolArgs},
			}}
			if finish == "" {
				finish = "tool_calls"
			}
		}
		if finish == "" {
			finish = "stop"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "chatcmpl-test", "object": "chat.completion",
			"choices": []any{map[string]any{"index": 0, "message": msg, "finish_reason": finish}},
			"usage":   map[string]any{"prompt_tokens": rep.promptTok, "completion_tokens": 5, "total_tokens": rep.promptTok + 5},
		})
	}))
	t.Cleanup(srv.Close)
	pm := provider.NewManager()
	pm.AddLocalModels([]provider.Model{{Provider: srv.URL, Name: "m", Local: true}})
	return pm, srv.URL, ls
}

func loopCfg(t *testing.T, pm *provider.Manager, url string) toolLoopConfig {
	t.Helper()
	return toolLoopConfig{
		SystemPrompt:    "sys",
		UserTask:        "do the task",
		CWD:             t.TempDir(),
		AllowedTools:    []string{"file-read", "file-write", "comment"},
		ProviderManager: pm,
		ProviderName:    func() string { return url },
		ModelName:       func() string { return "m" },
		Permission:      config.PermissionYOLO,
	}
}

// fastRetries shrinks the retry backoff for the duration of a test.
func fastRetries(t *testing.T) {
	t.Helper()
	saved := provider.DefaultRetryPolicy
	provider.DefaultRetryPolicy.BaseDelay = time.Millisecond
	provider.DefaultRetryPolicy.MaxDelay = 5 * time.Millisecond
	t.Cleanup(func() { provider.DefaultRetryPolicy = saved })
}

// lastUserText returns the text of the last plain user message of a recorded
// chat-completions request body.
func lastUserText(body map[string]any) string {
	msgs, _ := body["messages"].([]any)
	for i := len(msgs) - 1; i >= 0; i-- {
		m, _ := msgs[i].(map[string]any)
		if m["role"] == "user" {
			s, _ := m["content"].(string)
			if s == "" {
				if parts, ok := m["content"].([]any); ok && len(parts) > 0 {
					p, _ := parts[0].(map[string]any)
					s, _ = p["text"].(string)
				}
			}
			return s
		}
	}
	return ""
}

func TestRunToolLoopEmptyRepliesNudgeThenAnswer(t *testing.T) {
	pm, url, ls := newLoopServer(t, loopReply{content: ""}, loopReply{content: "done"})
	res, err := runToolLoop(context.Background(), loopCfg(t, pm, url))
	if err != nil {
		t.Fatal(err)
	}
	if res.content != "done" {
		t.Fatalf("content = %q", res.content)
	}
	reqs := ls.requests()
	if len(reqs) != 2 {
		t.Fatalf("requests = %d, want 2", len(reqs))
	}
	if got := lastUserText(reqs[1]); got != emptyReplyNudge {
		t.Fatalf("second request must carry the nudge, last user text = %q", got)
	}
}

func TestRunToolLoopEmptyRepliesEndTurnWithError(t *testing.T) {
	pm, url, ls := newLoopServer(t, loopReply{}, loopReply{}, loopReply{}, loopReply{content: "never reached"})
	res, err := runToolLoop(context.Background(), loopCfg(t, pm, url))
	if err == nil || !strings.Contains(err.Error(), "3 empty responses") {
		t.Fatalf("err = %v, want the empty-response error", err)
	}
	if n := len(ls.requests()); n != maxEmptyReplies {
		t.Fatalf("requests = %d, want %d (never resent forever)", n, maxEmptyReplies)
	}
	for _, m := range res.messages {
		if m.Content == emptyReplyNudge {
			t.Fatal("the nudges must not stay in the carried history of a failed turn")
		}
	}
}

// Compaction during an empty-reply streak rewrites the history; giving up
// must still drop the nudges by content, not by a stale index (which used to
// panic with a slice-bounds error).
func TestRunToolLoopEmptyStreakSurvivesCompaction(t *testing.T) {
	fastRetries(t)
	pm, url, _ := newLoopServer(t,
		loopReply{}, // empty → nudge 1
		loopReply{status: http.StatusBadRequest, errMsg: "This model's maximum context length is 100000 tokens"},
		loopReply{content: "SUMMARY OF EARLIER WORK"}, // the compaction summarizer
		loopReply{}, // empty → nudge 2
		loopReply{}, // empty → give up
	)
	cfg := loopCfg(t, pm, url)
	for i := range 40 {
		role := provider.RoleUser
		if i%2 == 1 {
			role = provider.RoleAssistant
		}
		cfg.Messages = append(cfg.Messages, provider.Message{Role: role, Content: fmt.Sprintf("earlier turn %d", i)})
	}
	res, err := runToolLoop(context.Background(), cfg)
	if err == nil || !strings.Contains(err.Error(), "3 empty responses") {
		t.Fatalf("err = %v, want the empty-response error", err)
	}
	if !strings.Contains(fmt.Sprint(res.messages), "[earlier progress summarized]") {
		t.Fatal("the carried history must be the compacted one")
	}
	for _, m := range res.messages {
		if m.Content == emptyReplyNudge {
			t.Fatal("the nudges must not stay in the carried history of a failed turn")
		}
	}
}

// Steering the user sent during an empty-reply streak stays in the carried
// history when the run gives up; only the nudges are removed.
func TestRunToolLoopEmptyStreakKeepsSteering(t *testing.T) {
	pm, url, _ := newLoopServer(t, loopReply{}, loopReply{}, loopReply{})
	cfg := loopCfg(t, pm, url)
	cfg.Steering = NewSteeringQueue()
	pushed := false
	cfg.ToolCallback = func(tr ToolTrace) {
		if !pushed && strings.Contains(tr.Output, "empty response") {
			pushed = true
			cfg.Steering.Push("focus on the tests")
		}
	}
	res, err := runToolLoop(context.Background(), cfg)
	if err == nil {
		t.Fatal("expected the empty-response error")
	}
	var steering, nudges int
	for _, m := range res.messages {
		switch {
		case m.Content == emptyReplyNudge:
			nudges++
		case strings.HasPrefix(m.Content, steeringMessagePrefix):
			steering++
		}
	}
	if steering != 1 || nudges != 0 {
		t.Fatalf("steering turns = %d (want 1), nudges = %d (want 0)", steering, nudges)
	}
}

func TestDropEmptyNudges(t *testing.T) {
	u := func(c string) provider.Message { return provider.Message{Role: provider.RoleUser, Content: c} }
	a := provider.Message{Role: provider.RoleAssistant, Content: "ok"}
	msgs := []provider.Message{u("task"), u(emptyReplyNudge), a, u("results"), u(emptyReplyNudge), u("steer"), u(emptyTruncatedNudge)}
	got := dropEmptyNudges(msgs, 2)
	var contents []string
	for _, m := range got {
		contents = append(contents, m.Content)
	}
	want := []string{"task", emptyReplyNudge, "ok", "results", "steer"}
	if strings.Join(contents, "|") != strings.Join(want, "|") {
		t.Fatalf("got %q, want %q (an earlier, recovered streak's nudge is kept)", contents, want)
	}
	if len(msgs) != 7 || msgs[6].Content != emptyTruncatedNudge {
		t.Fatal("the input slice must not be modified")
	}
}

func TestRunToolLoopTruncatedTextIsContinued(t *testing.T) {
	pm, url, ls := newLoopServer(t,
		loopReply{content: "The answer is forty", finish: "length"},
		loopReply{content: "-two.", finish: "stop"},
	)
	res, err := runToolLoop(context.Background(), loopCfg(t, pm, url))
	if err != nil {
		t.Fatal(err)
	}
	if res.content != "The answer is forty-two." {
		t.Fatalf("content = %q, want the joined continuation", res.content)
	}
	reqs := ls.requests()
	if len(reqs) != 2 || lastUserText(reqs[1]) != continueTruncatedNudge {
		t.Fatalf("the cut answer must be continued, requests = %d", len(reqs))
	}
	// The history holds the pieces, not a duplicated joined copy.
	var assistant []string
	for _, m := range res.messages {
		if m.Role == provider.RoleAssistant {
			assistant = append(assistant, m.Content)
		}
	}
	if strings.Join(assistant, "|") != "The answer is forty|-two." {
		t.Fatalf("assistant turns = %q", assistant)
	}
}

func TestRunToolLoopTruncatedToolCallIsNotExecuted(t *testing.T) {
	pm, url, ls := newLoopServer(t,
		loopReply{toolName: "file-write", toolArgs: `{"path":"big.go","content":"package main\nfunc main() {`, finish: "length"},
		loopReply{content: "ok, will split"},
	)
	cfg := loopCfg(t, pm, url)
	res, err := runToolLoop(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, statErr := os.Stat(filepath.Join(cfg.CWD, "big.go")); !os.IsNotExist(statErr) {
		t.Fatal("a truncated file-write must never be executed")
	}
	if len(res.traces) != 1 || res.traces[0].Status != "error" || !strings.Contains(res.traces[0].Output, "truncated at the output token limit") {
		t.Fatalf("traces = %+v", res.traces)
	}
	msgs, _ := ls.requests()[1]["messages"].([]any)
	found := false
	for _, m := range msgs {
		if mm, _ := m.(map[string]any); mm["role"] == "tool" && strings.Contains(fmt.Sprint(mm["content"]), "truncated at the output token limit") {
			found = true
		}
	}
	if !found {
		t.Fatalf("the model must be told its call was truncated: %v", msgs)
	}
}

func TestRunToolLoopRetriesTransientFailures(t *testing.T) {
	fastRetries(t)
	pm, url, ls := newLoopServer(t,
		loopReply{status: http.StatusServiceUnavailable, errMsg: "overloaded"},
		loopReply{status: http.StatusTooManyRequests, errMsg: "slow down"},
		loopReply{content: "done"},
	)
	res, err := runToolLoop(context.Background(), loopCfg(t, pm, url))
	if err != nil {
		t.Fatal(err)
	}
	if res.content != "done" || len(ls.requests()) != 3 {
		t.Fatalf("content = %q after %d requests", res.content, len(ls.requests()))
	}
}

func TestRunToolLoopHonorsRetryAfter(t *testing.T) {
	saved := provider.DefaultRetryPolicy
	// A backoff this long would time the test out: only the server's
	// Retry-After hint can make the retry fast.
	provider.DefaultRetryPolicy.BaseDelay = time.Hour
	provider.DefaultRetryPolicy.MaxDelay = time.Hour
	t.Cleanup(func() { provider.DefaultRetryPolicy = saved })
	pm, url, _ := newLoopServer(t,
		loopReply{status: http.StatusTooManyRequests, errMsg: "rate limited", header: map[string]string{"Retry-After-Ms": "20"}},
		loopReply{content: "done"},
	)
	start := time.Now()
	res, err := runToolLoop(context.Background(), loopCfg(t, pm, url))
	if err != nil || res.content != "done" {
		t.Fatalf("res = %q, err = %v", res.content, err)
	}
	if time.Since(start) > 10*time.Second {
		t.Fatal("Retry-After was not honored")
	}
}

func TestRunToolLoopDoesNotRetryAuthErrors(t *testing.T) {
	fastRetries(t)
	pm, url, ls := newLoopServer(t,
		loopReply{status: http.StatusUnauthorized, errMsg: "invalid api key"},
		loopReply{content: "never reached"},
	)
	if _, err := runToolLoop(context.Background(), loopCfg(t, pm, url)); err == nil {
		t.Fatal("expected the auth error")
	}
	if n := len(ls.requests()); n != 1 {
		t.Fatalf("requests = %d, want 1 (auth errors are not retried)", n)
	}
}

func TestRunToolLoopContextOverflowCompactsAndRetries(t *testing.T) {
	fastRetries(t)
	pm, url, ls := newLoopServer(t,
		loopReply{status: http.StatusBadRequest, errMsg: "This model's maximum context length is 8192 tokens. However, you requested 9000 tokens"},
		loopReply{content: "SUMMARY OF EARLIER WORK"}, // the compaction summarizer
		loopReply{content: "done"},
	)
	cfg := loopCfg(t, pm, url)
	for i := range 6 {
		role := provider.RoleUser
		if i%2 == 1 {
			role = provider.RoleAssistant
		}
		cfg.Messages = append(cfg.Messages, provider.Message{Role: role, Content: fmt.Sprintf("earlier turn %d", i)})
	}
	res, err := runToolLoop(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if res.content != "done" {
		t.Fatalf("content = %q", res.content)
	}
	reqs := ls.requests()
	if len(reqs) != 3 {
		t.Fatalf("requests = %d, want overflow + summarizer + retry", len(reqs))
	}
	if !strings.Contains(fmt.Sprint(reqs[2]["messages"]), "[earlier progress summarized]") {
		t.Fatal("the retried request must carry the compacted history")
	}
}

func TestRunToolLoopStoresReasoningOnAssistantTurns(t *testing.T) {
	pm, url, ls := newLoopServer(t,
		loopReply{reasoning: "I should read the file", toolName: "comment", toolArgs: `{"message":"hi"}`},
		loopReply{content: "done"},
	)
	res, err := runToolLoop(context.Background(), loopCfg(t, pm, url))
	if err != nil {
		t.Fatal(err)
	}
	var withReasoning *provider.Message
	for i := range res.messages {
		if len(res.messages[i].ToolCalls) > 0 {
			withReasoning = &res.messages[i]
		}
	}
	if withReasoning == nil || len(withReasoning.Reasoning) != 1 || withReasoning.Reasoning[0].Text != "I should read the file" {
		t.Fatalf("assistant tool turn must keep its reasoning: %+v", withReasoning)
	}
	// And it is sent back on the next step (reasoning_content round-trip).
	if !strings.Contains(fmt.Sprint(ls.requests()[1]["messages"]), "I should read the file") {
		t.Fatal("reasoning must be replayed to the same model")
	}
}

// A thinking level the model rejects is stepped down once and remembered for
// the rest of the run instead of being re-sent (and re-rejected) every step.
func TestRunToolLoopRemembersDowngradedThinking(t *testing.T) {
	pm, url, ls := newLoopServer(t,
		loopReply{status: http.StatusBadRequest, errMsg: "unsupported value for reasoning_effort"},
		loopReply{toolName: "comment", toolArgs: `{"message":"step"}`},
		loopReply{content: "done"},
	)
	cfg := loopCfg(t, pm, url)
	cfg.Thinking = provider.ThinkingLow
	if _, err := runToolLoop(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	reqs := ls.requests()
	if len(reqs) != 3 {
		t.Fatalf("requests = %d, want 3 (one rejected attempt only)", len(reqs))
	}
	if _, has := reqs[2]["reasoning_effort"]; has {
		t.Fatal("the rejected reasoning_effort was sent again on a later step")
	}
}

func TestUsageCalibration(t *testing.T) {
	var c usageCalibration
	if got := c.apply(1000); got != 1000 {
		t.Fatalf("uncalibrated = %d", got)
	}
	c.observe(1500, 1000) // the real tokenizer counts 1.5x the local estimate
	if got := c.apply(2000); got != 3000 {
		t.Fatalf("calibrated = %d, want 3000", got)
	}
	c.observe(100000, 1000) // absurd report: ratio clamped at 3x
	if got := c.apply(1000); got != 3000 {
		t.Fatalf("clamped = %d, want 3000", got)
	}
	c.observe(0, 1000) // no usage reported: keep the previous calibration
	if got := c.apply(1000); got != 3000 {
		t.Fatalf("after empty report = %d", got)
	}
}

// An overflow on a short history (nothing to compact) whose error states a
// smaller window than the request was sized for is resent once with that
// window, instead of failing the run.
func TestRunToolLoopOverflowWithNothingToCompactResendsWithLearnedWindow(t *testing.T) {
	fastRetries(t)
	pm, url, ls := newLoopServer(t,
		loopReply{status: http.StatusBadRequest, errMsg: "input length and max_tokens exceed context limit: 188240 + 32000 > 200000"},
		loopReply{content: "done"},
	)
	res, err := runToolLoop(context.Background(), loopCfg(t, pm, url))
	if err != nil {
		t.Fatal(err)
	}
	if res.content != "done" || len(ls.requests()) != 2 {
		t.Fatalf("content = %q after %d requests", res.content, len(ls.requests()))
	}
}
