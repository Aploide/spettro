package acp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
	if turn.onWorkflowTool(wfTrace("agent", "running", `{"agent":"code#1","task":"x","swarm":true}`, "")) {
		t.Fatal("an ultra swarm trace must not be claimed by the workflow path")
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
