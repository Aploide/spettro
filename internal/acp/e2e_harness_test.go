package acp

// End-to-end ACP test harness.
//
// The unit tests elsewhere in this package call bridge methods directly. The
// harness here instead runs the bridge exactly as `spettro --acp` does:
// behind the SDK's agent-side connection, driven by a real client-side
// connection over in-memory pipes, against a scripted OpenAI-compatible model
// server. Every line the agent writes is recorded (wireTap), so tests assert
// on the JSON-RPC traffic an editor actually receives (field names, absolute
// paths, sizes) rather than on Go structs that might marshal differently.
//
// Pieces:
//   - scriptedLLM: an httptest server speaking the chat-completions SSE
//     protocol. Each request pops the next llmReply from a queue; a reply can
//     stream reasoning, answer text and native tool calls, and can be held
//     open until a test releases it.
//   - recordingClient: the editor. It records and answers
//     session/request_permission through a per-test function; the
//     session/update notifications are read from the wire tap instead.
//   - acpHarness: wires the two together with a temporary HOME whose
//     ~/.spettro/config.json points at the scripted model.

import (
	"bytes"
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

	"spettro/internal/agent"
	"spettro/internal/config"
	"spettro/internal/provider"
)

// llmCall is one native tool call in a scripted model reply.
type llmCall struct {
	name string
	args string // raw JSON arguments
}

// llmReply is one scripted model response.
type llmReply struct {
	// reasoning is streamed first, as reasoning_content deltas (one per
	// word, so the bridge sees several thought chunks).
	reasoning string
	// content is the answer text.
	content string
	// calls are native tool calls sent after the content.
	calls []llmCall
	// finish overrides the finish_reason; empty means "tool_calls" when
	// calls are present and "stop" otherwise.
	finish string
	// hold, when non-nil, makes the server wait until it is closed (or the
	// request is abandoned) before answering.
	hold chan struct{}
}

// scriptedLLM is a fake OpenAI-compatible chat-completions server.
type scriptedLLM struct {
	t   *testing.T
	srv *httptest.Server

	mu sync.Mutex
	// queues maps a routing marker to the replies for requests whose body
	// contains it; the "" queue serves every other request. Routing lets
	// concurrent sessions each follow their own script.
	queues   map[string][]llmReply
	markers  []string
	requests []string
	// arrived is signalled (non-blocking) on every request, so tests can
	// wait for a turn to reach the model.
	arrived chan struct{}
}

func newScriptedLLM(t *testing.T, replies ...llmReply) *scriptedLLM {
	t.Helper()
	l := &scriptedLLM{t: t, queues: map[string][]llmReply{"": replies}, arrived: make(chan struct{}, 64)}
	l.srv = httptest.NewServer(http.HandlerFunc(l.serve))
	t.Cleanup(l.srv.Close)
	return l
}

// route queues replies for requests whose body contains marker.
func (l *scriptedLLM) route(marker string, replies ...llmReply) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, ok := l.queues[marker]; !ok {
		l.markers = append(l.markers, marker)
	}
	l.queues[marker] = append(l.queues[marker], replies...)
}

// requestBodies returns a copy of every request body received so far.
func (l *scriptedLLM) requestBodies() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.requests...)
}

func (l *scriptedLLM) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	l.mu.Lock()
	l.requests = append(l.requests, string(body))
	key := ""
	for _, m := range l.markers {
		if strings.Contains(string(body), m) {
			key = m
			break
		}
	}
	queue := l.queues[key]
	var reply llmReply
	ok := len(queue) > 0
	if ok {
		reply = queue[0]
		l.queues[key] = queue[1:]
	}
	l.mu.Unlock()
	select {
	case l.arrived <- struct{}{}:
	default:
	}
	if !ok {
		l.t.Errorf("scripted LLM: no reply left for request (route %q)", key)
		http.Error(w, "no scripted reply left", http.StatusInternalServerError)
		return
	}
	if reply.hold != nil {
		select {
		case <-reply.hold:
		case <-r.Context().Done():
			return
		}
	}
	writeSSEReply(w, reply)
}

