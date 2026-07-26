package spettromobile

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// testHandler is the Go stand-in for the Swift EngineHandler: it records
// everything the engine emits and lets a test block until a specific JSON-RPC
// response id arrives.
type testHandler struct {
	mu    sync.Mutex
	lines []json.RawMessage
	logs  []string
	code  int64

	exited  chan struct{}
	arrived chan struct{} // signalled (non-blocking) on every OnLine
}

func newTestHandler() *testHandler {
	return &testHandler{
		exited:  make(chan struct{}),
		arrived: make(chan struct{}, 64),
	}
}

func (h *testHandler) OnLine(line []byte) {
	h.mu.Lock()
	h.lines = append(h.lines, json.RawMessage(append([]byte(nil), line...)))
	h.mu.Unlock()
	select {
	case h.arrived <- struct{}{}:
	default:
	}
}

func (h *testHandler) OnLog(line string) {
	h.mu.Lock()
	h.logs = append(h.logs, line)
	h.mu.Unlock()
}

func (h *testHandler) OnExit(code int64) {
	h.mu.Lock()
	h.code = code
	h.mu.Unlock()
	close(h.exited)
}

func (h *testHandler) exitCode() int64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.code
}

func (h *testHandler) logText() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return strings.Join(h.logs, "\n")
}

// awaitResponse blocks until a JSON-RPC message carrying the given id shows
// up, then returns its `result` object. Notifications and other ids are
// ignored, which is the point: the transport is a stream, not a call.
func (h *testHandler) awaitResponse(t *testing.T, id int) map[string]any {
	t.Helper()
	deadline := time.After(30 * time.Second)
	for {
		h.mu.Lock()
		lines := append([]json.RawMessage(nil), h.lines...)
		h.mu.Unlock()

		for _, raw := range lines {
			var msg struct {
				ID     *json.Number    `json:"id"`
				Result json.RawMessage `json:"result"`
				Error  json.RawMessage `json:"error"`
				Method string          `json:"method"`
			}
			if err := json.Unmarshal(raw, &msg); err != nil {
				t.Fatalf("engine emitted a line that is not JSON: %s (%v)", raw, err)
			}
			if msg.ID == nil || msg.Method != "" || msg.ID.String() != strconv.Itoa(id) {
				continue
			}
			if len(msg.Error) > 0 {
				t.Fatalf("request id %d failed: %s", id, msg.Error)
			}
			var result map[string]any
			if err := json.Unmarshal(msg.Result, &result); err != nil {
				t.Fatalf("request id %d: result is not an object: %s", id, msg.Result)
			}
			return result
		}

		select {
		case <-h.arrived:
		case <-h.exited:
			t.Fatalf("engine exited before answering id %d; logs:\n%s", id, h.logText())
		case <-deadline:
			t.Fatalf("timed out waiting for response id %d; logs:\n%s", id, h.logText())
		}
	}
}

// newProject gives the engine a private $HOME (so global config, keys and the
// model cache land in a throwaway directory) and a project directory. The
// catalog cache is pre-seeded so the bootstrap never has to hit the network to
// populate the provider list.
func newProject(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)

	spettroDir := filepath.Join(home, ".spettro")
	if err := os.MkdirAll(spettroDir, 0o755); err != nil {
		t.Fatal(err)
	}
	catalog := `{"version":1,"updated":"2026-01-01","providers":{"anthropic":` +
		`{"name":"Anthropic","api":"anthropic","base_url":"https://api.anthropic.com",` +
		`"env":"ANTHROPIC_API_KEY","models":{"claude-test":{"name":"Claude Test","tool_call":true,"context":200000}}}}}`
	if err := os.WriteFile(filepath.Join(spettroDir, "catalog.json"), []byte(catalog), 0o644); err != nil {
		t.Fatal(err)
	}

	project := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	return project
}

func send(t *testing.T, e *Engine, id int, method string, params any) {
	t.Helper()
	body := map[string]any{"jsonrpc": "2.0", "id": id, "method": method}
	if params != nil {
		body["params"] = params
	}
	line, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Send(line); err != nil {
		t.Fatalf("Send(%s): %v", method, err)
	}
}

// handshake drives initialize + session/new and returns the new session id.
func handshake(t *testing.T, e *Engine, h *testHandler, cwd string) string {
	t.Helper()

	send(t, e, 1, "initialize", map[string]any{
		"protocolVersion":    1,
		"clientCapabilities": map[string]any{},
	})
	initResult := h.awaitResponse(t, 1)
	if _, ok := initResult["protocolVersion"]; !ok {
		t.Fatalf("initialize result has no protocolVersion: %#v", initResult)
	}
	if _, ok := initResult["agentCapabilities"]; !ok {
		t.Fatalf("initialize result has no agentCapabilities: %#v", initResult)
	}

	send(t, e, 2, "session/new", map[string]any{
		"cwd":        cwd,
		"mcpServers": []any{},
	})
	newResult := h.awaitResponse(t, 2)
	sid, _ := newResult["sessionId"].(string)
	if sid == "" {
		t.Fatalf("session/new returned no sessionId: %#v", newResult)
	}
	return sid
}

