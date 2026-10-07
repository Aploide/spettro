package acp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	acpsdk "github.com/coder/acp-go-sdk"

	"spettro/internal/agent"
	"spettro/internal/workflow"
)

func wfTrace(name, status, args, output string) agent.ToolTrace {
	return agent.ToolTrace{AgentID: "coding", Name: name, Status: status, Args: args, Output: output}
}

// newSilentTurn builds a turnState with an already-cancelled context, so
// sessionUpdate drops every notification. These tests assert on the workflow
// model the traces accumulate, not on the wire traffic.
func newSilentTurn() *turnState {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return &turnState{
		bridge: newBridge(Options{}),
		ctx:    ctx,
		open:   map[string][]openToolCall{},
	}
}

func TestACPWorkflowTraceHandling(t *testing.T) {
	turn := newSilentTurn()

	// A trace with no workflow field is none of this code's business.
	if turn.onWorkflowTool(wfTrace("agent", "running", `{"agent":"code","task":"x"}`, "")) {
		t.Fatal("an ordinary delegation trace must not be claimed by the workflow path")
	}
	if turn.onWorkflowTool(wfTrace("bash", "running", `{"command":"ls"}`, "")) {
		t.Fatal("an ordinary tool trace must not be claimed")
	}

	if !turn.onWorkflowTool(wfTrace("workflow", "running",
		`{"run_id":"wf_1","workflow":"audit","description":"Audit the repo","phases":[{"title":"Scan"},{"title":"Judge"}]}`, "")) {
		t.Fatal("the lifecycle trace must be claimed")
	}
	if turn.workflow == nil || turn.workflow.name != "audit" || len(turn.workflow.phases) != 2 {
		t.Fatalf("workflow state = %+v", turn.workflow)
	}

	// A member's agent trace is recorded here but still travels the normal
	// path, so the editor gets a tool call for the sub-agent itself.
	if turn.onWorkflowTool(wfTrace("agent", "running",
		`{"agent":"general-purpose#1","task":"scan pkg/a","workflow":"audit","run_id":"wf_1","phase":"Scan"}`, "")) {
		t.Fatal("a workflow member's agent trace must still reach the generic tool-call path")
	}
	if len(turn.workflow.agents) != 1 {
		t.Fatalf("agents = %+v", turn.workflow.agents)
	}

	if !turn.onWorkflowTool(wfTrace("workflow-progress", "success",
		`{"run_id":"wf_1","workflow":"audit","kind":"log"}`, "3 packages")) {
		t.Fatal("a progress trace must be claimed")
	}
	rendered := turn.workflow.render()
	for _, want := range []string{"Audit the repo", "▸ Scan", "○ Judge — pending", "general-purpose#1", "3 packages"} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("render missing %q:\n%s", want, rendered)
		}
	}

	if !turn.onWorkflowTool(wfTrace("workflow", "success", `{"run_id":"wf_1","workflow":"audit"}`, "1 agent · 0 failed")) {
		t.Fatal("the finishing trace must be claimed")
	}
	if turn.workflow != nil {
		t.Fatal("the workflow tool call must be closed out at the end of the run")
	}
}

func TestACPWorkflowRenderOrdersPhases(t *testing.T) {
	w := &acpWorkflow{name: "flow", phases: []string{"Review", "Verify"}}
	w.agents = []acpWorkflowAgent{
		{Instance: "gp#1", Label: "a", Phase: "Verify", Status: "success"},
		{Instance: "gp#2", Label: "b", Phase: "Review", Status: "error"},
		{Instance: "gp#3", Label: "c", Phase: "Synthesise", Status: "running"},
		{Instance: "gp#4", Label: "d", Phase: "", Status: "running"},
	}
	out := w.render()
	order := []string{"Review", "Verify", "Synthesise", "(no phase)"}
	pos := -1
	for _, name := range order {
		i := strings.Index(out, name)
		if i < 0 {
			t.Fatalf("render missing %q:\n%s", name, out)
		}
		if i < pos {
			t.Fatalf("phase %q out of order:\n%s", name, out)
		}
		pos = i
	}
	if !strings.Contains(out, "1 failed") {
		t.Fatalf("failures must be counted:\n%s", out)
	}
}

func TestACPWorkflowToolTitles(t *testing.T) {
	cases := map[string]agent.ToolTrace{
		"workflow audit": wfTrace("workflow", "running", `{"workflow":"audit","run_id":"wf_1"}`, ""),
		"workflow audit ▸ Scan": wfTrace("workflow-progress", "success",
			`{"workflow":"audit","kind":"phase","phase":"Scan"}`, ""),
		"workflow audit · log": wfTrace("workflow-progress", "success",
			`{"workflow":"audit","kind":"log"}`, "hi"),
		"workflow audit ⏸ cp-2": wfTrace("workflow-progress", "success",
			`{"workflow":"audit","kind":"checkpoint","checkpoint_id":"cp-2","message":"m"}`, "{}"),
	}
	for want, tr := range cases {
		if got := toolCallTitle(tr); got != want {
			t.Fatalf("title = %q, want %q", got, want)
		}
	}
	if toolKind("workflow") != acpsdk.ToolKindThink {
		t.Fatalf("workflow should be a thinking-kind tool call")
	}
}

func TestACPWorkflowsTextAndRunRewrite(t *testing.T) {
	cwd := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	dir := filepath.Join(cwd, ".spettro", workflow.SavedWorkflowsDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	script := "export const meta = {name: 'audit', description: 'Audit the repo'}\nreturn 1"
	if err := os.WriteFile(filepath.Join(dir, "audit.js"), []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}

	if out := acpWorkflowsText(cwd, []string{"/workflows"}); !strings.Contains(out, "audit") ||
		!strings.Contains(out, "Audit the repo") {
		t.Fatalf("list = %q", out)
	}
	if out := acpWorkflowsText(cwd, []string{"/workflows", "show", "audit"}); !strings.Contains(out, "export const meta") {
		t.Fatalf("show = %q", out)
	}
	if out := acpWorkflowsText(cwd, []string{"/workflows", "where"}); !strings.Contains(out, ".spettro") {
		t.Fatalf("where = %q", out)
	}

	prompt, ok := acpWorkflowRunPrompt(cwd, `/workflows run audit {"deep": true}`)
	if !ok {
		t.Fatal("run of an existing workflow must rewrite into a prompt")
	}
	for _, want := range []string{"ultracode", `"name": "audit"`, `"args": {"deep": true}`} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q:\n%s", want, prompt)
		}
	}
	// The rewritten prompt must actually switch the tool on.
	if !agent.WorkflowRequested(prompt) {
		t.Fatal("the rewritten prompt does not activate workflows")
	}
	// Anything that is not a runnable workflow falls through to the normal
	// command path so the user sees the error, not a hallucinated turn.
	for _, in := range []string{"/workflows", "/workflows list", "/workflows run missing", "/ultra on"} {
		if _, ok := acpWorkflowRunPrompt(cwd, in); ok {
			t.Fatalf("%q must not be rewritten into a prompt", in)
		}
	}
}