// writeSSEReply streams reply in the chat.completion.chunk format.
func writeSSEReply(w http.ResponseWriter, reply llmReply) {
	w.Header().Set("Content-Type", "text/event-stream")
	send := func(delta map[string]any, finish string, usage bool) {
		choice := map[string]any{"index": 0, "delta": delta}
		if finish != "" {
			choice["finish_reason"] = finish
		}
		ev := map[string]any{
			"id": "chatcmpl-e2e", "object": "chat.completion.chunk", "model": "fake-model",
			"choices": []any{choice},
		}
		if usage {
			ev["usage"] = map[string]any{"prompt_tokens": 100, "completion_tokens": 20, "total_tokens": 120}
		}
		raw, _ := json.Marshal(ev)
		fmt.Fprintf(w, "data: %s\n\n", raw)
	}
	send(map[string]any{"role": "assistant"}, "", false)
	for _, word := range strings.SplitAfter(reply.reasoning, " ") {
		if word != "" {
			send(map[string]any{"reasoning_content": word}, "", false)
		}
	}
	if reply.content != "" {
		send(map[string]any{"content": reply.content}, "", false)
	}
	if len(reply.calls) > 0 {
		calls := make([]any, len(reply.calls))
		for i, c := range reply.calls {
			calls[i] = map[string]any{
				"index": i, "id": fmt.Sprintf("call_%d", i+1), "type": "function",
				"function": map[string]any{"name": c.name, "arguments": c.args},
			}
		}
		send(map[string]any{"tool_calls": calls}, "", false)
	}
	finish := reply.finish
	if finish == "" {
		finish = "stop"
		if len(reply.calls) > 0 {
			finish = "tool_calls"
		}
	}
	send(map[string]any{}, finish, true)
	fmt.Fprint(w, "data: [DONE]\n\n")
}

// wireTap records every JSON-RPC message one side of the connection writes.
// It sits between the writer and the pipe, so a message is recorded before
// the peer can read it.
type wireTap struct {
	w io.Writer

	mu      sync.Mutex
	partial []byte
	lines   []string
}

func (t *wireTap) Write(p []byte) (int, error) {
	t.mu.Lock()
	t.partial = append(t.partial, p...)
	for {
		i := bytes.IndexByte(t.partial, '\n')
		if i < 0 {
			break
		}
		t.lines = append(t.lines, string(t.partial[:i]))
		t.partial = t.partial[i+1:]
	}
	t.mu.Unlock()
	return t.w.Write(p)
}

// messages returns the recorded messages, each decoded into a generic map.
func (t *wireTap) messages() []map[string]any {
	t.mu.Lock()
	lines := append([]string(nil), t.lines...)
	t.mu.Unlock()
	out := make([]map[string]any, 0, len(lines))
	for _, line := range lines {
		var m map[string]any
		if json.Unmarshal([]byte(line), &m) == nil {
			out = append(out, m)
		}
	}
	return out
}

// rawLines returns the recorded messages as written, for size assertions.
func (t *wireTap) rawLines() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]string(nil), t.lines...)
}

// recordingClient is the editor side of the connection. It supports no
// file system or terminal methods, like an editor that declares neither
// capability.
type recordingClient struct {
	mu          sync.Mutex
	permissions []acpsdk.RequestPermissionRequest
	// onPermission answers a permission request; nil selects "allow-once".
	onPermission func(context.Context, acpsdk.RequestPermissionRequest) (acpsdk.RequestPermissionResponse, error)
}

func (c *recordingClient) SessionUpdate(context.Context, acpsdk.SessionNotification) error {
	return nil
}

func (c *recordingClient) RequestPermission(ctx context.Context, req acpsdk.RequestPermissionRequest) (acpsdk.RequestPermissionResponse, error) {
	c.mu.Lock()
	c.permissions = append(c.permissions, req)
	answer := c.onPermission
	c.mu.Unlock()
	if answer == nil {
		return acpsdk.RequestPermissionResponse{Outcome: acpsdk.NewRequestPermissionOutcomeSelected("allow-once")}, nil
	}
	return answer(ctx, req)
}

func (c *recordingClient) permissionRequests() []acpsdk.RequestPermissionRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]acpsdk.RequestPermissionRequest(nil), c.permissions...)
}

func (c *recordingClient) ReadTextFile(context.Context, acpsdk.ReadTextFileRequest) (acpsdk.ReadTextFileResponse, error) {
	return acpsdk.ReadTextFileResponse{}, acpsdk.NewMethodNotFound(acpsdk.ClientMethodFsReadTextFile)
}

