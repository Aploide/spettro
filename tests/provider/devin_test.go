package provider_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"spettro/internal/provider"
)

// devinScript drives the httptest server to walk a v1 session through:
// create → working → finished. The created session id and url are
// captured via an atomic so concurrent tests can verify them.
type devinScript struct {
	pollsBeforeFinish int32 // number of "working" responses before the first "finished" reply
	createCount       int32
	pollCount         int32
	finalMessage      string
	failOnCreate      bool
}

func newDevinV1Server(t *testing.T, sc *devinScript) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "Bearer ") {
			http.Error(w, "missing auth", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/sessions":
			atomic.AddInt32(&sc.createCount, 1)
			if sc.failOnCreate {
				w.WriteHeader(http.StatusBadRequest)
				w.Write([]byte(`{"detail":"missing prompt"}`))
				return
			}
			json.NewEncoder(w).Encode(map[string]any{
				"session_id":     "devin-fake-1",
				"url":            "https://app.devin.ai/sessions/devin-fake-1",
				"is_new_session": true,
			})
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/sessions/"):
			n := atomic.AddInt32(&sc.pollCount, 1)
			if n <= sc.pollsBeforeFinish {
				json.NewEncoder(w).Encode(map[string]any{
					"session_id":  "devin-fake-1",
					"status":      "working",
					"status_enum": "working",
				})
				return
			}
			json.NewEncoder(w).Encode(map[string]any{
				"session_id":  "devin-fake-1",
				"status":      "finished",
				"status_enum": "finished",
				"messages": []map[string]any{
					{"type": "user_message", "message": "hi", "timestamp": "2026-01-01T00:00:00Z", "event_id": "1"},
					{"type": "devin_message", "message": sc.finalMessage, "timestamp": "2026-01-01T00:00:01Z", "event_id": "2"},
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestDevinAdapter_V1_CreateAndPoll(t *testing.T) {
	sc := &devinScript{pollsBeforeFinish: 2, finalMessage: "hello from devin"}
	srv := newDevinV1Server(t, sc)

	a := provider.DevinAdapter{
		APIKey:       "apk_test",
		BaseURL:      srv.URL,
		PollInterval: 5 * time.Millisecond,
		MaxWait:      5 * time.Second,
	}
	resp, err := a.Send(context.Background(), "session", provider.Request{Prompt: "do something"})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if !strings.Contains(resp.Content, "hello from devin") {
		t.Fatalf("expected session content in response, got %q", resp.Content)
	}
	if !strings.Contains(resp.Content, "https://app.devin.ai/sessions/devin-fake-1") {
		t.Fatalf("expected session url footer, got %q", resp.Content)
	}
	if atomic.LoadInt32(&sc.createCount) != 1 {
		t.Fatalf("expected 1 create call, got %d", sc.createCount)
	}
	// 2 working + 1 finished poll = 3 total.
	if atomic.LoadInt32(&sc.pollCount) < 3 {
		t.Fatalf("expected at least 3 polls (2 working + 1 finished), got %d", sc.pollCount)
	}
}

func TestDevinAdapter_V1_RejectsEmptyKey(t *testing.T) {
	a := provider.DevinAdapter{APIKey: ""}
	_, err := a.Send(context.Background(), "session", provider.Request{Prompt: "x"})
	if err == nil || !strings.Contains(err.Error(), "API key is required") {
		t.Fatalf("expected missing-key error, got %v", err)
	}
}

func TestDevinAdapter_V3_RequiresOrgID(t *testing.T) {
	// cog_ keys must be paired with an org id; the adapter should refuse
	// to call /v3 without it.
	a := provider.DevinAdapter{APIKey: "cog_test"}
	_, err := a.Send(context.Background(), "session", provider.Request{Prompt: "x"})
	if err == nil || !strings.Contains(err.Error(), "organization id") {
		t.Fatalf("expected missing-org-id error, got %v", err)
	}
}

// TestDevinAdapter_V1_HandlesContextCancel verifies that a /interrupt while
// the adapter is mid-poll exits within ~one poll interval, not after the
// max-wait timeout.
func TestDevinAdapter_V1_HandlesContextCancel(t *testing.T) {
	// Server keeps responding "working" forever.
	sc := &devinScript{pollsBeforeFinish: 1_000_000, finalMessage: "never"}
	srv := newDevinV1Server(t, sc)

	a := provider.DevinAdapter{
		APIKey:       "apk_test",
		BaseURL:      srv.URL,
		PollInterval: 50 * time.Millisecond,
		MaxWait:      30 * time.Second,
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	_, err := a.Send(ctx, "session", provider.Request{Prompt: "long task"})
	elapsed := time.Since(start)
	if err == nil || !strings.Contains(err.Error(), "context canceled") {
		t.Fatalf("expected context cancellation error, got %v", err)
	}
	if elapsed > time.Second {
		t.Fatalf("cancel did not stop polling promptly: elapsed=%s", elapsed)
	}
}

func newDevinV3Server(t *testing.T, finalContent string) *httptest.Server {
	t.Helper()
	var pollCount int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		path := r.URL.Path
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(path, "/sessions"):
			fmt.Fprintf(w, `{"session_id":"devin-v3-1","url":"https://app.devin.ai/sessions/devin-v3-1","status":"new","tags":[],"org_id":"org-test","created_at":1,"updated_at":1,"acus_consumed":0,"pull_requests":[],"origin":"api"}`)
		case r.Method == http.MethodGet && strings.HasSuffix(path, "/sessions/devin-v3-1"):
			n := atomic.AddInt32(&pollCount, 1)
			if n < 2 {
				fmt.Fprintf(w, `{"session_id":"devin-v3-1","status":"running","status_detail":"working","tags":[],"org_id":"org-test","created_at":1,"updated_at":1,"acus_consumed":0,"pull_requests":[],"origin":"api","url":"x"}`)
				return
			}
			fmt.Fprintf(w, `{"session_id":"devin-v3-1","status":"running","status_detail":"finished","tags":[],"org_id":"org-test","created_at":1,"updated_at":1,"acus_consumed":1,"pull_requests":[],"origin":"api","url":"x"}`)
		case r.Method == http.MethodGet && strings.HasSuffix(path, "/messages"):
			fmt.Fprintf(w,
				`{"items":[{"event_id":"1","source":"user","message":"hi","created_at":1},{"event_id":"2","source":"devin","message":%q,"created_at":2}],"has_next_page":false}`,
				finalContent)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestDevinAdapter_V3_CreatePollMessages(t *testing.T) {
	srv := newDevinV3Server(t, "v3 final answer")
	a := provider.DevinAdapter{
		APIKey:       "cog_test",
		OrgID:        "org-test",
		BaseURL:      srv.URL,
		PollInterval: 5 * time.Millisecond,
		MaxWait:      5 * time.Second,
	}
	resp, err := a.Send(context.Background(), "session", provider.Request{Prompt: "v3 task"})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if !strings.Contains(resp.Content, "v3 final answer") {
		t.Fatalf("expected final agent message in response, got %q", resp.Content)
	}
}
