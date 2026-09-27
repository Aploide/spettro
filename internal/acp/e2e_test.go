package acp

// End-to-end ACP scenarios, run through the harness in e2e_harness_test.go:
// a real client connection, the bridge behind the SDK's agent connection,
// the default manifest, and a scripted model. Assertions read the JSON-RPC
// traffic as an editor receives it.

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	acpsdk "github.com/coder/acp-go-sdk"

	"spettro/internal/config"
	"spettro/internal/skills"
)

// Handshake, session creation, errors for bad requests, session/close, and
// session/load of a closed session replaying its conversation.
func TestACPEndToEnd_HandshakeAndSessionLifecycle(t *testing.T) {
	llm := newScriptedLLM(t,
		llmReply{content: "Hi there."},
		llmReply{content: "Welcome back."},
	)
	h := newACPHarness(t, llm, config.PermissionYOLO)

	init := h.initialize()
	if init.ProtocolVersion != acpsdk.ProtocolVersionNumber {
		t.Errorf("protocolVersion = %d", init.ProtocolVersion)
	}
	caps := init.AgentCapabilities
	if !caps.LoadSession || caps.SessionCapabilities.List == nil || caps.SessionCapabilities.Resume == nil || caps.SessionCapabilities.Close == nil {
		t.Errorf("session capabilities: %s", jsonString(caps))
	}
	if !caps.PromptCapabilities.Image || !caps.PromptCapabilities.EmbeddedContext {
		t.Errorf("prompt capabilities: %s", jsonString(caps.PromptCapabilities))
	}

	// session/new returns the toolbar selectors.
	resp, err := h.conn.NewSession(h.ctx(), acpsdk.NewSessionRequest{Cwd: h.cwd, McpServers: []acpsdk.McpServer{}})
	if err != nil {
		t.Fatalf("session/new: %v", err)
	}
	var ids []string
	for _, opt := range resp.ConfigOptions {
		switch {
		case opt.Select != nil:
			ids = append(ids, string(opt.Select.Id))
		case opt.Boolean != nil:
			ids = append(ids, string(opt.Boolean.Id))
		}
	}
	if strings.Join(ids, ",") != "mode,model,permission,thinking,ultra" {
		t.Errorf("config options = %v", ids)
	}
	sid := resp.SessionId
	cmds := h.waitForUpdate(sid, "available_commands_update")
	if !strings.Contains(jsonString(cmds), `"name":"skills"`) {
		t.Errorf("commands not announced: %s", jsonString(cmds))
	}

	// Bad requests fail as invalid params, not as internal errors.
	_, err = h.conn.NewSession(h.ctx(), acpsdk.NewSessionRequest{Cwd: "relative/dir", McpServers: []acpsdk.McpServer{}})
	assertRequestError(t, err, -32602, "relative cwd")
	_, err = h.prompt("no-such-session", "hello")
	assertRequestError(t, err, -32602, "unknown session")
	_, err = h.conn.SetSessionConfigOption(h.ctx(), acpsdk.SetSessionConfigOptionRequest{
		ValueId: &acpsdk.SetSessionConfigOptionValueId{SessionId: sid, ConfigId: configIDMode, Value: "no-such-mode"},
	})
	assertRequestError(t, err, -32602, "unknown mode")

	if r, err := h.prompt(sid, "hello"); err != nil || r.StopReason != acpsdk.StopReasonEndTurn {
		t.Fatalf("first turn: %v %s", err, jsonString(r))
	}

	// session/close drops the session from the connection...
	if _, err := h.conn.CloseSession(h.ctx(), acpsdk.CloseSessionRequest{SessionId: sid}); err != nil {
		t.Fatalf("session/close: %v", err)
	}
	_, err = h.prompt(sid, "still there?")
	assertRequestError(t, err, -32602, "prompt after close")
	_, err = h.conn.CloseSession(h.ctx(), acpsdk.CloseSessionRequest{SessionId: sid})
	assertRequestError(t, err, -32602, "second close")

	// ...but not from disk: session/load brings it back and replays the
	// conversation before answering.
	before := len(h.updates(sid, ""))
	if _, err := h.conn.LoadSession(h.ctx(), acpsdk.LoadSessionRequest{SessionId: sid, Cwd: h.cwd, McpServers: []acpsdk.McpServer{}}); err != nil {
		t.Fatalf("session/load: %v", err)
	}
	var replay []string
	for _, u := range h.updates(sid, "")[before:] {
		if c, ok := u["content"].(map[string]any); ok {
			replay = append(replay, u["sessionUpdate"].(string)+": "+c["text"].(string))
		}
	}
	if strings.Join(replay, " | ") != "user_message_chunk: hello | agent_message_chunk: Hi there." {
		t.Errorf("replay = %q", replay)
	}
	if r, err := h.prompt(sid, "again"); err != nil || r.StopReason != acpsdk.StopReasonEndTurn {
		t.Fatalf("turn after load: %v %s", err, jsonString(r))
	}
	bodies := llm.requestBodies()
	if last := bodies[len(bodies)-1]; !strings.Contains(last, "Hi there.") {
		t.Errorf("the turn after load lacks the prior conversation:\n%s", last)
	}
}