func (c *recordingClient) WriteTextFile(context.Context, acpsdk.WriteTextFileRequest) (acpsdk.WriteTextFileResponse, error) {
	return acpsdk.WriteTextFileResponse{}, acpsdk.NewMethodNotFound(acpsdk.ClientMethodFsWriteTextFile)
}

func (c *recordingClient) CreateTerminal(context.Context, acpsdk.CreateTerminalRequest) (acpsdk.CreateTerminalResponse, error) {
	return acpsdk.CreateTerminalResponse{}, acpsdk.NewMethodNotFound(acpsdk.ClientMethodTerminalCreate)
}

func (c *recordingClient) KillTerminal(context.Context, acpsdk.KillTerminalRequest) (acpsdk.KillTerminalResponse, error) {
	return acpsdk.KillTerminalResponse{}, acpsdk.NewMethodNotFound(acpsdk.ClientMethodTerminalKill)
}

func (c *recordingClient) TerminalOutput(context.Context, acpsdk.TerminalOutputRequest) (acpsdk.TerminalOutputResponse, error) {
	return acpsdk.TerminalOutputResponse{}, acpsdk.NewMethodNotFound(acpsdk.ClientMethodTerminalOutput)
}

func (c *recordingClient) ReleaseTerminal(context.Context, acpsdk.ReleaseTerminalRequest) (acpsdk.ReleaseTerminalResponse, error) {
	return acpsdk.ReleaseTerminalResponse{}, acpsdk.NewMethodNotFound(acpsdk.ClientMethodTerminalRelease)
}

func (c *recordingClient) WaitForTerminalExit(context.Context, acpsdk.WaitForTerminalExitRequest) (acpsdk.WaitForTerminalExitResponse, error) {
	return acpsdk.WaitForTerminalExitResponse{}, acpsdk.NewMethodNotFound(acpsdk.ClientMethodTerminalWaitForExit)
}

// acpHarness is one editor connection to one in-process ACP agent.
type acpHarness struct {
	t      *testing.T
	client *recordingClient
	conn   *acpsdk.ClientSideConnection
	// wire records what the agent sent the client.
	wire *wireTap
	// cwd is the project directory sessions are created in.
	cwd string
}