// A saved workflow is a template: the run prompt has the agent read and adapt
// it (or run it as-is by name), lists its declared params, passes JSON args
// through byte for byte, and treats anything else as the task.
func TestACPWorkflowRunPromptIsATemplate(t *testing.T) {
	cwd := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	dir := filepath.Join(cwd, ".spettro", workflow.SavedWorkflowsDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	script := "export const meta = {name: 'audit', description: 'Audit the repo', params: {" +
		"base: {type: 'string', description: 'branch to diff against', default: 'main'}, " +
		"focus: {type: 'string', description: 'what to look at', required: true}}}\nreturn args"
	if err := os.WriteFile(filepath.Join(dir, "audit.js"), []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	// The prompt JSON-quotes the path, which doubles Windows backslashes.
	quotedPath, _ := json.Marshal(filepath.Join(dir, "audit.js"))
	if err := os.WriteFile(filepath.Join(dir, "plain.js"),
		[]byte("export const meta = {name: 'plain', description: 'No params'}\nreturn 1"), 0o644); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name, input string
		want, not   []string
	}{
		{
			name:  "json args keep their whitespace",
			input: "/workflows run audit   {\"focus\": \"auth  and   session\",\n \"base\": \"dev\"}",
			want: []string{
				"ultracode", "template", `"show": true`, string(quotedPath),
				"adapt anything task-specific or stale", "discovered at runtime",
				`"args": {"focus": "auth  and   session",` + "\n" + ` "base": "dev"}`,
				`base (string, default "main") — branch to diff against`,
				"focus (string, required) — what to look at",
			},
			not: []string{"Do not rewrite", "Task:"},
		},
		{
			name:  "free text is the task",
			input: "/workflows run audit look at the login flow\nand the token refresh",
			want:  []string{`{"name": "audit"}`, "Task: look at the login flow\nand the token refresh", "fits the task below"},
			not:   []string{`"args"`},
		},
		{
			name:  "no args",
			input: "/workflow start plain",
			want:  []string{`{"name": "plain"}`, "Declared params: none"},
			not:   []string{"Task:", `"args"`},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prompt, ok := acpWorkflowRunPrompt(cwd, tc.input)
			if !ok {
				t.Fatalf("%q was not rewritten", tc.input)
			}
			for _, want := range tc.want {
				if !strings.Contains(prompt, want) {
					t.Errorf("prompt missing %q:\n%s", want, prompt)
				}
			}
			for _, not := range tc.not {
				if strings.Contains(prompt, not) {
					t.Errorf("prompt must not contain %q:\n%s", not, prompt)
				}
			}
			if !agent.WorkflowRequested(prompt) {
				t.Error("the rewritten prompt does not activate workflows")
			}
		})
	}
}

func TestRestAfterFields(t *testing.T) {
	cases := []struct {
		in   string
		n    int
		want string
	}{
		{"/workflows run audit", 3, ""},
		{"/workflows run audit {\"a\":  1}", 3, "{\"a\":  1}"},
		{"  /workflows\trun\naudit \n line one\n  line two  ", 3, "line one\n  line two"},
		{"/workflows run", 3, ""},
	}
	for _, tc := range cases {
		if got := restAfterFields(tc.in, tc.n); got != tc.want {
			t.Errorf("restAfterFields(%q, %d) = %q, want %q", tc.in, tc.n, got, tc.want)
		}
	}
}

// A "run" of a workflow that does not exist must answer with the error, not
// fall through and become a prompt the model tries to satisfy by guessing.
func TestACPWorkflowsRunUnknownIsHandledInline(t *testing.T) {
	cwd := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	sess := &acpSession{cwd: cwd}

	reply, _, handled := handleExtendedSlashCommand(nil, sess, nil, nil, "/workflows run nope")
	if !handled || !strings.Contains(reply, "no saved workflow") {
		t.Fatalf("handled=%v reply=%q", handled, reply)
	}
	reply, _, handled = handleExtendedSlashCommand(nil, sess, nil, nil, "/workflows run")
	if !handled || !strings.Contains(reply, "usage:") {
		t.Fatalf("handled=%v reply=%q", handled, reply)
	}

	dir := filepath.Join(cwd, ".spettro", workflow.SavedWorkflowsDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "audit.js"),
		[]byte("export const meta = {name: 'audit', description: 'Audit'}\nreturn 1"), 0o644); err != nil {
		t.Fatal(err)
	}
	// An existing one falls through so the bridge's rewritten prompt runs.
	if _, _, handled := handleExtendedSlashCommand(nil, sess, nil, nil, "/workflows run audit"); handled {
		t.Fatal("a runnable workflow must fall through to the prompt path")
	}
}

// The finish payload is the one the observer builds from workflow.Result, so
// this test writes it the way workflowObserver.finish does rather than by
// hand: "cached" is a count there and a bool on a member trace, and the two
// used to share one Go struct. The finish payload then failed to unmarshal,
// onWorkflowTool disowned the trace, and the editor's workflow tool call was
// left in progress forever with a duplicate completed call appended after it.
func TestACPWorkflowFinishClosesTheCall(t *testing.T) {
	turn := newSilentTurn()

	if !turn.onWorkflowTool(wfTrace("workflow", "running",
		`{"run_id":"wf_1","workflow":"audit","description":"d","phases":[{"title":"Scan"}]}`, "")) {
		t.Fatal("start trace must be claimed")
	}
	if turn.workflow == nil {
		t.Fatal("start trace must open a run")
	}
	callID := turn.workflow.callID

	// A member reports a boolean "cached"; the run reports a count. Both must
	// be understood, and neither may disown the other's trace.
	turn.onWorkflowTool(wfTrace("agent", "running",
		`{"agent":"scan#1","task":"t","workflow":"audit","run_id":"wf_1","phase":"Scan","cached":true}`, ""))
	if n := len(turn.workflow.agents); n != 1 {
		t.Fatalf("member not recorded: %d agents", n)
	}
	if !turn.workflow.agents[0].Cached {
		t.Fatal("a replayed member must be marked cached")
	}

	if !turn.onWorkflowTool(wfTrace("workflow", "success",
		`{"agents":3,"cached":1,"failed":0,"run_id":"wf_1","tokens":28787,"workflow":"audit"}`,
		"3 agents · 0 failed · 1 replayed")) {
		t.Fatal("finish trace must be claimed — otherwise it escapes to the ordinary " +
			"tool path and the editor gets a second, orphaned workflow call")
	}
	if turn.workflow != nil {
		t.Fatalf("finish must close the run, still open: %+v", turn.workflow)
	}
	_ = callID
}