// assertRequestError checks err is a JSON-RPC error with the given code.
func assertRequestError(t *testing.T, err error, code int, what string) {
	t.Helper()
	var reqErr *acpsdk.RequestError
	if !errors.As(err, &reqErr) {
		t.Errorf("%s: want a JSON-RPC error %d, got %v", what, code, err)
		return
	}
	if reqErr.Code != code {
		t.Errorf("%s: error code %d (%s), want %d", what, reqErr.Code, reqErr.Message, code)
	}
}

// One turn with reasoning, parallel reads (one under a retired name), an
// edit, a new file, and a final answer: thoughts stream as
// agent_thought_chunk, the model's prose between tool calls and its answer
// each arrive once as agent messages, every call gets a kind, a readable
// title, absolute locations and a completion with content, and the writes
// carry real diffs.
func TestACPEndToEnd_ToolCallsReasoningAndDiffs(t *testing.T) {
	llm := newScriptedLLM(t,
		llmReply{reasoning: "look at the file first", content: "Let me look.", calls: []llmCall{
			{"file-read", `{"path":"a.txt"}`},
			{"ls", `{"path":"."}`},
		}},
		llmReply{calls: []llmCall{{"file-edit", `{"path":"a.txt","old_string":"hello","new_string":"bye"}`}}},
		llmReply{calls: []llmCall{{"file-write", `{"path":"sub/new.txt","content":"fresh\n"}`}}},
		llmReply{content: "All done."},
	)
	h := newACPHarness(t, llm, config.PermissionYOLO)
	writeFile(t, filepath.Join(h.cwd, "a.txt"), "hello world\n")
	h.initialize()
	sid := h.newSession("coding")

	resp, err := h.prompt(sid, "tidy a.txt")
	if err != nil {
		t.Fatalf("prompt: %v", err)
	}
	if resp.StopReason != acpsdk.StopReasonEndTurn || resp.Usage == nil || resp.Usage.TotalTokens == 0 {
		t.Errorf("response = %s", jsonString(resp))
	}

	var thought strings.Builder
	for _, u := range h.updates(sid, "agent_thought_chunk") {
		thought.WriteString(u["content"].(map[string]any)["text"].(string))
	}
	if thought.String() != "look at the file first" {
		t.Errorf("thoughts = %q", thought.String())
	}
	// The prose of a tool-calling step is shown once, then the answer.
	if got := answersOf(h, sid); got != "Let me look.\n\n|All done." {
		t.Errorf("agent messages = %q", got)
	}

	calls, order := h.toolCallsByID(sid)
	if len(order) != 4 {
		t.Fatalf("want 4 tool calls, got %d: %s", len(order), jsonString(calls))
	}
	byTitle := map[string]map[string]any{}
	for _, id := range order {
		c := calls[id]
		if c["status"] != "completed" {
			t.Errorf("%s ended %v", c["title"], c["status"])
		}
		byTitle[c["title"].(string)] = c
	}
	abs := func(rel string) string { return filepath.Join(h.cwd, rel) }
	for title, want := range map[string]struct{ kind, path string }{
		"Read a.txt":        {"read", abs("a.txt")},
		"List .":            {"search", h.cwd},
		"Edit a.txt":        {"edit", abs("a.txt")},
		"Write sub/new.txt": {"edit", abs("sub/new.txt")},
	} {
		c, ok := byTitle[title]
		if !ok {
			t.Errorf("no tool call titled %q (have %v)", title, keys(byTitle))
			continue
		}
		if c["kind"] != want.kind {
			t.Errorf("%s: kind %v, want %s", title, c["kind"], want.kind)
		}
		locs, _ := c["locations"].([]any)
		if len(locs) != 1 || locs[0].(map[string]any)["path"] != want.path {
			t.Errorf("%s: locations %s, want %s", title, jsonString(locs), want.path)
		}
	}

	// The edit's completion carries the diff first, then the text result.
	edit := byTitle["Edit a.txt"]["content"].([]any)
	diff := edit[0].(map[string]any)
	if diff["type"] != "diff" || diff["path"] != abs("a.txt") || diff["oldText"] != "hello world\n" || diff["newText"] != "bye world\n" {
		t.Errorf("edit diff = %s", jsonString(diff))
	}
	// A created file's diff has no oldText.
	created := byTitle["Write sub/new.txt"]["content"].([]any)[0].(map[string]any)
	if _, has := created["oldText"]; has || created["newText"] != "fresh\n" {
		t.Errorf("created-file diff = %s", jsonString(created))
	}
}

