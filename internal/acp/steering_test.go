package acp

// Mid-run steering over ACP: a session/prompt that arrives while a turn is
// already executing must be delivered to the running agent as steering (at
// its next step boundary) instead of starting — or killing — a new turn.
// The SDK cancels the request context of the running turn when the second
// prompt arrives, so these tests also pin that the agent runs under the
// session-owned context (beginRun) and that only session/cancel stops it.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	acpsdk "github.com/coder/acp-go-sdk"

	"spettro/internal/config"
	"spettro/internal/provider"
)

func TestBeginRun_SecondClaimSteersAndCancelStops(t *testing.T) {
	b := newBridge(Options{})
	s := &acpSession{id: "s1"}
	b.sessions["s1"] = s

	runCtx, finish, ok := b.beginRun(context.Background(), s)
	if !ok {
		t.Fatal("first beginRun must claim the run slot")
	}
	if s.steering == nil {
		t.Fatal("beginRun must create the session steering queue")
	}
	if _, _, ok := b.beginRun(context.Background(), s); ok {
		t.Fatal("second beginRun must report an in-flight run")
	}

	// session/cancel goes through bridge.Cancel and must stop the run.
	if err := b.Cancel(context.Background(), acpsdk.CancelNotification{SessionId: "s1"}); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if runCtx.Err() == nil {
		t.Fatal("Cancel must cancel the run context")
	}

	finish()
	if _, finish2, ok := b.beginRun(context.Background(), s); !ok {
		t.Fatal("run slot must be reclaimable after finish")
	} else {
		finish2()
	}
}

func TestBeginRun_DetachedFromRequestContext(t *testing.T) {
	b := newBridge(Options{})
	s := &acpSession{id: "s1"}
	b.sessions["s1"] = s

	reqCtx, cancelReq := context.WithCancel(context.Background())
	runCtx, finish, ok := b.beginRun(reqCtx, s)
	if !ok {
		t.Fatal("beginRun failed")
	}
	defer finish()
	// The SDK cancels the request context when a new prompt arrives for the
	// session; the run must survive that.
	cancelReq()
	if runCtx.Err() != nil {
		t.Fatal("run context must not inherit request-context cancellation")
	}
}

// steeringCaptureServer serves scripted OpenAI-style responses, records every
// request body, and can hold a response until released.
type steeringCaptureServer struct {
	srv      *httptest.Server
	mu       sync.Mutex
	requests []string
	gate     map[int]chan struct{} // request index → release channel
}