// A finish trace arriving with no run open is not ours to handle: it must fall
// through so the ordinary path still shows the user something.
func TestACPWorkflowFinishWithoutStart(t *testing.T) {
	turn := newSilentTurn()
	if turn.onWorkflowTool(wfTrace("workflow", "success",
		`{"agents":1,"cached":0,"failed":0,"run_id":"wf_9","tokens":1,"workflow":"audit"}`, "done")) {
		t.Fatal("a finish with no open run must not be claimed")
	}
}

// recordedUpdate is one session/update notification a recording turn sent.
type recordedUpdate struct {
	Kind       string `json:"sessionUpdate"`
	ToolCallID string `json:"toolCallId"`
	Title      string `json:"title"`
	Status     string `json:"status"`
	Content    []struct {
		Content struct {
			Text string `json:"text"`
		} `json:"content"`
	} `json:"content"`
	Meta map[string]json.RawMessage `json:"_meta"`
}

// workflow decodes the update's `_meta["spettro.app/workflow"]`; ok is false
// when it has none.
func (u recordedUpdate) workflow(t *testing.T) (m acpWorkflowMeta, ok bool) {
	t.Helper()
	raw, ok := u.Meta[workflowMetaKey]
	if !ok {
		return m, false
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("workflow meta does not decode: %v\n%s", err, raw)
	}
	return m, true
}

func (u recordedUpdate) text() string {
	var parts []string
	for _, c := range u.Content {
		parts = append(parts, c.Content.Text)
	}
	return strings.Join(parts, "\n")
}

// wireRecorder is the client end of a connection whose notifications the
// tests read back.
type wireRecorder struct {
	b   *bridge
	out *syncBuffer
}

func newWireRecorder(t *testing.T) *wireRecorder {
	t.Helper()
	b := newBridge(Options{})
	out := &syncBuffer{}
	pr, pw := io.Pipe()
	t.Cleanup(func() { _ = pw.Close() })
	b.conn = acpsdk.NewAgentSideConnection(b, out, pr)
	return &wireRecorder{b: b, out: out}
}

// turn starts a prompt turn on the recorder's connection that shares cards
// with every other turn built from the same set, as a session's turns do.
func (r *wireRecorder) turn(cards *acpWorkflowCards) *turnState {
	return &turnState{
		bridge:    r.b,
		ctx:       context.Background(),
		sessionID: "sess-wf",
		open:      map[string][]openToolCall{},
		workflows: cards,
	}
}