// A call with megabytes of arguments, or changing a very large file, never
// produces a notification an editor would choke on.
func TestACPEndToEnd_HugeCallsAreBounded(t *testing.T) {
	huge := strings.Repeat("0123456789abcdef", 1<<16) // 1 MiB
	bigArgs, _ := json.Marshal(map[string]string{"path": "big.txt", "content": huge})
	longCmd, _ := json.Marshal(map[string]string{"command": "echo " + strings.Repeat("x", 200<<10)})
	llm := newScriptedLLM(t,
		llmReply{calls: []llmCall{{"file-write", string(bigArgs)}}},
		llmReply{calls: []llmCall{{"bash", string(longCmd)}}},
		llmReply{content: "done"},
	)
	h := newACPHarness(t, llm, config.PermissionYOLO)
	h.initialize()
	sid := h.newSession("coding")
	if _, err := h.prompt(sid, "write a big file"); err != nil {
		t.Fatalf("prompt: %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(h.cwd, "big.txt")); len(got) != len(huge) {
		t.Fatalf("big.txt has %d bytes, want %d", len(got), len(huge))
	}
	const limit = 64 << 10
	for _, line := range h.wire.rawLines() {
		if strings.Contains(line, `"session/update"`) && len(line) > limit {
			t.Errorf("a %d-byte session/update went out (limit %d): %.300s", len(line), limit, line)
		}
	}
	calls, order := h.toolCallsByID(sid)
	if len(order) != 2 {
		t.Fatalf("want 2 calls, got %s", jsonString(calls))
	}
	write := calls[order[0]]
	if !strings.Contains(jsonString(write["content"]), "diff not shown") {
		t.Errorf("an oversized change must be named instead of diffed: %.500s", jsonString(write["content"]))
	}
	if title := calls[order[1]]["title"].(string); len([]rune(title)) > maxTitleRunes {
		t.Errorf("title has %d runes", len([]rune(title)))
	}
}

// Under ask-first, a shell command and a file edit ask the editor through
// session/request_permission on the card it is already rendering; a denial
// fails the call without running it, an approval runs it.
func TestACPEndToEnd_AskFirstPermissions(t *testing.T) {
	llm := newScriptedLLM(t,
		llmReply{calls: []llmCall{{"bash", `{"command":"touch made.txt"}`}}},
		llmReply{calls: []llmCall{{"file-read", `{"path":"a.txt"}`}}},
		llmReply{calls: []llmCall{{"file-edit", `{"path":"a.txt","old_string":"hello","new_string":"bye"}`}}},
		llmReply{content: "ok"},
	)
	h := newACPHarness(t, llm, config.PermissionAskFirst)
	writeFile(t, filepath.Join(h.cwd, "a.txt"), "hello world\n")
	h.client.onPermission = func(_ context.Context, req acpsdk.RequestPermissionRequest) (acpsdk.RequestPermissionResponse, error) {
		// Deny the command, allow the edit.
		choice := acpsdk.PermissionOptionId(permAllowOnce)
		if strings.Contains(jsonString(req.ToolCall.Content), "made.txt") {
			choice = permDeny
		}
		return acpsdk.RequestPermissionResponse{Outcome: acpsdk.NewRequestPermissionOutcomeSelected(choice)}, nil
	}
	h.initialize()
	sid := h.newSession("coding")
	if _, err := h.prompt(sid, "go"); err != nil {
		t.Fatalf("prompt: %v", err)
	}

	reqs := h.client.permissionRequests()
	if len(reqs) != 2 {
		t.Fatalf("want 2 permission requests, got %d: %s", len(reqs), jsonString(reqs))
	}
	calls, order := h.toolCallsByID(sid)
	idOf := func(title string) acpsdk.ToolCallId {
		for _, id := range order {
			if calls[id]["title"] == title {
				return acpsdk.ToolCallId(id)
			}
		}
		t.Fatalf("no card titled %q", title)
		return ""
	}

	bashReq := reqs[0]
	if bashReq.ToolCall.ToolCallId != idOf("Run touch made.txt") {
		t.Errorf("command approval is not on the command's card: %s", jsonString(bashReq.ToolCall))
	}
	if got := optionIDs(bashReq.Options); got != "allow-once,allow-always,deny" {
		t.Errorf("command options = %s", got)
	}
	if bashReq.ToolCall.Title != nil {
		t.Errorf("an approval on an open card must not retitle it: %q", *bashReq.ToolCall.Title)
	}
	if _, err := os.Stat(filepath.Join(h.cwd, "made.txt")); !os.IsNotExist(err) {
		t.Error("a denied command ran")
	}
	if calls[string(idOf("Run touch made.txt"))]["status"] != "failed" {
		t.Errorf("a denied command must fail its card: %s", jsonString(calls[string(idOf("Run touch made.txt"))]))
	}

	editReq := reqs[1]
	if editReq.ToolCall.ToolCallId != idOf("Edit a.txt") {
		t.Errorf("edit approval is not on the edit's card: %s", jsonString(editReq.ToolCall))
	}
	// The runtime never remembers a write approval, so none is offered.
	if got := optionIDs(editReq.Options); got != "allow-once,deny" {
		t.Errorf("edit options = %s", got)
	}
	if len(editReq.ToolCall.Content) == 0 || editReq.ToolCall.Content[0].Diff == nil ||
		editReq.ToolCall.Content[0].Diff.NewText != "bye world\n" || editReq.ToolCall.Content[0].Diff.Path != filepath.Join(h.cwd, "a.txt") {
		t.Errorf("edit approval must show the diff: %s", jsonString(editReq.ToolCall.Content))
	}
	if got, _ := os.ReadFile(filepath.Join(h.cwd, "a.txt")); string(got) != "bye world\n" {
		t.Errorf("approved edit not applied: %q", got)
	}
	// The edit's card went pending -> in_progress -> completed.
	var statuses []string
	for _, u := range h.updates(sid, "tool_call_update") {
		if u["toolCallId"] == string(idOf("Edit a.txt")) {
			if st, ok := u["status"].(string); ok {
				statuses = append(statuses, st)
			}
		}
	}
	if strings.Join(statuses, ",") != "in_progress,completed" {
		t.Errorf("edit card statuses after approval = %v", statuses)
	}
}

func optionIDs(opts []acpsdk.PermissionOption) string {
	ids := make([]string, len(opts))
	for i, o := range opts {
		ids[i] = string(o.OptionId)
	}
	return strings.Join(ids, ",")
}

// session/cancel ends the turn with the "cancelled" stop reason, whether the
// agent is waiting on the model or on a permission prompt.
func TestACPEndToEnd_Cancellation(t *testing.T) {
	t.Run("while the model is thinking", func(t *testing.T) {
		hold := make(chan struct{})
		t.Cleanup(func() { close(hold) })
		llm := newScriptedLLM(t, llmReply{content: "never", hold: hold})
		h := newACPHarness(t, llm, config.PermissionYOLO)
		h.initialize()
		sid := h.newSession("coding")
		done := make(chan acpsdk.PromptResponse, 1)
		go func() {
			r, err := h.prompt(sid, "think hard")
			if err != nil {
				t.Errorf("prompt: %v", err)
			}
			done <- r
		}()
		<-llm.arrived
		if err := h.conn.Cancel(h.ctx(), acpsdk.CancelNotification{SessionId: sid}); err != nil {
			t.Fatal(err)
		}
		select {
		case r := <-done:
			if r.StopReason != acpsdk.StopReasonCancelled {
				t.Errorf("stop reason = %s", r.StopReason)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("cancelled turn did not end")
		}
	})

	t.Run("by session/close", func(t *testing.T) {
		hold := make(chan struct{})
		t.Cleanup(func() { close(hold) })
		llm := newScriptedLLM(t, llmReply{content: "never", hold: hold})
		h := newACPHarness(t, llm, config.PermissionYOLO)
		h.initialize()
		sid := h.newSession("coding")
		done := make(chan acpsdk.PromptResponse, 1)
		go func() {
			r, err := h.prompt(sid, "long task")
			if err != nil {
				t.Errorf("prompt: %v", err)
			}
			done <- r
		}()
		<-llm.arrived
		if _, err := h.conn.CloseSession(h.ctx(), acpsdk.CloseSessionRequest{SessionId: sid}); err != nil {
			t.Fatal(err)
		}
		select {
		case r := <-done:
			if r.StopReason != acpsdk.StopReasonCancelled {
				t.Errorf("stop reason = %s", r.StopReason)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("the closed session's turn did not end")
		}
	})

	t.Run("while a permission prompt is open", func(t *testing.T) {
		llm := newScriptedLLM(t, llmReply{calls: []llmCall{{"bash", `{"command":"touch x"}`}}})
		h := newACPHarness(t, llm, config.PermissionAskFirst)
		asked := make(chan struct{}, 1)
		h.client.onPermission = func(ctx context.Context, _ acpsdk.RequestPermissionRequest) (acpsdk.RequestPermissionResponse, error) {
			asked <- struct{}{}
			<-ctx.Done() // the agent withdraws the request ($/cancel_request)
			return acpsdk.RequestPermissionResponse{Outcome: acpsdk.NewRequestPermissionOutcomeCancelled()}, nil
		}
		h.initialize()
		sid := h.newSession("coding")
		done := make(chan acpsdk.PromptResponse, 1)
		go func() {
			r, err := h.prompt(sid, "make x")
			if err != nil {
				t.Errorf("prompt: %v", err)
			}
			done <- r
		}()
		select {
		case <-asked:
		case <-time.After(10 * time.Second):
			t.Fatal("no permission request")
		}
		if err := h.conn.Cancel(h.ctx(), acpsdk.CancelNotification{SessionId: sid}); err != nil {
			t.Fatal(err)
		}
		select {
		case r := <-done:
			if r.StopReason != acpsdk.StopReasonCancelled {
				t.Errorf("stop reason = %s", r.StopReason)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("cancelled turn did not end")
		}
		if _, err := os.Stat(filepath.Join(h.cwd, "x")); !os.IsNotExist(err) {
			t.Error("the command ran although the turn was cancelled at its prompt")
		}
	})
}

// A reply the provider's content filter stopped ends the turn with the
// "refusal" stop reason instead of a JSON-RPC error.
func TestACPEndToEnd_RefusalStopReason(t *testing.T) {
	llm := newScriptedLLM(t, llmReply{finish: "content_filter"})
	h := newACPHarness(t, llm, config.PermissionYOLO)
	h.initialize()
	sid := h.newSession("coding")
	r, err := h.prompt(sid, "something the filter stops")
	if err != nil {
		t.Fatalf("prompt: %v", err)
	}
	if r.StopReason != acpsdk.StopReasonRefusal {
		t.Errorf("stop reason = %s", r.StopReason)
	}
}

// A /goal iteration whose last step writes its conclusion and calls
// goal-complete with no summary returns that same prose as the run's content.
// The prose already reached the editor as narration, so the goal loop must
// not send it a second time.
func TestACPEndToEnd_GoalConclusionIsShownOnce(t *testing.T) {
	const conclusion = "Objective met: the README now has a usage section."
	llm := newScriptedLLM(t,
		llmReply{content: conclusion, calls: []llmCall{{"goal-complete", `{"summary":""}`}}},
	)
	h := newACPHarness(t, llm, config.PermissionYOLO)
	h.initialize()
	sid := h.newSession("coding")
	if _, err := h.prompt(sid, "/goal add a usage section to the README"); err != nil {
		t.Fatalf("prompt: %v", err)
	}
	messages := answersOf(h, sid)
	var shown []string
	for _, m := range strings.Split(messages, "|") {
		if strings.TrimSpace(m) == conclusion {
			shown = append(shown, m)
		}
	}
	if len(shown) != 1 {
		t.Errorf("the conclusion was sent as its own message %d times, want once; messages: %q", len(shown), messages)
	}
}

// todo-write, under its own name and a retired task-* name, keeps the
// editor's plan equal to the session task list: replace, merge, delete.
func TestACPEndToEnd_PlanFollowsTodoWrite(t *testing.T) {
	llm := newScriptedLLM(t,
		llmReply{calls: []llmCall{{"todo-write", `{"todos":[
			{"content":"design","status":"in_progress","priority":"high"},
			{"content":"build","dependencies":["task-1"]},
			{"content":"docs","priority":"low"}]}`}}},
		llmReply{calls: []llmCall{{"todo-write", `{"merge":true,"todos":[{"id":"task-1","status":"completed"}]}`}}},
		llmReply{calls: []llmCall{{"todo-write", `{"delete":["task-3"]}`}}},
		llmReply{calls: []llmCall{{"task-update", `{"id":"task-2","status":"in_progress"}`}}},
		llmReply{content: "planned"},
	)
	h := newACPHarness(t, llm, config.PermissionYOLO)
	h.initialize()
	sid := h.newSession("coding")
	if _, err := h.prompt(sid, "plan it"); err != nil {
		t.Fatalf("prompt: %v", err)
	}
	var plans []string
	for _, u := range h.updates(sid, "plan") {
		var entries []string
		for _, e := range u["entries"].([]any) {
			m := e.(map[string]any)
			entries = append(entries, m["content"].(string)+"="+m["status"].(string)+"/"+m["priority"].(string))
		}
		plans = append(plans, strings.Join(entries, ", "))
	}
	want := []string{
		// dependency order: build follows the task it waits for
		"design=in_progress/high, build (blocked)=pending/medium, docs=pending/low",
		"design=completed/high, build=pending/medium, docs=pending/low",
		"design=completed/high, build=pending/medium",
		"design=completed/high, build=in_progress/medium",
	}
	if strings.Join(plans, "\n") != strings.Join(want, "\n") {
		t.Errorf("plans:\n%s\nwant:\n%s", strings.Join(plans, "\n"), strings.Join(want, "\n"))
	}
}

// Skills are advertised as commands and run as /name; the transcript keeps
// what the user typed.
func TestACPEndToEnd_SkillsAsCommands(t *testing.T) {
	llm := newScriptedLLM(t, llmReply{content: "Hello, Ada!"})
	h := newACPHarness(t, llm, config.PermissionYOLO)
	dir := filepath.Join(h.cwd, ".spettro", "skills", "greet")
	writeFile(t, filepath.Join(dir, skills.SkillFilename),
		"---\nname: greet\ndescription: Greets a person\nargument-hint: <name>\n---\nSay hello to $ARGUMENTS.\n")
	h.initialize()
	sid := h.newSession("coding")
	cmds := h.waitForUpdate(sid, "available_commands_update")
	advertised := false
	for _, c := range cmds["availableCommands"].([]any) {
		cmd := c.(map[string]any)
		if cmd["name"] != "greet" {
			continue
		}
		input, _ := cmd["input"].(map[string]any)
		advertised = input != nil && input["hint"] == "<name>" && cmd["description"] == "skill: Greets a person"
	}
	if !advertised {
		t.Errorf("greet not advertised with its hint: %s", jsonString(cmds))
	}
	if r, err := h.prompt(sid, "/greet Ada"); err != nil || r.StopReason != acpsdk.StopReasonEndTurn {
		t.Fatalf("/greet: %v %s", err, jsonString(r))
	}
	bodies := llm.requestBodies()
	if !strings.Contains(bodies[len(bodies)-1], "Say hello to Ada.") {
		t.Errorf("the skill's instructions did not reach the model:\n%s", bodies[len(bodies)-1])
	}
}

// Two sessions in one directory run at the same time and each keeps the
// environment snapshot it started with: a file created after session A's
// first turn shows in session B's snapshot and never in A's.
func TestACPEndToEnd_ConcurrentSessionsKeepTheirOwnContext(t *testing.T) {
	holdA := make(chan struct{})
	llm := newScriptedLLM(t)
	// Routing picks the first registered marker a request contains, so the
	// second-turn markers go first (a second-turn request also carries the
	// first turn's text).
	llm.route("alpha-2", llmReply{content: "A2", hold: holdA})
	llm.route("beta-2", llmReply{content: "B2"})
	llm.route("alpha-1", llmReply{content: "A1"})
	llm.route("beta-1", llmReply{content: "B1"})
	h := newACPHarness(t, llm, config.PermissionYOLO)
	h.initialize()
	a := h.newSession("coding")
	b := h.newSession("coding")

	if _, err := h.prompt(a, "alpha-1"); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(h.cwd, "LATE_FILE.txt"), "x")
	if _, err := h.prompt(b, "beta-1"); err != nil {
		t.Fatal(err)
	}

	// A's second turn is held at the model while B's runs to completion.
	doneA := make(chan error, 1)
	go func() {
		_, err := h.prompt(a, "alpha-2")
		doneA <- err
	}()
	waitForRequestContaining(t, llm, "alpha-2")
	if r, err := h.prompt(b, "beta-2"); err != nil || r.StopReason != acpsdk.StopReasonEndTurn {
		t.Fatalf("B's turn while A runs: %v %s", err, jsonString(r))
	}
	close(holdA)
	if err := <-doneA; err != nil {
		t.Fatal(err)
	}

	for _, body := range llm.requestBodies() {
		inA := strings.Contains(body, "alpha-")
		if strings.Contains(body, "alpha-") && strings.Contains(body, "beta-") {
			t.Errorf("a request mixes both sessions:\n%.400s", body)
		}
		if hasLate := strings.Contains(body, "LATE_FILE.txt"); hasLate == inA {
			t.Errorf("session %s: snapshot lists LATE_FILE.txt = %v", map[bool]string{true: "A", false: "B"}[inA], hasLate)
		}
	}
	if got := answersOf(h, a); got != "A1|A2" {
		t.Errorf("session A answers = %s", got)
	}
	if got := answersOf(h, b); got != "B1|B2" {
		t.Errorf("session B answers = %s", got)
	}
}

// A shared setting changed from one session (permission, model, thinking,
// Ultra live in the user config) reaches the selectors of every other
// session on the connection.
func TestACPEndToEnd_SharedSettingsReachOtherSessions(t *testing.T) {
	llm := newScriptedLLM(t)
	h := newACPHarness(t, llm, config.PermissionAskFirst)
	h.initialize()
	a := h.newSession("coding")
	b := h.newSession("ask")
	before := len(h.updates(b, "config_option_update"))
	if _, err := h.conn.SetSessionConfigOption(h.ctx(), acpsdk.SetSessionConfigOptionRequest{
		ValueId: &acpsdk.SetSessionConfigOptionValueId{SessionId: a, ConfigId: configIDPermission, Value: acpsdk.SessionConfigValueId(config.PermissionYOLO)},
	}); err != nil {
		t.Fatal(err)
	}
	updates := h.updates(b, "config_option_update")
	if len(updates) != before+1 {
		t.Fatalf("session B got %d config updates, want 1", len(updates)-before)
	}
	opts := jsonString(updates[len(updates)-1]["configOptions"])
	if !strings.Contains(opts, `"currentValue":"yolo","description":"How Spettro requests approval for actions","id":"permission"`) {
		t.Errorf("B's permission selector not updated: %s", opts)
	}
	if !strings.Contains(opts, `"currentValue":"ask"`) {
		t.Errorf("B's own mode must be kept: %s", opts)
	}
	// A mode change is per session and reaches no one else.
	before = len(h.updates(b, "config_option_update"))
	if _, err := h.conn.SetSessionConfigOption(h.ctx(), acpsdk.SetSessionConfigOptionRequest{
		ValueId: &acpsdk.SetSessionConfigOptionValueId{SessionId: a, ConfigId: configIDMode, Value: "plan"},
	}); err != nil {
		t.Fatal(err)
	}
	if n := len(h.updates(b, "config_option_update")); n != before {
		t.Errorf("a mode change in A sent B %d updates", n-before)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func waitForRequestContaining(t *testing.T, llm *scriptedLLM, marker string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		for _, body := range llm.requestBodies() {
			if strings.Contains(body, marker) {
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("no model request containing %q", marker)
}

// answersOf joins the agent_message_chunk texts sent to sid.
func answersOf(h *acpHarness, sid acpsdk.SessionId) string {
	var out []string
	for _, u := range h.updates(sid, "agent_message_chunk") {
		out = append(out, u["content"].(map[string]any)["text"].(string))
	}
	return strings.Join(out, "|")
}

func keys(m map[string]map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
