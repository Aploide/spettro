package remote_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"spettro/internal/remote"
)

// newTestClient returns an HTTP client with keep-alives disabled. This prevents
// the shared connection pool from reusing stale connections when parallel tests
// happen to get the same port as a previously completed test.
func newTestClient() *http.Client {
	return &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
}

// startTestServer brings up a Server bound to an OS-assigned port and arranges
// for it to be torn down at the end of the test. Returns the base URL and the
// auth token that must be presented on every request.
func startTestServer(t *testing.T) (*remote.Server, string) {
	t.Helper()
	srv, err := remote.NewServer(remote.Options{Token: "test-token"})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	port, _, err := srv.Start(0) // 0 means scan from DefaultPort, but we still bind 127.0.0.1
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if port == 0 {
		t.Fatalf("Start returned port 0")
	}
	t.Cleanup(func() { _ = srv.Stop() })
	return srv, fmt.Sprintf("http://127.0.0.1:%d", port)
}

func authedRequest(t *testing.T, method, url, token string, body any) *http.Request {
	t.Helper()
	var reader io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		reader = bytes.NewReader(buf)
	}
	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req
}

func TestServer_AuthRequired(t *testing.T) {
	t.Parallel()
	_, base := startTestServer(t)

	resp, err := newTestClient().Get(base + "/status")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", resp.StatusCode)
	}
}

func TestServer_StatusReflectsSetStatus(t *testing.T) {
	t.Parallel()
	srv, base := startTestServer(t)

	srv.SetStatus(remote.Status{
		Thinking:      true,
		Mode:          "coding",
		ActiveAgent:   "coding",
		MessagesCount: 7,
		TokensUsed:    1234,
	})

	req := authedRequest(t, http.MethodGet, base+"/status", "test-token", nil)
	resp, err := newTestClient().Do(req)
	if err != nil {
		t.Fatalf("get status: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status code: %d", resp.StatusCode)
	}

	var out remote.Status
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Mode != "coding" || !out.Thinking || out.MessagesCount != 7 || out.TokensUsed != 1234 {
		t.Fatalf("unexpected status: %+v", out)
	}
}

func TestServer_MessagesEnqueuesAndAcceptsReply(t *testing.T) {
	t.Parallel()
	srv, base := startTestServer(t)

	// Drain the submission channel from a goroutine so the HTTP handler
	// doesn't block waiting for a reply.
	got := make(chan string, 1)
	go func() {
		req := <-srv.Submissions()
		got <- req.Message
		req.Reply <- remote.SubmitResponse{Accepted: true, Note: "running"}
	}()

	httpReq := authedRequest(t, http.MethodPost, base+"/messages", "test-token", map[string]string{"message": "hello world"})
	resp, err := newTestClient().Do(httpReq)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d: %s", resp.StatusCode, string(body))
	}
	var out remote.SubmitResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !out.Accepted || out.Note != "running" {
		t.Fatalf("unexpected response: %+v", out)
	}

	select {
	case msg := <-got:
		if msg != "hello world" {
			t.Fatalf("expected hello world, got %q", msg)
		}
	case <-time.After(time.Second):
		t.Fatal("submission was never delivered")
	}
}

func TestServer_RejectsEmptyMessage(t *testing.T) {
	t.Parallel()
	_, base := startTestServer(t)

	req := authedRequest(t, http.MethodPost, base+"/messages", "test-token", map[string]string{"message": "   "})
	resp, err := newTestClient().Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
}

func TestServer_InterruptDelivers(t *testing.T) {
	t.Parallel()
	srv, base := startTestServer(t)

	req := authedRequest(t, http.MethodPost, base+"/interrupt", "test-token", nil)
	resp, err := newTestClient().Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}

	select {
	case <-srv.Interrupts():
	case <-time.After(time.Second):
		t.Fatal("interrupt was never delivered")
	}
}

func TestServer_PortFallbackWhenBusy(t *testing.T) {
	// Bind a port to be sure preferredPort is unavailable.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	busyPort := ln.Addr().(*net.TCPAddr).Port

	srv, err := remote.NewServer(remote.Options{Token: "t"})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	t.Cleanup(func() { _ = srv.Stop() })

	port, fellBack, err := srv.Start(busyPort)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !fellBack {
		t.Fatalf("expected fallback when port %d is busy", busyPort)
	}
	if port == busyPort {
		t.Fatalf("server should not have bound the busy port %d", busyPort)
	}
}

