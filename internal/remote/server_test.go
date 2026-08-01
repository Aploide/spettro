package remote

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// testHTTPClient never reuses TCP connections. http.DefaultClient keeps idle
// conns in a process-wide pool; when tests sequentially bind DefaultPort
// (7878) on a fresh Server after a previous one stopped, a pooled conn can
// hit a half-closed socket and surface as "Post ...: EOF" — common on CI.
var testHTTPClient = &http.Client{
	Transport: &http.Transport{
		DisableKeepAlives: true,
	},
	Timeout: 5 * time.Second,
}

func startTestServer(t *testing.T) *Server {
	t.Helper()
	s, err := NewServer(Options{Token: "test-token"})
	if err != nil {
		t.Fatal(err)
	}
	// Bind an ephemeral port instead of scanning from DefaultPort (7878).
	// Production still uses Start(0); tests must not serialize on a fixed
	// port or invite keep-alive reuse against a rebound address after Stop.
	free, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := free.Addr().(*net.TCPAddr).Port
	_ = free.Close()
	if _, _, err := s.Start(port); err != nil {
		t.Fatal(err)
	}
	// Confirm the accept loop is up before the first request (slow CI).
	deadline := time.Now().Add(2 * time.Second)
	addr := fmt.Sprintf("127.0.0.1:%d", s.Port())
	for {
		conn, err := net.DialTimeout("tcp", addr, 50*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("server not accepting on %s: %v", addr, err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Cleanup(func() { _ = s.Stop() })
	return s
}

func doReq(t *testing.T, s *Server, method, path, token, body string) *http.Response {
	t.Helper()
	url := fmt.Sprintf("http://127.0.0.1:%d%s", s.Port(), path)
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := testHTTPClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { io.Copy(io.Discard, resp.Body); resp.Body.Close() })
	return resp
}

func TestAuthRequired(t *testing.T) {
	s := startTestServer(t)
	if resp := doReq(t, s, "GET", "/status", "", ""); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("no token: status %d, want 401", resp.StatusCode)
	}
	if resp := doReq(t, s, "GET", "/status", "wrong", ""); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("wrong token: status %d, want 401", resp.StatusCode)
	}
	if resp := doReq(t, s, "GET", "/status", "test-token", ""); resp.StatusCode != http.StatusOK {
		t.Errorf("valid token: status %d, want 200", resp.StatusCode)
	}
}

func TestStatusReflectsSetStatus(t *testing.T) {
	s := startTestServer(t)
	s.SetStatus(Status{Thinking: true, Mode: "coding", TokensUsed: 123, StartedAt: time.Now()})
	resp := doReq(t, s, "GET", "/status", "test-token", "")
	var st Status
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		t.Fatal(err)
	}
	if !st.Thinking || st.Mode != "coding" || st.TokensUsed != 123 {
		t.Errorf("status = %+v", st)
	}
}

func TestMessagesRoundTrip(t *testing.T) {
	s := startTestServer(t)

	go func() {
		req := <-s.Submissions()
		req.Reply <- SubmitResponse{Accepted: true, Note: "ok"}
	}()

	resp := doReq(t, s, "POST", "/messages", "test-token", `{"message":"hello"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var sr SubmitResponse
	if err := json.NewDecoder(resp.Body).Decode(&sr); err != nil {
		t.Fatal(err)
	}
	if !sr.Accepted || sr.Note != "ok" {
		t.Errorf("response = %+v", sr)
	}

	if resp := doReq(t, s, "POST", "/messages", "test-token", `{"message":"  "}`); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("empty message: status %d, want 400", resp.StatusCode)
	}
	if resp := doReq(t, s, "POST", "/messages", "test-token", `not json`); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("bad json: status %d, want 400", resp.StatusCode)
	}
	if resp := doReq(t, s, "GET", "/messages", "test-token", ""); resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET /messages: status %d, want 405", resp.StatusCode)
	}
}

func TestInterrupt(t *testing.T) {
	s := startTestServer(t)
	resp := doReq(t, s, "POST", "/interrupt", "test-token", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	select {
	case <-s.Interrupts():
	case <-time.After(2 * time.Second):
		t.Fatal("interrupt not delivered")
	}
}

func TestHostAllowedRebindingDefense(t *testing.T) {
	loopback := &Server{bindHost: "127.0.0.1"}
	cases := []struct {
		host string
		want bool
	}{
		{"127.0.0.1:7878", true},
		{"localhost:7878", true},
		{"localhost", true},
		{"[::1]:7878", true},
		{"", true},
		{"evil.example.com:7878", false},
		{"192.168.1.5:7878", false},
	}
	for _, c := range cases {
		if got := loopback.hostAllowed(c.host); got != c.want {
			t.Errorf("hostAllowed(%q) = %v, want %v", c.host, got, c.want)
		}
	}
	lan := &Server{bindHost: "0.0.0.0"}
	if !lan.hostAllowed("evil.example.com") {
		t.Error("LAN bind must not constrain Host (token is the gate)")
	}
}

func TestIsLoopbackBind(t *testing.T) {
	for bind, want := range map[string]bool{
		"":          true,
		"localhost": true,
		"127.0.0.1": true,
		"::1":       true,
		"0.0.0.0":   false,
		"10.0.0.2":  false,
	} {
		if got := isLoopbackBind(bind); got != want {
			t.Errorf("isLoopbackBind(%q) = %v, want %v", bind, got, want)
		}
	}
}

func TestNewServerGeneratesToken(t *testing.T) {
	s, err := NewServer(Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Token()) < 16 {
		t.Errorf("generated token too short: %q", s.Token())
	}
	if s.Host() != "127.0.0.1" {
		t.Errorf("default bind host = %q", s.Host())
	}
}