func newSteeringCaptureServer(t *testing.T, responses []string) *steeringCaptureServer {
	t.Helper()
	cs := &steeringCaptureServer{gate: map[int]chan struct{}{}}
	idx := 0
	cs.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		cs.mu.Lock()
		cs.requests = append(cs.requests, string(body))
		i := idx
		idx++
		gate := cs.gate[i]
		cs.mu.Unlock()
		if gate != nil {
			<-gate
		}
		if i >= len(responses) {
			t.Errorf("unexpected extra request #%d", i+1)
			http.Error(w, "no more responses", 500)
			return
		}
		// ACP turns stream (StreamCallback is set), so the fake must speak
		// SSE when asked; plain JSON otherwise.
		if strings.Contains(string(body), `"stream":true`) {
			w.Header().Set("Content-Type", "text/event-stream")
			chunk := map[string]any{
				"id": "chatcmpl-test", "object": "chat.completion.chunk", "model": "fake-model",
				"choices": []map[string]any{
					{"index": 0, "delta": map[string]any{"role": "assistant", "content": responses[i]}},
				},
			}
			done := map[string]any{
				"id": "chatcmpl-test", "object": "chat.completion.chunk", "model": "fake-model",
				"choices": []map[string]any{
					{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"},
				},
				"usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 20, "total_tokens": 30},
			}
			for _, ev := range []map[string]any{chunk, done} {
				raw, _ := json.Marshal(ev)
				fmt.Fprintf(w, "data: %s\n\n", raw)
			}
			fmt.Fprint(w, "data: [DONE]\n\n")
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
	t.Cleanup(cs.srv.Close)
	return cs
}

func (cs *steeringCaptureServer) holdRequest(i int) chan struct{} {
	ch := make(chan struct{})
	cs.mu.Lock()
	cs.gate[i] = ch
	cs.mu.Unlock()
	return ch
}

func (cs *steeringCaptureServer) request(i int) string {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	if i >= len(cs.requests) {
		return ""
	}
	return cs.requests[i]
}

func TestPrompt_ConcurrentPromptSteersRunningTurn(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	cs := newSteeringCaptureServer(t, []string{
		`TOOL_CALL {"tool":"comment","args":{"message":"step one"}}`,
		`TOOL_CALL {"tool":"comment","args":{"message":"step two"}}`,
		"FINAL\nDone, steering applied.",
	})

	// Prompt reloads config from $HOME each turn; point it at the fake model.
	if err := os.MkdirAll(filepath.Join(home, ".spettro"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfgJSON := `{"active_provider":` + strconvQuote(cs.srv.URL) + `,"active_model":"fake-model","permission":"yolo"}`
	if err := os.WriteFile(filepath.Join(home, ".spettro", "config.json"), []byte(cfgJSON), 0o644); err != nil {
		t.Fatal(err)
	}

	pm := provider.NewManager()
	pm.AddLocalModels([]provider.Model{{Provider: cs.srv.URL, Name: "fake-model", Local: true}})

	manifest := config.AgentManifest{Agents: []config.AgentSpec{{
		ID:           "coding",
		Mode:         "worker",
		AllowedTools: []string{"comment"},
		Permission:   config.PermissionYOLO,
		Enabled:      true,
	}}}

	b := newBridge(Options{
		CWD:       t.TempDir(),
		GlobalDir: t.TempDir(),
		Providers: pm,
		Manifest:  manifest,
	})
	// A connection whose peer never speaks: notifications go to io.Discard,
	// the receive loop blocks on an idle pipe.
	pr, pw := io.Pipe()
	t.Cleanup(func() { _ = pw.Close() })
	b.conn = acpsdk.NewAgentSideConnection(b, io.Discard, pr)

	s := &acpSession{
		id:                "sess-steer",
		cwd:               t.TempDir(),
		agentID:           "coding",
		manifest:          manifest,
		mediaDir:          t.TempDir(),
		startedAt:         time.Now(),
		commandsAnnounced: true,
	}
	b.sessions[s.id] = s

	// Hold the second LLM request open so the steering prompt provably lands
	// while the turn is mid-flight; it must then appear in the third request.
	release := cs.holdRequest(1)

	type promptResult struct {
		resp acpsdk.PromptResponse
		err  error
	}
	turn1 := make(chan promptResult, 1)
	go func() {
		resp, err := b.Prompt(context.Background(), acpsdk.PromptRequest{
			SessionId: acpsdk.SessionId(s.id),
			Prompt:    []acpsdk.ContentBlock{acpsdk.TextBlock("Do a multi-step task.")},
		})
		turn1 <- promptResult{resp, err}
	}()

	// Wait until the run is provably in flight (request 2 is being held).
	deadline := time.After(5 * time.Second)
	for cs.request(1) == "" {
		select {
		case <-deadline:
			t.Fatal("timed out waiting for the run to reach step 2")
		case <-time.After(5 * time.Millisecond):
		}
	}

	// Second prompt on the same session while the turn runs → steering.
	resp2, err := b.Prompt(context.Background(), acpsdk.PromptRequest{
		SessionId: acpsdk.SessionId(s.id),
		Prompt:    []acpsdk.ContentBlock{acpsdk.TextBlock("focus only on the README")},
	})
	if err != nil {
		t.Fatalf("steering prompt: %v", err)
	}
	if resp2.StopReason != acpsdk.StopReasonEndTurn {
		t.Fatalf("steering prompt stop reason = %v", resp2.StopReason)
	}
	if s.steering.Len() != 1 {
		t.Fatalf("expected 1 queued steering message, got %d", s.steering.Len())
	}

	close(release)
	res1 := <-turn1
	if res1.err != nil {
		for i := 0; ; i++ {
			r := cs.request(i)
			if r == "" {
				break
			}
			t.Logf("request %d: %s", i, r)
		}
		t.Fatalf("running turn failed: %v", res1.err)
	}
	if res1.resp.StopReason != acpsdk.StopReasonEndTurn {
		t.Fatalf("running turn must complete normally (not cancelled), got %v", res1.resp.StopReason)
	}

	if strings.Contains(cs.request(0), "focus only on the README") ||
		strings.Contains(cs.request(1), "focus only on the README") {
		t.Error("steering must not appear before it was sent")
	}
	req3 := cs.request(2)
	if !strings.Contains(req3, "focus only on the README") || !strings.Contains(req3, "user steering") {
		t.Errorf("steering message (with marker) missing from the next step's request:\n%s", req3)
	}
	if s.steering.Len() != 0 {
		t.Errorf("steering queue should be drained, %d left", s.steering.Len())
	}
	// The steered text must be part of the session's carried conversation.
	found := false
	for _, msg := range s.history {
		if msg.Role == provider.RoleUser && strings.Contains(msg.Content, "focus only on the README") {
			found = true
			break
		}
	}
	if !found {
		t.Error("steering message not adopted into session history")
	}
}

// strconvQuote is a tiny local JSON string quoter (the URL contains no
// characters needing escapes beyond quoting).
func strconvQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