func TestServer_EventsStreamReceivesPublishedEvents(t *testing.T) {
	t.Parallel()
	srv, base := startTestServer(t)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	t.Cleanup(cancel)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/events", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer test-token")

	resp, err := newTestClient().Do(req)
	if err != nil {
		t.Fatalf("get events: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); !strings.HasPrefix(got, "text/event-stream") {
		t.Fatalf("unexpected content type: %q", got)
	}

	// Publish AFTER the connection is established so the event flows through
	// the live subscription rather than the replay buffer.
	go func() {
		time.Sleep(50 * time.Millisecond)
		srv.Publish("test", map[string]interface{}{"hello": "world"})
	}()

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	done := make(chan string, 1)
	go func() {
		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, "data: ") {
				done <- strings.TrimPrefix(line, "data: ")
				return
			}
		}
		done <- ""
	}()

	select {
	case data := <-done:
		if data == "" {
			t.Fatal("did not receive any SSE data")
		}
		var ev remote.Event
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			t.Fatalf("decode event: %v\nraw: %s", err, data)
		}
		if ev.Kind != "test" {
			t.Fatalf("unexpected kind: %q", ev.Kind)
		}
		if got := ev.Data["hello"]; got != "world" {
			t.Fatalf("unexpected payload: %v", ev.Data)
		}
	case <-deadline.C:
		t.Fatal("timed out waiting for SSE event")
	}
}

func TestServer_RejectsRunningTwice(t *testing.T) {
	t.Parallel()
	srv, err := remote.NewServer(remote.Options{Token: "t"})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	t.Cleanup(func() { _ = srv.Stop() })

	if _, _, err := srv.Start(0); err != nil {
		t.Fatalf("first Start: %v", err)
	}
	if _, _, err := srv.Start(0); err == nil {
		t.Fatal("expected error on second Start")
	}
}

func TestServer_StopIsIdempotent(t *testing.T) {
	t.Parallel()
	srv, _ := startTestServer(t)
	if err := srv.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := srv.Stop(); err != nil {
		t.Fatalf("second Stop: %v", err)
	}
}

// TestServer_PublishRaceWithDisconnect exercises the formerly-broken cleanup
// path: a client connects to /events, disconnects, and the server keeps
// publishing. Before the fix, the handler's defer closed the per-subscriber
// channel under lock, but Publish snapshotted the subscribers map BEFORE
// release, then sent on those channels AFTER release. When the cleanup
// raced with the fan-out, Publish would send to a closed channel and panic
// (the `default` branch on a select does NOT rescue sends to a closed
// channel). This test loops enough times under `-race` to surface that bug
// reliably; it must complete without crashing the test binary.
func TestServer_PublishRaceWithDisconnect(t *testing.T) {
	srv, base := startTestServer(t)

	for i := 0; i < 30; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/events", nil)
		if err != nil {
			t.Fatalf("new req: %v", err)
		}
		req.Header.Set("Authorization", "Bearer test-token")
		resp, err := newTestClient().Do(req)
		if err != nil {
			cancel()
			t.Fatalf("connect /events: %v", err)
		}
		// Drain a couple of bytes so we know the handler is up before we
		// disconnect, then immediately drop the connection while flooding
		// the publisher.
		go func() {
			buf := make([]byte, 512)
			_, _ = resp.Body.Read(buf)
		}()
		// Cause the cleanup defer to run roughly concurrently with the
		// publish below.
		cancel()
		_ = resp.Body.Close()
		for j := 0; j < 100; j++ {
			srv.Publish("test", map[string]interface{}{"i": i, "j": j})
		}
	}
}

func TestServer_RejectsNonLoopbackHost(t *testing.T) {
	t.Parallel()
	_, base := startTestServer(t)

	// A DNS-rebinding attempt: valid token, but the Host header points at an
	// attacker-controlled name that resolved to 127.0.0.1.
	req := authedRequest(t, http.MethodGet, base+"/status", "test-token", nil)
	req.Host = "evil.example.com"
	resp, err := newTestClient().Do(req)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 for non-loopback Host, got %d", resp.StatusCode)
	}
}

func TestServer_QueryTokenOnlyAllowedOnEvents(t *testing.T) {
	t.Parallel()
	srv, base := startTestServer(t)

	// A POST endpoint must NOT accept the token via query parameter.
	req, err := http.NewRequest(http.MethodPost, base+"/interrupt?token=test-token", nil)
	if err != nil {
		t.Fatalf("new req: %v", err)
	}
	resp, err := newTestClient().Do(req)
	if err != nil {
		t.Fatalf("post interrupt: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 for query token on POST, got %d", resp.StatusCode)
	}

	// The SSE stream may still authenticate via query parameter.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	evReq, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/events?token=test-token", nil)
	if err != nil {
		t.Fatalf("new events req: %v", err)
	}
	evResp, err := newTestClient().Do(evReq)
	if err != nil {
		t.Fatalf("get events: %v", err)
	}
	defer evResp.Body.Close()
	if evResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for query token on /events, got %d", evResp.StatusCode)
	}
	_ = srv
}

func TestServer_RejectsOversizedBody(t *testing.T) {
	t.Parallel()
	_, base := startTestServer(t)

	huge := bytes.NewReader(make([]byte, (1<<20)+1024)) // > 1 MiB cap
	req, err := http.NewRequest(http.MethodPost, base+"/messages", huge)
	if err != nil {
		t.Fatalf("new req: %v", err)
	}
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	resp, err := newTestClient().Do(req)
	if err != nil {
		// The server stops reading and tears down the connection once the
		// body exceeds the cap — the client sees a write/EOF error. That is a
		// valid rejection.
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Fatalf("expected oversized body to be rejected, got 200")
	}
}