// newACPHarness starts an agent with the given permission level against llm.
// The agent loads the built-in default manifest (the project has no
// spettro.agents.toml), so tool names, approval policies and agent modes are
// the ones a real installation ships with.
func newACPHarness(t *testing.T, llm *scriptedLLM, permission config.PermissionLevel) *acpHarness {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", "")
	// The scripted model never fails, but a stray 500 must not stall a test
	// in exponential backoff.
	saved := provider.DefaultRetryPolicy
	provider.DefaultRetryPolicy.BaseDelay = time.Millisecond
	provider.DefaultRetryPolicy.MaxDelay = 5 * time.Millisecond
	t.Cleanup(func() { provider.DefaultRetryPolicy = saved })

	if err := os.MkdirAll(filepath.Join(home, ".spettro"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfgJSON := fmt.Sprintf(`{"active_provider":%q,"active_model":"fake-model","permission":%q}`, llm.srv.URL, permission)
	if err := os.WriteFile(filepath.Join(home, ".spettro", "config.json"), []byte(cfgJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	cwd := t.TempDir()
	// Resolve symlinks (macOS /var -> /private/var) so paths the agent
	// reports compare equal to the ones the test builds.
	if resolved, err := filepath.EvalSymlinks(cwd); err == nil {
		cwd = resolved
	}
	agent.ReloadSkills()
	t.Cleanup(agent.ReloadSkills)

	pm := provider.NewManager()
	pm.SetStreamAll(true)
	pm.AddLocalModels([]provider.Model{{Provider: llm.srv.URL, Name: "fake-model", Local: true}})
	cfg, err := config.LoadFull()
	if err != nil {
		t.Fatal(err)
	}

	b := newBridge(Options{
		CWD:       cwd,
		GlobalDir: filepath.Join(home, ".spettro"),
		Cfg:       cfg,
		Providers: pm,
		Manifest:  config.DefaultAgentManifest(),
	})

	// Two one-way pipes: what the agent writes the client reads, and the
	// other way round. The tap sits on the agent's writing end.
	a2cR, a2cW := io.Pipe()
	c2aR, c2aW := io.Pipe()
	wire := &wireTap{w: a2cW}
	b.conn = acpsdk.NewAgentSideConnection(b, wire, c2aR)
	client := &recordingClient{}
	conn := acpsdk.NewClientSideConnection(client, c2aW, a2cR)
	t.Cleanup(func() {
		_ = a2cW.Close()
		_ = c2aW.Close()
	})

	return &acpHarness{t: t, client: client, conn: conn, wire: wire, cwd: cwd}
}

// initialize performs the handshake.
func (h *acpHarness) initialize() acpsdk.InitializeResponse {
	h.t.Helper()
	resp, err := h.conn.Initialize(h.ctx(), acpsdk.InitializeRequest{ProtocolVersion: acpsdk.ProtocolVersionNumber})
	if err != nil {
		h.t.Fatalf("initialize: %v", err)
	}
	return resp
}

// newSession creates a session in the harness project and switches it to
// the given mode (agent id).
func (h *acpHarness) newSession(mode string) acpsdk.SessionId {
	h.t.Helper()
	resp, err := h.conn.NewSession(h.ctx(), acpsdk.NewSessionRequest{Cwd: h.cwd, McpServers: []acpsdk.McpServer{}})
	if err != nil {
		h.t.Fatalf("session/new: %v", err)
	}
	if mode != "" {
		if _, err := h.conn.SetSessionConfigOption(h.ctx(), acpsdk.SetSessionConfigOptionRequest{
			ValueId: &acpsdk.SetSessionConfigOptionValueId{SessionId: resp.SessionId, ConfigId: configIDMode, Value: acpsdk.SessionConfigValueId(mode)},
		}); err != nil {
			h.t.Fatalf("set mode %s: %v", mode, err)
		}
	}
	return resp.SessionId
}

// prompt sends a text prompt and waits for the turn to end.
func (h *acpHarness) prompt(sid acpsdk.SessionId, text string) (acpsdk.PromptResponse, error) {
	return h.conn.Prompt(h.ctx(), acpsdk.PromptRequest{SessionId: sid, Prompt: []acpsdk.ContentBlock{acpsdk.TextBlock(text)}})
}

// ctx is a per-call context with a generous deadline, so a hung turn fails
// the test instead of the whole package run.
func (h *acpHarness) ctx() context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	h.t.Cleanup(cancel)
	return ctx
}

// updates returns the session/update payloads ("update" objects) the agent
// sent for sid, in wire order, optionally filtered to one sessionUpdate kind.
func (h *acpHarness) updates(sid acpsdk.SessionId, kind string) []map[string]any {
	var out []map[string]any
	for _, msg := range h.wire.messages() {
		if msg["method"] != acpsdk.ClientMethodSessionUpdate {
			continue
		}
		params, _ := msg["params"].(map[string]any)
		if params == nil || params["sessionId"] != string(sid) {
			continue
		}
		update, _ := params["update"].(map[string]any)
		if update == nil || (kind != "" && update["sessionUpdate"] != kind) {
			continue
		}
		out = append(out, update)
	}
	return out
}

// waitForUpdate polls the wire until an update of kind arrives for sid.
func (h *acpHarness) waitForUpdate(sid acpsdk.SessionId, kind string) map[string]any {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if got := h.updates(sid, kind); len(got) > 0 {
			return got[len(got)-1]
		}
		time.Sleep(10 * time.Millisecond)
	}
	h.t.Fatalf("no %s update for session %s", kind, sid)
	return nil
}

// toolCallsByID folds the tool_call and tool_call_update notifications for
// sid into the final state of each call, the way a client renders them:
// scalar fields replace, content is replaced by an update that carries it,
// and the first tool_call's fields are kept for the rest.
func (h *acpHarness) toolCallsByID(sid acpsdk.SessionId) (map[string]map[string]any, []string) {
	calls := map[string]map[string]any{}
	var order []string
	for _, u := range h.updates(sid, "") {
		kind := u["sessionUpdate"]
		if kind != "tool_call" && kind != "tool_call_update" {
			continue
		}
		id, _ := u["toolCallId"].(string)
		cur, ok := calls[id]
		if !ok {
			cur = map[string]any{}
			calls[id] = cur
			order = append(order, id)
		}
		for k, v := range u {
			if k == "sessionUpdate" {
				continue
			}
			cur[k] = v
		}
	}
	return calls, order
}