// updates waits until at least n notifications are on the wire and returns
// them in order.
func (r *wireRecorder) updates(t *testing.T, n int) []recordedUpdate {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		var got []recordedUpdate
		for _, line := range strings.Split(r.out.String(), "\n") {
			var msg struct {
				Method string `json:"method"`
				Params struct {
					Update recordedUpdate `json:"update"`
				} `json:"params"`
			}
			if json.Unmarshal([]byte(line), &msg) == nil && msg.Method == "session/update" {
				got = append(got, msg.Params.Update)
			}
		}
		if len(got) >= n || time.Now().After(deadline) {
			if len(got) < n {
				t.Fatalf("want %d notifications, got %d: %+v", n, len(got), got)
			}
			return got
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A run paused at a checkpoint is not finished: its card stays in progress,
// says what it waits for, and the session keeps its state for whoever
// continues it.
func TestACPWorkflowPausedKeepsCardOpen(t *testing.T) {
	rec := newWireRecorder(t)
	cards := newACPWorkflowCards()
	turn := rec.turn(cards)

	turn.onWorkflowTool(wfTrace("workflow", "running",
		`{"run_id":"wf_1","workflow":"audit","phases":[{"title":"Scan"}]}`, ""))
	turn.onWorkflowTool(wfTrace("agent", "success",
		`{"agent":"gp#1","task":"scan a","workflow":"audit","run_id":"wf_1","phase":"Scan"}`, ""))
	if !turn.onWorkflowTool(wfTrace("workflow", "paused",
		`{"run_id":"wf_1","workflow":"audit","checkpoint_id":"cp-1","message":"which findings to fix?"}`, "")) {
		t.Fatal("a paused trace must be claimed")
	}

	w := cards.runs["wf_1"]
	if w == nil || turn.workflow != w {
		t.Fatalf("paused run must stay on the session and the turn: cards=%v turn=%v", cards.runs, turn.workflow)
	}
	if w.status != "paused" || w.checkpointID != "cp-1" {
		t.Fatalf("paused state = %q %q", w.status, w.checkpointID)
	}
	if want := "⏸ paused at cp-1 — waiting for orchestrator: which findings to fix?"; !strings.Contains(w.render(), want) {
		t.Fatalf("render missing %q:\n%s", want, w.render())
	}

	ups := rec.updates(t, 3)
	last := ups[len(ups)-1]
	if last.ToolCallID != string(w.callID) || last.Status != "in_progress" {
		t.Fatalf("a paused card must stay in progress, last update = %+v", last)
	}
	if !strings.Contains(last.text(), "waiting for orchestrator") {
		t.Fatalf("the paused card must say it is waiting: %q", last.text())
	}
}

// The continue call's "running" trace (resumed:true) may arrive in a later
// turn. It must find the run's card where the earlier turn left it, keep
// everything recorded so far, and show it in the turn that drives it now.
func TestACPWorkflowResumeInLaterTurnReattaches(t *testing.T) {
	rec := newWireRecorder(t)
	cards := newACPWorkflowCards()
	first := rec.turn(cards)

	first.onWorkflowTool(wfTrace("workflow", "running",
		`{"run_id":"wf_1","workflow":"audit","description":"Audit","phases":[{"title":"Scan"},{"title":"Fix"}]}`, ""))
	first.onWorkflowTool(wfTrace("agent", "success",
		`{"agent":"gp#1","task":"scan a","workflow":"audit","run_id":"wf_1","phase":"Scan"}`, ""))
	first.onWorkflowTool(wfTrace("workflow-progress", "success",
		`{"run_id":"wf_1","workflow":"audit","kind":"log"}`, "3 findings"))
	first.onWorkflowTool(wfTrace("workflow", "paused",
		`{"run_id":"wf_1","workflow":"audit","checkpoint_id":"cp-1","message":"fix which?"}`, ""))
	w := cards.runs["wf_1"]
	oldID := w.callID

	second := rec.turn(cards)
	if !second.onWorkflowTool(wfTrace("workflow", "running",
		`{"run_id":"wf_1","workflow":"audit","resumed":true}`, "")) {
		t.Fatal("the resumed trace must be claimed")
	}
	if cards.runs["wf_1"] != w || second.workflow != w {
		t.Fatal("a resumed run must re-attach to its existing card state, not start a new one")
	}
	if w.status != "running" || w.checkpointID != "" {
		t.Fatalf("resuming must clear the pause: status=%q cp=%q", w.status, w.checkpointID)
	}
	if len(w.agents) != 1 || len(w.logs) != 1 || len(w.phases) != 2 || w.description != "Audit" {
		t.Fatalf("resumed card lost state: %+v", w)
	}
	if w.callID == oldID || w.turn != second {
		t.Fatalf("the continuing turn must announce the card itself: id %q (was %q)", w.callID, oldID)
	}

	second.onWorkflowTool(wfTrace("agent", "running",
		`{"agent":"gp#2","task":"fix a","workflow":"audit","run_id":"wf_1","phase":"Fix"}`, ""))
	if len(w.agents) != 2 {
		t.Fatalf("members after resume = %+v", w.agents)
	}
	if !second.onWorkflowTool(wfTrace("workflow", "success",
		`{"run_id":"wf_1","workflow":"audit","agents":2,"cached":0,"failed":0,"tokens":9}`, "2 agents")) {
		t.Fatal("finish must be claimed")
	}
	if _, ok := cards.runs["wf_1"]; ok || second.workflow != nil {
		t.Fatal("a finished run must leave the session's cards")
	}

	// On the wire: the earlier turn's card is closed with a pointer forward,
	// and the later turn opens its own card carrying the state so far.
	// Four from the first turn (start, member, log, pause), four from the
	// second (close the old card, open the new one, member, finish).
	ups := rec.updates(t, 8)
	var closedOld, openedNew bool
	for _, u := range ups {
		if u.ToolCallID == string(oldID) && u.Status == "completed" &&
			strings.Contains(u.text(), "continued in a later turn") &&
			strings.Contains(u.text(), "paused at cp-1") {
			closedOld = true
		}
		if u.Kind == "tool_call" && u.ToolCallID == string(w.callID) && strings.Contains(u.text(), "gp#1") {
			openedNew = true
		}
	}
	if !closedOld || !openedNew {
		t.Fatalf("closedOld=%v openedNew=%v updates=%+v", closedOld, openedNew, ups)
	}
}

// Continued in the turn that paused it, the run keeps its card and ID.
func TestACPWorkflowResumeInSameTurnKeepsCard(t *testing.T) {
	turn := newSilentTurn()
	turn.onWorkflowTool(wfTrace("workflow", "running", `{"run_id":"wf_1","workflow":"audit"}`, ""))
	w := turn.workflow
	id := w.callID
	turn.onWorkflowTool(wfTrace("workflow", "paused", `{"run_id":"wf_1","workflow":"audit","checkpoint_id":"cp-1"}`, ""))
	turn.onWorkflowTool(wfTrace("workflow", "running", `{"run_id":"wf_1","workflow":"audit","resumed":true}`, ""))
	if turn.workflow != w || w.callID != id || w.status != "running" {
		t.Fatalf("same-turn continue must reuse the card: %+v", w)
	}
}

// A run starting over under a run_id the session already knows (not a
// continue) gets a fresh card whose ID cannot collide with the old one's.
func TestACPWorkflowRestartWithoutResumedStartsFresh(t *testing.T) {
	turn := newSilentTurn()
	turn.onWorkflowTool(wfTrace("workflow", "running", `{"run_id":"wf_1","workflow":"audit"}`, ""))
	turn.onWorkflowTool(wfTrace("workflow-progress", "success", `{"run_id":"wf_1","workflow":"audit","kind":"log"}`, "old"))
	old := turn.workflow
	turn.onWorkflowTool(wfTrace("workflow", "running", `{"run_id":"wf_1","workflow":"audit"}`, ""))
	if turn.workflow == old || len(turn.workflow.logs) != 0 {
		t.Fatal("a non-resumed start must not inherit the previous card's state")
	}
	if turn.workflow.callID == old.callID {
		t.Fatalf("restarted card reused ID %q", old.callID)
	}
}

// The model's own call of the workflow tool is a "workflow" trace too, but its
// arguments are the tool's: it must stay a generic card, also when it is a
// continue call naming a run the session holds.
func TestACPWorkflowToolCallTraceIsNotALifecycleTrace(t *testing.T) {
	turn := newSilentTurn()
	turn.onWorkflowTool(wfTrace("workflow", "running", `{"run_id":"wf_1","workflow":"audit"}`, ""))
	for _, args := range []string{
		`{"name":"audit","args":{"deep":true}}`,
		`{"continue_run_id":"wf_1","reply":{"fix":["a"]}}`,
		`{"continue_run_id":"wf_1","stop":true}`,
	} {
		for _, status := range []string{"running", "success"} {
			if turn.onWorkflowTool(wfTrace("workflow", status, args, "")) {
				t.Fatalf("tool-call trace %s (%s) was claimed", args, status)
			}
		}
	}
	if turn.workflow == nil {
		t.Fatal("a tool-call trace closed the run's card")
	}
}

func TestACPWorkflowCheckpointProgress(t *testing.T) {
	turn := newSilentTurn()
	turn.onWorkflowTool(wfTrace("workflow", "running", `{"run_id":"wf_1","workflow":"audit"}`, ""))
	if !turn.onWorkflowTool(wfTrace("workflow-progress", "success",
		`{"run_id":"wf_1","workflow":"audit","kind":"checkpoint","phase":"Scan","checkpoint_id":"cp-1","message":"pick targets"}`,
		`{"findings":[1,2,3]}`)) {
		t.Fatal("a checkpoint trace must be claimed")
	}
	turn.onWorkflowTool(wfTrace("workflow-progress", "success",
		`{"run_id":"wf_1","workflow":"audit","kind":"checkpoint","checkpoint_id":"cp-2","message":"again","cached":true}`, "null"))
	out := turn.workflow.render()
	for _, want := range []string{"⏸ cp-1 pick targets", "⏸ cp-2 (answer replayed) again"} {
		if !strings.Contains(out, want) {
			t.Fatalf("render missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "findings") {
		t.Fatalf("checkpoint data belongs to the orchestrator, not the card:\n%s", out)
	}
}

// Statuses this code does not know never read as done: not for a member, and
// not for the run, which stays open instead of being closed as completed.
func TestACPWorkflowUnknownStatusesAreNotDone(t *testing.T) {
	turn := newSilentTurn()
	turn.onWorkflowTool(wfTrace("workflow", "running",
		`{"run_id":"wf_1","workflow":"audit","phases":[{"title":"Scan"}]}`, ""))
	turn.onWorkflowTool(wfTrace("agent", "queued",
		`{"agent":"gp#1","task":"t","workflow":"audit","run_id":"wf_1","phase":"Scan"}`, ""))
	out := turn.workflow.render()
	if strings.Contains(out, "✓") || !strings.Contains(out, "0/1 done") || !strings.Contains(out, "· gp#1") {
		t.Fatalf("an unknown member status rendered as done:\n%s", out)
	}

	if !turn.onWorkflowTool(wfTrace("workflow", "draining", `{"run_id":"wf_1","workflow":"audit"}`, "")) {
		t.Fatal("an unknown lifecycle status must still be claimed")
	}
	if turn.workflow == nil {
		t.Fatal("an unknown lifecycle status closed the card")
	}
	if !strings.Contains(turn.workflow.render(), "status: draining") {
		t.Fatalf("unknown status not shown:\n%s", turn.workflow.render())
	}

	if !turn.onWorkflowTool(wfTrace("workflow", "stopped", `{"run_id":"wf_1","workflow":"audit"}`, "stopped by orchestrator")) {
		t.Fatal("a stop must be claimed")
	}
	if turn.workflow != nil {
		t.Fatal("a stopped run must close its card")
	}
}

func TestACPWorkflowAgentGlyphs(t *testing.T) {
	for status, want := range map[string]string{
		"success": "✓", "error": "✗", "running": "▶", "queued": "·", "": "·",
	} {
		if got := agentGlyph(status); got != want {
			t.Errorf("agentGlyph(%q) = %q, want %q", status, got, want)
		}
	}
}

func TestACPWorkflowDynamicPhases(t *testing.T) {
	turn := newSilentTurn()
	turn.onWorkflowTool(wfTrace("workflow", "running",
		`{"run_id":"wf_1","workflow":"audit","phases":[{"title":"Scan","detail":"find candidates"}]}`, ""))
	turn.onWorkflowTool(wfTrace("workflow-progress", "success",
		`{"run_id":"wf_1","workflow":"audit","kind":"phase","phase":"Scan","dynamic":false}`, "Scan"))
	turn.onWorkflowTool(wfTrace("workflow-progress", "success",
		`{"run_id":"wf_1","workflow":"audit","kind":"phase","phase":"Verify auth","detail":"3 suspects","dynamic":true}`, "Verify auth"))
	out := turn.workflow.render()
	for _, want := range []string{"○ Scan — pending", "↳ find candidates", "○ Verify auth (added at runtime) — pending", "↳ 3 suspects"} {
		if !strings.Contains(out, want) {
			t.Fatalf("render missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Scan (added at runtime)") {
		t.Fatalf("a declared phase was marked dynamic:\n%s", out)
	}
	if strings.Index(out, "Scan") > strings.Index(out, "Verify auth") {
		t.Fatalf("declared phases come first:\n%s", out)
	}
}

func TestACPWorkflowSizeInTitle(t *testing.T) {
	cases := []struct {
		args, title, body string
	}{
		{`{"run_id":"wf_1","workflow":"audit"}`, "workflow audit", ""},
		{`{"run_id":"wf_1","workflow":"audit","size":"large","size_agents":30}`, "workflow audit · large", "size: large (~30 agents, a guideline)"},
		{`{"run_id":"wf_1","workflow":"audit","size":"unbounded","size_agents":0,"budget_tokens":500000}`,
			"workflow audit · unbounded · budget 500k", "size: unbounded (no guideline) · budget 500k tokens"},
	}
	for _, tc := range cases {
		turn := newSilentTurn()
		turn.onWorkflowTool(wfTrace("workflow", "running", tc.args, ""))
		if got := turn.workflow.title(); got != tc.title {
			t.Errorf("title(%s) = %q, want %q", tc.args, got, tc.title)
		}
		if out := turn.workflow.render(); tc.body != "" && !strings.Contains(out, tc.body) {
			t.Errorf("render(%s) missing %q:\n%s", tc.args, tc.body, out)
		}
	}
}

func TestCompactTokens(t *testing.T) {
	for n, want := range map[int]string{
		999: "999", 1000: "1k", 1500: "1.5k", 500_000: "500k", 750_000: "750k",
		1_000_000: "1m", 1_500_000: "1.5m", 2_000_000: "2m", 1_250_000: "1.25m",
	} {
		if got := compactTokens(n); got != want {
			t.Errorf("compactTokens(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestACPWorkflowLogTailIsBounded(t *testing.T) {
	w := &acpWorkflow{name: "audit"}
	for i := range maxWorkflowLogLines + 10 {
		w.addLog(fmt.Sprintf("round %d", i))
	}
	if len(w.logs) != maxWorkflowLogLines || w.dropped != 10 {
		t.Fatalf("logs=%d dropped=%d", len(w.logs), w.dropped)
	}
	out := w.render()
	if !strings.Contains(out, "… 10 earlier lines") || strings.Contains(out, "round 9\n") ||
		!strings.Contains(out, fmt.Sprintf("round %d", maxWorkflowLogLines+9)) {
		t.Fatalf("render:\n%s", out)
	}
}

// A trace reaching a turn that has ended (a run whose turn was cancelled
// under it reports its stop through that turn) must not take the card away
// from the turn now showing it.
func TestACPWorkflowDeadTurnDoesNotStealCard(t *testing.T) {
	cards := newACPWorkflowCards()
	// Both turns are silent (cancelled contexts drop every notification); a
	// card nobody has announced yet is still taken by the turn that opens it.
	live := newSilentTurn()
	live.workflows = cards
	live.onWorkflowTool(wfTrace("workflow", "running", `{"run_id":"wf_1","workflow":"audit"}`, ""))
	w := cards.runs["wf_1"]
	dead := newSilentTurn()
	dead.workflows = cards
	dead.onWorkflowTool(wfTrace("workflow-progress", "success", `{"run_id":"wf_1","workflow":"audit","kind":"log"}`, "late"))
	if w.turn != live || w.attach != 1 {
		t.Fatalf("a dead turn took the card: turn=%p live=%p attach=%d", w.turn, live, w.attach)
	}
	if len(w.logs) != 1 {
		t.Fatal("the late trace's content must still be recorded")
	}
}

// A run the orchestrator stops (continue_run_id + stop:true) reports status
// "stopped" with a reason. That is a deliberate end, not a failure: the card
// closes as completed and says it was stopped and why, instead of reading as
// done or as failed.
func TestACPWorkflowStoppedClosesAsCompleted(t *testing.T) {
	rec := newWireRecorder(t)
	cards := newACPWorkflowCards()
	turn := rec.turn(cards)
	turn.onWorkflowTool(wfTrace("workflow", "running", `{"run_id":"wf_1","workflow":"audit"}`, ""))
	callID := turn.workflow.callID
	if !turn.onWorkflowTool(wfTrace("workflow", "stopped",
		`{"run_id":"wf_1","workflow":"audit","reason":"at the orchestrator's request"}`, "0 agents")) {
		t.Fatal("a stopped trace must be claimed")
	}
	if _, ok := cards.runs["wf_1"]; ok || turn.workflow != nil {
		t.Fatal("a stopped run must leave the session's cards and the turn")
	}
	ups := rec.updates(t, 2)
	last := ups[len(ups)-1]
	if last.ToolCallID != string(callID) || last.Status != "completed" {
		t.Fatalf("a stopped run's card must close as completed, last update = %+v", last)
	}
	if !strings.Contains(last.text(), "■ stopped: at the orchestrator's request") {
		t.Fatalf("the card must say why the run stopped:\n%s", last.text())
	}
}

// A paused run is detached: its tool call returned and its observer is
// unbound, so when the idle reaper (or StopAll) stops it no finish trace can
// reach any turn. The registry's stop hook must close the card instead, on a
// session update of its own, and take it out of the session's cards. Before
// the hook the card read "paused … waiting for orchestrator" for good and
// its state stayed in the session for the rest of it.
func TestACPWorkflowStopHookClosesDetachedCard(t *testing.T) {
	rec := newWireRecorder(t)
	cards := newACPWorkflowCards()
	turn := rec.turn(cards)
	turn.onWorkflowTool(wfTrace("workflow", "running", `{"run_id":"wf_1","workflow":"audit"}`, ""))
	turn.onWorkflowTool(wfTrace("workflow", "paused",
		`{"run_id":"wf_1","workflow":"audit","checkpoint_id":"cp-1","message":"fix which?"}`, ""))
	callID := cards.runs["wf_1"].callID

	hook := workflowStopHook(cards, rec.b.sessionNotifier("sess-wf"))
	hook("wf_1", "audit", "paused for over 30m with no continue")
	if _, ok := cards.runs["wf_1"]; ok {
		t.Fatal("a reaped run's card must leave the session's cards")
	}
	ups := rec.updates(t, 3)
	last := ups[2]
	if last.ToolCallID != string(callID) || last.Status != "completed" {
		t.Fatalf("a reaped run's card must close as completed, update = %+v", last)
	}
	if !strings.Contains(last.text(), "■ stopped: paused for over 30m with no continue") ||
		strings.Contains(last.text(), "waiting for orchestrator") {
		t.Fatalf("the closed card must say why it stopped:\n%s", last.text())
	}

	// A second report for the same run (or one for a run whose card is
	// already gone) sends nothing.
	hook("wf_1", "audit", "the session ended")
	hook("wf_unknown", "x", "the session ended")
	time.Sleep(50 * time.Millisecond)
	if n := len(rec.updates(t, 3)); n != 3 {
		t.Fatalf("a run without a card must not produce updates, got %d", n)
	}
}

var (
	wfPendingLine = regexp.MustCompile(`^○ (.*) — pending$`)
	wfActiveLine  = regexp.MustCompile(`^▸ (.*) — (\d+)/(\d+) done(?:, (\d+) failed)?$`)
	wfMemberLine  = regexp.MustCompile(`^    ([✓✗▶·]) (\S+)  (replayed · )?(.*)$`)
)

// assertMetaMatchesText reads the phases and members back out of a card's
// text, the way a meta-less client parses them, and checks the card's meta
// says the same: same phases in the same order with the same counts, and the
// same members with the same states.
func assertMetaMatchesText(t *testing.T, u recordedUpdate, m acpWorkflowMeta) {
	t.Helper()
	var phases []acpWorkflowMetaPhase
	var members []acpWorkflowMetaMember
	for _, line := range strings.Split(u.text(), "\n") {
		var title string
		var p acpWorkflowMetaPhase
		if g := wfPendingLine.FindStringSubmatch(line); g != nil {
			title = g[1]
		} else if g := wfActiveLine.FindStringSubmatch(line); g != nil {
			title = g[1]
			p.Done, _ = strconv.Atoi(g[2])
			p.Total, _ = strconv.Atoi(g[3])
			p.Failed, _ = strconv.Atoi(g[4])
		} else if g := wfMemberLine.FindStringSubmatch(line); g != nil {
			status := map[string]string{"✓": "done", "✗": "failed", "▶": "running", "·": "pending"}[g[1]]
			members = append(members, acpWorkflowMetaMember{Instance: g[2], Task: g[4], Status: status, Replayed: g[3] != ""})
			continue
		} else {
			continue
		}
		if t, ok := strings.CutSuffix(title, " (added at runtime)"); ok {
			title, p.Dynamic = t, true
		}
		if title == "(no phase)" {
			title = ""
		}
		p.Title = title
		phases = append(phases, p)
	}
	if len(phases) != len(m.Phases) {
		t.Fatalf("text shows %d phases, meta %d:\ntext: %s\nmeta: %+v", len(phases), len(m.Phases), u.text(), m.Phases)
	}
	for i, p := range phases {
		mp := m.Phases[i]
		mp.Detail = ""
		if p != mp {
			t.Fatalf("phase %d: text %+v, meta %+v", i, p, m.Phases[i])
		}
	}
	if len(members) != len(m.Members) {
		t.Fatalf("text shows %d members, meta %d:\ntext: %s\nmeta: %+v", len(members), len(m.Members), u.text(), m.Members)
	}
	failed, replayed := 0, 0
	for i, a := range members {
		mm := m.Members[i]
		if a.Instance != mm.Instance || a.Task != mm.Task || a.Status != mm.Status || a.Replayed != mm.Replayed {
			t.Fatalf("member %d: text %+v, meta %+v", i, a, mm)
		}
		if a.Status == "failed" {
			failed++
		}
		if a.Replayed {
			replayed++
		}
	}
	if want := (acpWorkflowMetaCounts{Agents: len(members), Failed: failed, Replayed: replayed}); m.Counts != want {
		t.Fatalf("counts = %+v, the text shows %+v", m.Counts, want)
	}
}

// workflowCardUpdates returns the workflow card's notifications among ups,
// each with its decoded meta; every one of them must carry the meta and
// agree with its own text.
func workflowCardUpdates(t *testing.T, ups []recordedUpdate) ([]recordedUpdate, []acpWorkflowMeta) {
	t.Helper()
	var cards []recordedUpdate
	var metas []acpWorkflowMeta
	for _, u := range ups {
		if !strings.HasPrefix(u.ToolCallID, "workflow-") {
			continue
		}
		m, ok := u.workflow(t)
		if !ok {
			t.Fatalf("workflow card update without %s meta: %+v", workflowMetaKey, u)
		}
		if m.Version != workflowMetaVersion || m.RunID != "wf_1" || m.Name != "audit" {
			t.Fatalf("meta header = version %d run %q name %q", m.Version, m.RunID, m.Name)
		}
		assertMetaMatchesText(t, u, m)
		cards = append(cards, u)
		metas = append(metas, m)
	}
	return cards, metas
}

// Every tool_call and tool_call_update a workflow card sends carries its
// state as `_meta["spettro.app/workflow"]`, built from the state render()
// reads: start, member and log updates, the pause, the close of the card an
// earlier turn showed, the card the continuing turn opens, and the finish.
func TestACPWorkflowMetaFollowsTheRun(t *testing.T) {
	rec := newWireRecorder(t)
	cards := newACPWorkflowCards()
	first := rec.turn(cards)

	first.onWorkflowTool(wfTrace("workflow", "running",
		`{"run_id":"wf_1","workflow":"audit","description":"Audit the repo","size":"large","size_agents":30,"budget_tokens":500000,`+
			`"phases":[{"title":"Scan","detail":"find candidates"},{"title":"Fix"}]}`, ""))
	first.onWorkflowTool(wfTrace("agent", "success",
		`{"agent":"gp#1","task":"scan a","workflow":"audit","run_id":"wf_1","phase":"Scan"}`, ""))
	first.onWorkflowTool(wfTrace("agent", "error",
		`{"agent":"gp#2","task":"scan b","workflow":"audit","run_id":"wf_1","phase":"Scan"}`, ""))
	first.onWorkflowTool(wfTrace("agent", "success",
		`{"agent":"gp#3","task":"scan c","workflow":"audit","run_id":"wf_1","phase":"Scan","cached":true}`, ""))
	first.onWorkflowTool(wfTrace("workflow-progress", "success",
		`{"run_id":"wf_1","workflow":"audit","kind":"phase","phase":"Verify","detail":"2 suspects","dynamic":true}`, ""))
	first.onWorkflowTool(wfTrace("workflow-progress", "success",
		`{"run_id":"wf_1","workflow":"audit","kind":"log"}`, "3 findings"))
	first.onWorkflowTool(wfTrace("workflow", "paused",
		`{"run_id":"wf_1","workflow":"audit","checkpoint_id":"cp-1","message":"fix which?"}`, ""))

	second := rec.turn(cards)
	second.onWorkflowTool(wfTrace("workflow", "running", `{"run_id":"wf_1","workflow":"audit","resumed":true}`, ""))
	second.onWorkflowTool(wfTrace("agent", "running",
		`{"agent":"gp#4","task":"fix a","workflow":"audit","run_id":"wf_1","phase":"Fix"}`, ""))
	second.onWorkflowTool(wfTrace("agent", "queued",
		`{"agent":"gp#5","task":"loose","workflow":"audit","run_id":"wf_1"}`, ""))
	second.onWorkflowTool(wfTrace("workflow", "success",
		`{"run_id":"wf_1","workflow":"audit","agents":5,"cached":1,"failed":1,"tokens":9}`, "5 agents · 1 failed · 1 replayed"))

	// First turn: start, 3 members, phase, log, pause. Second: close the old
	// card, open the new one, 2 members, finish.
	ups, metas := workflowCardUpdates(t, rec.updates(t, 12))
	if len(ups) != 12 {
		t.Fatalf("want 12 workflow card updates, got %d", len(ups))
	}

	start := metas[0]
	if ups[0].Kind != "tool_call" || start.Status != "running" || start.Attach != 1 ||
		start.Description != "Audit the repo" || start.Size != "large" || start.SizeAgents != 30 || start.BudgetTokens != 500000 {
		t.Fatalf("start meta = %+v", start)
	}
	if len(start.Phases) != 2 || start.Phases[0].Detail != "find candidates" || start.Phases[0].Total != 0 {
		t.Fatalf("declared phases = %+v", start.Phases)
	}

	paused := metas[6]
	if paused.Status != "paused" || paused.PausedAt == nil ||
		*paused.PausedAt != (acpWorkflowMetaPause{CheckpointID: "cp-1", Message: "fix which?"}) {
		t.Fatalf("paused meta = %+v", paused)
	}
	if scan := paused.Phases[0]; scan != (acpWorkflowMetaPhase{Title: "Scan", Detail: "find candidates", Done: 3, Total: 3, Failed: 1}) {
		t.Fatalf("Scan phase = %+v", scan)
	}
	if verify := paused.Phases[2]; verify.Title != "Verify" || !verify.Dynamic || verify.Detail != "2 suspects" {
		t.Fatalf("runtime-added phase = %+v", verify)
	}
	if len(paused.LogTail) != 1 || paused.LogTail[0] != "3 findings" || paused.DroppedLogLines != 0 {
		t.Fatalf("log tail = %q dropped %d", paused.LogTail, paused.DroppedLogLines)
	}

	closed, closedMeta := ups[7], metas[7]
	if closed.ToolCallID != "workflow-wf_1" || closed.Status != "completed" ||
		closedMeta.ContinuedIn != "workflow-wf_1-2" || closedMeta.Status != "paused" || closedMeta.Attach != 1 {
		t.Fatalf("the earlier turn's closed card: %+v meta %+v", closed, closedMeta)
	}
	opened, openedMeta := ups[8], metas[8]
	if opened.Kind != "tool_call" || opened.ToolCallID != "workflow-wf_1-2" ||
		openedMeta.ContinuedFrom != "workflow-wf_1" || openedMeta.Attach != 2 || openedMeta.Status != "running" ||
		openedMeta.PausedAt != nil || openedMeta.Counts.Agents != 3 {
		t.Fatalf("the continuing turn's card: %+v meta %+v", opened, openedMeta)
	}

	done := metas[11]
	if ups[11].Status != "completed" || done.Status != "success" || done.Summary != "5 agents · 1 failed · 1 replayed" ||
		done.ContinuedFrom != "workflow-wf_1" {
		t.Fatalf("finished meta = %+v", done)
	}
	if done.Counts != (acpWorkflowMetaCounts{Agents: 5, Failed: 1, Replayed: 1}) {
		t.Fatalf("counts = %+v", done.Counts)
	}
	last := done.Members[len(done.Members)-1]
	if last.Instance != "gp#5" || last.Phase != "" || last.Status != "pending" {
		t.Fatalf("an unknown member status must read as pending, outside any phase: %+v", last)
	}
	if loose := done.Phases[len(done.Phases)-1]; loose.Title != "" || loose.Total != 1 {
		t.Fatalf("the no-phase bucket = %+v", loose)
	}
}

// A run's end states each have their meta status: a stop (by trace or by the
// stop hook for a detached run) says why, a failure reads "failed", and a
// status this code does not know passes through on a card that stays open.
func TestACPWorkflowMetaEndStates(t *testing.T) {
	cases := []struct {
		status, args, wantStatus, wantCard, wantReason string
	}{
		{"stopped", `{"run_id":"wf_1","workflow":"audit","reason":"at the orchestrator's request"}`,
			"stopped", "completed", "at the orchestrator's request"},
		{"error", `{"run_id":"wf_1","workflow":"audit"}`, "failed", "failed", ""},
		{"canceled", `{"run_id":"wf_1","workflow":"audit"}`, "cancelled", "failed", ""},
		{"draining", `{"run_id":"wf_1","workflow":"audit"}`, "draining", "in_progress", ""},
	}
	for _, tc := range cases {
		rec := newWireRecorder(t)
		turn := rec.turn(newACPWorkflowCards())
		turn.onWorkflowTool(wfTrace("workflow", "running", `{"run_id":"wf_1","workflow":"audit"}`, ""))
		turn.onWorkflowTool(wfTrace("workflow", tc.status, tc.args, "1 agent"))
		ups, metas := workflowCardUpdates(t, rec.updates(t, 2))
		u, m := ups[len(ups)-1], metas[len(metas)-1]
		if u.Status != tc.wantCard || m.Status != tc.wantStatus || m.StoppedReason != tc.wantReason {
			t.Errorf("%s: card %q meta status %q reason %q, want %q %q %q",
				tc.status, u.Status, m.Status, m.StoppedReason, tc.wantCard, tc.wantStatus, tc.wantReason)
		}
		if tc.status == "stopped" && m.Summary != "" {
			t.Errorf("a stopped card's reason is its summary; meta summary = %q", m.Summary)
		}
	}

	// A paused run reaped while detached closes through the stop hook, and
	// that update carries the meta too.
	rec := newWireRecorder(t)
	cards := newACPWorkflowCards()
	turn := rec.turn(cards)
	turn.onWorkflowTool(wfTrace("workflow", "running", `{"run_id":"wf_1","workflow":"audit"}`, ""))
	turn.onWorkflowTool(wfTrace("workflow", "paused", `{"run_id":"wf_1","workflow":"audit","checkpoint_id":"cp-1"}`, ""))
	workflowStopHook(cards, rec.b.sessionNotifier("sess-wf"))("wf_1", "audit", "the conversation was cleared")
	_, metas := workflowCardUpdates(t, rec.updates(t, 3))
	if m := metas[2]; m.Status != "stopped" || m.StoppedReason != "the conversation was cleared" || m.PausedAt != nil {
		t.Fatalf("reaped card meta = %+v", m)
	}
}

// The meta's arrays are never null, so a client can iterate them without a
// guard, and the log tail is the same bounded tail the text shows.
func TestACPWorkflowMetaShape(t *testing.T) {
	w := &acpWorkflow{runID: "wf_1", name: "audit"}
	raw, err := json.Marshal(w.meta())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"phases":[]`, `"members":[]`, `"logTail":[]`, `"status":"running"`, `"version":1`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("empty card meta missing %s: %s", want, raw)
		}
	}
	for _, absent := range []string{"pausedAt", "stoppedReason", "continuedFrom", "continuedIn", "summary"} {
		if strings.Contains(string(raw), absent) {
			t.Errorf("empty card meta carries %s: %s", absent, raw)
		}
	}
	for i := range maxWorkflowLogLines + 5 {
		w.addLog(fmt.Sprintf("round %d", i))
	}
	m := w.metaView()
	if len(m.LogTail) != maxWorkflowLogLines || m.DroppedLogLines != 5 || m.LogTail[0] != "round 5" {
		t.Fatalf("log tail = %d lines from %q, dropped %d", len(m.LogTail), m.LogTail[0], m.DroppedLogLines)
	}
}

// A run without an ID gets a per-turn "wf-N" card; it carries the meta all the
// same, with an empty runId. A failed run's summary is the error the text
// opens with, not a tally.
func TestACPWorkflowMetaWithoutRunID(t *testing.T) {
	rec := newWireRecorder(t)
	turn := rec.turn(newACPWorkflowCards())
	turn.onWorkflowTool(wfTrace("workflow", "running", `{"workflow":"audit","phases":[{"title":"Scan"}]}`, ""))
	turn.onWorkflowTool(wfTrace("agent", "error", `{"agent":"gp#1","task":"scan a","workflow":"audit","phase":"Scan"}`, ""))
	turn.onWorkflowTool(wfTrace("workflow", "error", `{"workflow":"audit"}`, "script threw: boom"))
	ups := rec.updates(t, 3)
	var metas []acpWorkflowMeta
	for _, u := range ups {
		if !strings.HasPrefix(u.ToolCallID, "wf-") {
			continue
		}
		m, ok := u.workflow(t)
		if !ok {
			t.Fatalf("card update without %s meta: %+v", workflowMetaKey, u)
		}
		if m.RunID != "" || m.Name != "audit" || m.Attach != 1 || m.ContinuedFrom != "" || m.ContinuedIn != "" {
			t.Fatalf("meta header = %+v", m)
		}
		assertMetaMatchesText(t, u, m)
		metas = append(metas, m)
	}
	if len(metas) != 3 {
		t.Fatalf("want 3 card updates with meta, got %d", len(metas))
	}
	if last := metas[2]; last.Status != "failed" || last.Summary != "script threw: boom" || last.Counts.Failed != 1 {
		t.Fatalf("failed run meta = %+v", last)
	}
}