func stopAndWait(t *testing.T, e *Engine, h *testHandler) {
	t.Helper()
	e.Stop()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := e.waitForExit(ctx); err != nil {
		t.Fatalf("engine did not exit after Stop: %v; logs:\n%s", err, h.logText())
	}
	select {
	case <-h.exited:
	case <-time.After(5 * time.Second):
		t.Fatal("OnExit was never delivered to the handler")
	}
	if code := h.exitCode(); code != exitClean {
		t.Fatalf("Stop reported exit code %d, want %d; logs:\n%s", code, exitClean, h.logText())
	}
}

// TestEngineHandshake is the acceptance test for the bridge: a real ACP
// handshake driven entirely through the exported Start/Send/Stop surface.
func TestEngineHandshake(t *testing.T) {
	cwd := newProject(t)

	h := newTestHandler()
	e, err := Start(cwd, h)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	sid := handshake(t, e, h, cwd)
	t.Logf("session id: %s", sid)

	stopAndWait(t, e, h)

	// Nothing may be accepted after Stop.
	if err := e.Send([]byte(`{"jsonrpc":"2.0","id":3,"method":"initialize"}`)); err == nil {
		t.Fatal("Send after Stop should fail")
	}
	// Stop is idempotent.
	e.Stop()
	e.Stop()
}

// TestEngineRestart covers the app's crash/teardown path: the engine is
// stopped and a fresh one started repeatedly inside one process, and the
// goroutine population must reach a steady state rather than growing per
// cycle.
func TestEngineRestart(t *testing.T) {
	cwd := newProject(t)

	const cycles = 3
	counts := make([]int, cycles)

	for i := range cycles {
		h := newTestHandler()
		e, err := Start(cwd, h)
		if err != nil {
			t.Fatalf("cycle %d: Start: %v", i, err)
		}
		if sid := handshake(t, e, h, cwd); sid == "" {
			t.Fatalf("cycle %d: empty session id", i)
		}
		stopAndWait(t, e, h)
		counts[i] = settledGoroutines()
		t.Logf("cycle %d: %d goroutines after Stop", i, counts[i])
	}

	// Cycle 0 pays for the process-lifetime goroutines the bootstrap starts
	// once (the model-catalog refresher, HTTP transport idle conns). From
	// there the count must not grow: a per-Start leak would show up as a
	// monotonic climb.
	if counts[cycles-1] > counts[0] {
		t.Fatalf("goroutines leaked across Start/Stop cycles: %v", counts)
	}
}

// TestStartRejectsBadInput checks the synchronous guardrails Swift relies on.
func TestStartRejectsBadInput(t *testing.T) {
	if _, err := Start("", newTestHandler()); err == nil {
		t.Fatal("Start with an empty cwd should fail")
	}
	if _, err := Start(t.TempDir(), nil); err == nil {
		t.Fatal("Start with a nil handler should fail")
	}
	missing := filepath.Join(t.TempDir(), "nope")
	if _, err := Start(missing, newTestHandler()); err == nil {
		t.Fatal("Start with a missing cwd should fail")
	}
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Start(file, newTestHandler()); err == nil {
		t.Fatal("Start with a file as cwd should fail")
	}
}

// TestSendFraming pins the newline contract: the bridge owns framing, so the
// caller must hand over exactly one message with no newline in it.
func TestSendFraming(t *testing.T) {
	cwd := newProject(t)
	h := newTestHandler()
	e, err := Start(cwd, h)
	if err != nil {
		t.Fatal(err)
	}
	defer stopAndWait(t, e, h)

	if err := e.Send(nil); err == nil {
		t.Fatal("Send(nil) should fail")
	}
	if err := e.Send([]byte("   ")); err == nil {
		t.Fatal("Send of blank bytes should fail")
	}
	if err := e.Send([]byte("{\"a\":1}\n{\"b\":2}")); err == nil {
		t.Fatal("Send of two framed messages should fail")
	}
}

// settledGoroutines waits for the goroutine count to stop moving, so shutdown
// stragglers (the SDK's notification-drain watchdog, http idle-conn reapers)
// are not mistaken for leaks.
func settledGoroutines() int {
	prev := -1
	stable := 0
	for range 200 {
		runtime.Gosched()
		n := runtime.NumGoroutine()
		if n == prev {
			stable++
			if stable >= 5 {
				return n
			}
		} else {
			stable = 0
			prev = n
		}
		time.Sleep(50 * time.Millisecond)
	}
	return runtime.NumGoroutine()
}
