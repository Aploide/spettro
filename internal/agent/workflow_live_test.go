package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"spettro/internal/config"
	"spettro/internal/provider"
	"spettro/internal/workflow"
)

// subagentServer is a fake OpenAI-compatible provider for workflow members:
// every request is a sub-agent's turn, answered with plain text (no tool
// calls), so each agent() costs exactly one request.
type subagentServer struct {
	url  string
	pm   *provider.Manager
	hits atomic.Int32
	// started receives one value per request, before it is answered.
	started chan string
}

// newSubagentServer answers each sub-agent with answer(task). A nil answer
// blocks until the request is cancelled — a member that is still running.
func newSubagentServer(t *testing.T, answer func(task string) string) *subagentServer {
	t.Helper()
	s := &subagentServer{started: make(chan string, 64)}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		s.hits.Add(1)
		task := lastUserText(body)
		select {
		case s.started <- task:
		default:
		}
		if answer == nil {
			<-r.Context().Done()
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "chatcmpl-test", "object": "chat.completion",
			"choices": []any{map[string]any{"index": 0, "finish_reason": "stop",
				"message": map[string]any{"role": "assistant", "content": answer(task)}}},
			"usage": map[string]any{"prompt_tokens": 30, "completion_tokens": 5, "total_tokens": 35},
		})
	}))
	t.Cleanup(srv.Close)
	s.url = srv.URL
	s.pm = provider.NewManager()
	s.pm.AddLocalModels([]provider.Model{{Provider: srv.URL, Name: "m", Local: true}})
	return s
}

// traceLog collects the traces a runtime emits.
type traceLog struct {
	mu     sync.Mutex
	traces []ToolTrace
}

func (l *traceLog) add(tr ToolTrace) {
	l.mu.Lock()
	l.traces = append(l.traces, tr)
	l.mu.Unlock()
}

func (l *traceLog) all() []ToolTrace {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]ToolTrace(nil), l.traces...)
}

// find returns the payloads of the traces named name with status status.
func (l *traceLog) find(name, status string) []map[string]any {
	var out []map[string]any
	for _, tr := range l.all() {
		if tr.Name != name || (status != "" && tr.Status != status) {
			continue
		}
		var payload map[string]any
		_ = json.Unmarshal([]byte(tr.Args), &payload)
		out = append(out, payload)
	}
	return out
}

// workflowTestRuntime builds a top-level runtime that can run workflows
// against srv (nil: a provider no agent may reach), pre-approved, recording
// its traces.
func workflowTestRuntime(t *testing.T, cwd string, srv *subagentServer, runs *WorkflowRuns) (*toolRuntime, *traceLog) {
	t.Helper()
	manifest := config.DefaultAgentManifest()
	log := &traceLog{}
	rt := &toolRuntime{
		cwd: cwd, manifest: &manifest, providerMgr: &provider.Manager{},
		permission:          config.PermissionYOLO,
		workflowPreapproved: true,
		workflowRuns:        runs,
		toolCallback:        log.add,
		agentID:             "coding",
		providerName:        func() string { return "" },
		modelName:           func() string { return "m" },
	}
	if srv != nil {
		url := srv.url
		rt.providerMgr = srv.pm
		rt.providerName = func() string { return url }
	}
	return rt, log
}

var checkpointRunIDRe = regexp.MustCompile(`run_id="([^"]+)"`)

func checkpointRunID(t *testing.T, out string) string {
	t.Helper()
	m := checkpointRunIDRe.FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("no run id in:\n%s", out)
	}
	return m[1]
}

func callWorkflow(t *testing.T, ctx context.Context, rt *toolRuntime, args map[string]any) (string, error) {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	return rt.runWorkflow(ctx, raw)
}

// waitFor polls cond for up to two seconds.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

const checkpointScript = `export const meta = {name: 'triage', description: 'find, ask, fix', phases: [{title: 'Find'}, {title: 'Fix'}]}
phase('Find')
const found = await agent('find the bugs', {label: 'find'})
const answer = await checkpoint('Fix these?', {found})
if (!answer || !answer.fix) return {skipped: true}
phase('Fix', {detail: 'apply the approved fixes'})
const fixed = await agent('fix: ' + found, {label: 'fix'})
return {fixed, note: answer.note}`

// The whole orchestrator-in-the-loop round trip through the real tool path:
// the first call returns the checkpoint, a call from a later turn (a new
// runtime sharing the host's registry) answers it, and the script picks the
// reply up as checkpoint()'s value.
func TestWorkflowCheckpointContinueRoundTrip(t *testing.T) {
	srv := newSubagentServer(t, func(task string) string {
		if strings.Contains(task, "fix:") {
			return "patched"
		}
		return "bug-1"
	})
	cwd := t.TempDir()
	runs := NewWorkflowRuns()
	turn1, log1 := workflowTestRuntime(t, cwd, srv, runs)

	out, err := callWorkflow(t, context.Background(), turn1, map[string]any{"script": checkpointScript})
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	for _, want := range []string{"<workflow_checkpoint", `checkpoint_id="cp-1"`, "<message>Fix these?</message>", "bug-1", "continue_run_id", `phase="Find"`} {
		if !strings.Contains(out, want) {
			t.Fatalf("checkpoint result lacks %q:\n%s", want, out)
		}
	}
	runID := checkpointRunID(t, out)

	paused := runs.Paused()
	if len(paused) != 1 || paused[0].RunID != runID || paused[0].CheckpointID != "cp-1" || paused[0].Message != "Fix these?" {
		t.Fatalf("Paused() = %+v", paused)
	}
	if got := log1.find(workflowTraceName, "paused"); len(got) != 1 || got[0]["checkpoint_id"] != "cp-1" || got[0]["run_id"] != runID {
		t.Fatalf("want one paused lifecycle trace, got %v", got)
	}
	var sawCheckpoint bool
	for _, p := range log1.find(workflowProgressTraceName, "") {
		if p["kind"] == "checkpoint" && p["checkpoint_id"] == "cp-1" && p["message"] == "Fix these?" {
			sawCheckpoint = true
		}
	}
	if !sawCheckpoint {
		t.Fatalf("no checkpoint progress trace: %v", log1.find(workflowProgressTraceName, ""))
	}
	firstTurnTraces := len(log1.all())

	// A later turn: a new runtime, the same host registry.
	turn2, log2 := workflowTestRuntime(t, cwd, srv, runs)
	out, err = callWorkflow(t, context.Background(), turn2, map[string]any{
		"continue_run_id": runID, "checkpoint_id": "cp-1",
		"reply": map[string]any{"fix": true, "note": "only the safe ones"},
	})
	if err != nil {
		t.Fatalf("continue: %v", err)
	}
	for _, want := range []string{"<workflow_result", "patched", "only the safe ones", "2 agents"} {
		if !strings.Contains(out, want) {
			t.Fatalf("final result lacks %q:\n%s", want, out)
		}
	}
	if n := len(log1.all()); n != firstTurnTraces {
		t.Fatalf("the first turn's callback was called after its call returned (%d → %d traces)", firstTurnTraces, n)
	}
	resumed := log2.find(workflowTraceName, "running")
	if len(resumed) != 1 || resumed[0]["resumed"] != true || resumed[0]["run_id"] != runID {
		t.Fatalf("want one resumed running trace on the continuing turn, got %v", resumed)
	}
	if got := log2.find(workflowTraceName, "success"); len(got) != 1 {
		t.Fatalf("want the finish trace on the continuing turn, got %v", got)
	}
	var fixPhase map[string]any
	for _, p := range log2.find(workflowProgressTraceName, "") {
		if p["kind"] == "phase" && p["phase"] == "Fix" {
			fixPhase = p
		}
	}
	if fixPhase == nil || fixPhase["detail"] != "apply the approved fixes" || fixPhase["dynamic"] != false {
		t.Fatalf("phase trace should carry detail and dynamic: %v", fixPhase)
	}
	if len(runs.ids()) != 0 || len(runs.Paused()) != 0 {
		t.Fatalf("a settled run must leave the registry: %v", runs.ids())
	}
	// The transcript is complete: result written, journal holding the answer.
	dir := turn1.workflowRunDir(runID)
	if _, err := os.Stat(filepath.Join(dir, "result.json")); err != nil {
		t.Fatalf("result.json: %v", err)
	}
	journal, err := os.ReadFile(filepath.Join(dir, "journal.jsonl"))
	if err != nil || !strings.Contains(string(journal), `"kind":"checkpoint"`) {
		t.Fatalf("journal should record the answered checkpoint: %v\n%s", err, journal)
	}

	// Continuing a run that has finished is an error naming the way back.
	_, err = callWorkflow(t, context.Background(), turn2, map[string]any{"continue_run_id": runID, "reply": 1})
	if err == nil || !strings.Contains(err.Error(), "resume_from_run_id") || !strings.Contains(err.Error(), dir) {
		t.Fatalf("want a resume hint for a finished run, got %v", err)
	}
}

func TestWorkflowContinueStop(t *testing.T) {
	srv := newSubagentServer(t, func(string) string { return "bug-1" })
	runs := NewWorkflowRuns()
	rt, log := workflowTestRuntime(t, t.TempDir(), srv, runs)
	out, err := callWorkflow(t, context.Background(), rt, map[string]any{"script": checkpointScript})
	if err != nil {
		t.Fatal(err)
	}
	runID := checkpointRunID(t, out)

	out, err = callWorkflow(t, context.Background(), rt, map[string]any{"continue_run_id": runID, "stop": true})
	if err != nil {
		t.Fatalf("stop: %v", err)
	}
	if !strings.Contains(out, "stopped at your request") || !strings.Contains(out, "1 agents") {
		t.Fatalf("stop result should report the partial run:\n%s", out)
	}
	if len(runs.ids()) != 0 {
		t.Fatalf("a stopped run must leave the registry: %v", runs.ids())
	}
	if srv.hits.Load() != 1 {
		t.Fatalf("no agent may run after the stop, got %d requests", srv.hits.Load())
	}
	fin := log.find(workflowTraceName, "error")
	if len(fin) != 1 {
		t.Fatalf("want one finish trace, got %v", log.find(workflowTraceName, ""))
	}
	var finOut string
	for _, tr := range log.all() {
		if tr.Name == workflowTraceName && tr.Status == "error" {
			finOut = tr.Output
		}
	}
	if !strings.Contains(finOut, "stopped by the orchestrator") {
		t.Fatalf("finish trace should say who stopped it, got %q", finOut)
	}
}

// An automatic phase-boundary checkpoint is answered by the host: a
// {"stop": true} reply ends the run, anything else continues.
func TestWorkflowAutoCheckpointStopReply(t *testing.T) {
	script := `export const meta = {name: 'phased', description: 'two phases'}
phase('One')
await agent('first', {label: 'a'})
phase('Two')
await agent('second', {label: 'b'})
return 'done'`
	srv := newSubagentServer(t, func(string) string { return "ok" })
	runs := NewWorkflowRuns()
	rt, _ := workflowTestRuntime(t, t.TempDir(), srv, runs)

	out, err := callWorkflow(t, context.Background(), rt, map[string]any{"script": script, "auto_checkpoint": true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `auto="true"`) || !strings.Contains(out, "phase One finished; next: Two") {
		t.Fatalf("want an automatic checkpoint:\n%s", out)
	}
	runID := checkpointRunID(t, out)
	out, err = callWorkflow(t, context.Background(), rt, map[string]any{"continue_run_id": runID, "reply": map[string]any{"stop": true}})
	if err != nil || !strings.Contains(out, "stopped at your request") {
		t.Fatalf("a stop reply to an automatic checkpoint must stop the run: %v\n%s", err, out)
	}
	if srv.hits.Load() != 1 {
		t.Fatalf("the second phase must not run, got %d requests", srv.hits.Load())
	}
}

func TestWorkflowContinueRejectsWrongCheckpointAndUnknownRun(t *testing.T) {
	srv := newSubagentServer(t, func(string) string { return "bug-1" })
	runs := NewWorkflowRuns()
	rt, _ := workflowTestRuntime(t, t.TempDir(), srv, runs)
	out, err := callWorkflow(t, context.Background(), rt, map[string]any{"script": checkpointScript})
	if err != nil {
		t.Fatal(err)
	}
	runID := checkpointRunID(t, out)

	_, err = callWorkflow(t, context.Background(), rt, map[string]any{"continue_run_id": runID, "checkpoint_id": "cp-7", "reply": true})
	if err == nil || !strings.Contains(err.Error(), "paused at cp-1") {
		t.Fatalf("want a checkpoint mismatch error, got %v", err)
	}
	// The mismatch left the run paused and continuable.
	if p := runs.Paused(); len(p) != 1 {
		t.Fatalf("the run should still be paused: %+v", p)
	}

	_, err = callWorkflow(t, context.Background(), rt, map[string]any{"continue_run_id": "wf_nope"})
	if err == nil || !strings.Contains(err.Error(), "not paused at a checkpoint") || !strings.Contains(err.Error(), runID) {
		t.Fatalf("an unknown run id should list the live runs, got %v", err)
	}
	if strings.Contains(err.Error(), "resume_from_run_id") {
		t.Fatalf("a run with no journal has nothing to resume from: %v", err)
	}

	// Continue-only arguments without a run to continue are a mistake.
	_, err = callWorkflow(t, context.Background(), rt, map[string]any{"script": checkpointScript, "stop": true})
	if err == nil || !strings.Contains(err.Error(), "only apply with continue_run_id") {
		t.Fatalf("want a misuse error, got %v", err)
	}
	runs.StopAll()
}

// Esc must still stop a workflow: cancelling the tool call while the run is
// running (not paused) stops the run instead of leaving it detached.
func TestWorkflowToolCancelStopsRunningRun(t *testing.T) {
	srv := newSubagentServer(t, nil) // members never answer
	runs := NewWorkflowRuns()
	rt, log := workflowTestRuntime(t, t.TempDir(), srv, runs)
	script := `export const meta = {name: 'slow', description: 'one slow agent'}
return await agent('take forever', {label: 'slow'})`

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := callWorkflow(t, ctx, rt, map[string]any{"script": script})
		done <- err
	}()
	select {
	case <-srv.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the agent never started")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("want the cancellation, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancelling the tool call did not stop the run")
	}
	if len(runs.ids()) != 0 {
		t.Fatalf("a cancelled run must not stay live: %v", runs.ids())
	}
	if got := log.find(workflowTraceName, "error"); len(got) != 1 {
		t.Fatalf("want an error finish trace, got %v", log.find(workflowTraceName, ""))
	}
}

// Only a paused run outlives its tool call: the call's context ending (the
// tool deadline's defer cancel, an ACP turn finishing) must not kill it.
func TestWorkflowPausedRunSurvivesToolContext(t *testing.T) {
	srv := newSubagentServer(t, func(task string) string {
		if strings.Contains(task, "fix:") {
			return "patched"
		}
		return "bug-1"
	})
	runs := NewWorkflowRuns()
	rt, _ := workflowTestRuntime(t, t.TempDir(), srv, runs)
	ctx, cancel := context.WithCancel(context.Background())
	out, err := callWorkflow(t, ctx, rt, map[string]any{"script": checkpointScript})
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	runID := checkpointRunID(t, out)
	time.Sleep(20 * time.Millisecond)
	if p := runs.Paused(); len(p) != 1 {
		t.Fatalf("the paused run died with its tool call: %+v", p)
	}
	out, err = callWorkflow(t, context.Background(), rt, map[string]any{"continue_run_id": runID, "reply": map[string]any{"fix": true}})
	if err != nil || !strings.Contains(out, "patched") {
		t.Fatalf("continue after the first call's context ended: %v\n%s", err, out)
	}
}

func TestWorkflowIdleReaperStopsAbandonedRun(t *testing.T) {
	saved := workflowIdleTimeout
	workflowIdleTimeout = 30 * time.Millisecond
	t.Cleanup(func() { workflowIdleTimeout = saved })

	srv := newSubagentServer(t, func(string) string { return "bug-1" })
	runs := NewWorkflowRuns()
	rt, _ := workflowTestRuntime(t, t.TempDir(), srv, runs)
	out, err := callWorkflow(t, context.Background(), rt, map[string]any{"script": checkpointScript})
	if err != nil {
		t.Fatal(err)
	}
	runID := checkpointRunID(t, out)
	waitFor(t, "the reaper", func() bool { return len(runs.ids()) == 0 })

	_, err = callWorkflow(t, context.Background(), rt, map[string]any{"continue_run_id": runID, "reply": true})
	if err == nil || !strings.Contains(err.Error(), "paused at a checkpoint with no continue") ||
		!strings.Contains(err.Error(), "resume_from_run_id") {
		t.Fatalf("a reaped run should say why it is gone and how to resume, got %v", err)
	}
	// The journal is kept and closed: the run wrote its result.
	if _, err := os.Stat(filepath.Join(rt.workflowRunDir(runID), "result.json")); err != nil {
		t.Fatalf("a reaped run must still finalize its transcript: %v", err)
	}
}

// A turn-local registry (no host one) is stopped when the turn's tool loop
// returns; StopAll also finalizes what it stops.
func TestWorkflowRunsStopAllFinalizes(t *testing.T) {
	srv := newSubagentServer(t, func(string) string { return "bug-1" })
	runs := NewWorkflowRuns()
	rt, _ := workflowTestRuntime(t, t.TempDir(), srv, runs)
	out, err := callWorkflow(t, context.Background(), rt, map[string]any{"script": checkpointScript})
	if err != nil {
		t.Fatal(err)
	}
	runID := checkpointRunID(t, out)
	runs.StopAll()
	if len(runs.ids()) != 0 || len(runs.Paused()) != 0 {
		t.Fatalf("StopAll left runs behind: %v", runs.ids())
	}
	if _, err := os.Stat(filepath.Join(rt.workflowRunDir(runID), "result.json")); err != nil {
		t.Fatalf("StopAll must wait for the run to finalize: %v", err)
	}
}

func TestRunToolLoopStopsTurnLocalWorkflowRuns(t *testing.T) {
	script := "export const meta = {name: 'ask', description: 'asks once'}\nreturn await checkpoint('go on?')"
	toolArgs, _ := json.Marshal(map[string]any{"script": script})
	manifest := config.DefaultAgentManifest()
	turn := func(runs *WorkflowRuns) (string, *loopServer) {
		pm, url, ls := newLoopServer(t,
			loopReply{toolName: workflowToolID, toolArgs: string(toolArgs)},
			loopReply{content: "done"})
		cfg := loopCfg(t, pm, url)
		cfg.AllowedTools = append(cfg.AllowedTools, workflowToolID)
		cfg.Manifest = &manifest
		cfg.WorkflowPreapproved = true
		cfg.WorkflowRuns = runs
		if _, err := runToolLoop(context.Background(), cfg); err != nil {
			t.Fatal(err)
		}
		return cfg.CWD, ls
	}

	// No host registry: the paused run belongs to the turn and is stopped —
	// and its transcript finalized — when the turn's loop returns.
	cwd, ls := turn(nil)
	if body, _ := json.Marshal(ls.requests()[1]); !strings.Contains(string(body), "workflow_checkpoint") {
		t.Fatalf("the checkpoint should reach the model as the tool result: %s", body)
	}
	results, _ := filepath.Glob(filepath.Join(cwd, ".spettro", "workflow-runs", "*", "result.json"))
	if len(results) != 1 {
		t.Fatalf("the turn-local run was not stopped and finalized at turn end: %v", results)
	}

	// A host registry outlives the turn: the run is still paused after it.
	runs := NewWorkflowRuns()
	turn(runs)
	if p := runs.Paused(); len(p) != 1 || p[0].Message != "go on?" {
		t.Fatalf("a host-owned registry must keep the paused run: %+v", p)
	}
	runs.StopAll()
}

// A "+500k" directive is a pool shared by the turn's runs: each new run
// defaults to what the earlier ones left; an explicit budget_tokens wins.
func TestWorkflowBudgetPoolIsSharedAcrossTurnRuns(t *testing.T) {
	srv := newSubagentServer(t, func(string) string { return "ok" })
	rt, log := workflowTestRuntime(t, t.TempDir(), srv, NewWorkflowRuns())
	rt.workflowBudget = 1000
	script := `export const meta = {name: 'spend', description: 'two agents'}
await agent('a', {label: 'a'})
await agent('b', {label: 'b'})
return budget.total`

	out, err := callWorkflow(t, context.Background(), rt, map[string]any{"script": script})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "<returned>\n1000\n") {
		t.Fatalf("the first run should get the whole pool:\n%s", out)
	}
	spent := 0
	rt.workflowMu.Lock()
	for _, run := range rt.workflowTurnRuns {
		spent += run.handle.Snapshot().Tokens
	}
	rt.workflowMu.Unlock()
	if spent <= 0 {
		t.Fatal("the run spent no tokens; the test cannot tell a shared pool from a fresh one")
	}
	if got, want := rt.workflowBudgetLeft(), 1000-spent; got != want {
		t.Fatalf("budget left = %d, want %d", got, want)
	}
	out, err = callWorkflow(t, context.Background(), rt, map[string]any{"script": script})
	if err != nil {
		t.Fatal(err)
	}
	if want := fmt.Sprintf("<returned>\n%d\n", 1000-spent); !strings.Contains(out, want) {
		t.Fatalf("the second run should get what the first left (%d):\n%s", 1000-spent, out)
	}
	starts := log.find(workflowTraceName, "running")
	if len(starts) != 2 || starts[1]["budget_tokens"] != float64(1000-spent) {
		t.Fatalf("the start trace should carry the run's budget: %v", starts)
	}
	out, err = callWorkflow(t, context.Background(), rt, map[string]any{"script": script, "budget_tokens": 77})
	if err != nil || !strings.Contains(out, "<returned>\n77\n") {
		t.Fatalf("an explicit budget_tokens overrides the pool: %v\n%s", err, out)
	}

	// A spent pool never reads as "no budget".
	rt.workflowBudget = 1
	if got := rt.workflowBudgetLeft(); got != 1 {
		t.Fatalf("a spent pool = %d, want 1", got)
	}
	rt.workflowBudget = 0
	if got := rt.workflowBudgetLeft(); got != 0 {
		t.Fatalf("no directive = %d, want 0", got)
	}
}

func TestWorkflowSizeTierSelection(t *testing.T) {
	cases := []struct {
		arg, configured, want string
		wantErr               bool
	}{
		{"", "", "medium", false},
		{"", "large", "large", false},
		{"", "bogus", "medium", false},
		{"small", "large", "small", false},
		{" Unbounded ", "", "unbounded", false},
		{"huge", "", "", true},
	}
	for _, c := range cases {
		got, err := workflowSizeTier(c.arg, c.configured)
		if (err != nil) != c.wantErr || got != c.want {
			t.Errorf("workflowSizeTier(%q, %q) = %q, %v; want %q (error %v)", c.arg, c.configured, got, err, c.want, c.wantErr)
		}
	}
}

// The size tier reaches the script (size global) and the hosts (start trace).
func TestWorkflowSizeReachesScriptAndTrace(t *testing.T) {
	rt, log := workflowTestRuntime(t, t.TempDir(), nil, NewWorkflowRuns())
	rt.workflowSize = "large"
	script := `export const meta = {name: 'sized', description: 'reads size'}
return size.tier + ':' + size.agents + ':' + size.fanout`

	out, err := callWorkflow(t, context.Background(), rt, map[string]any{"script": script})
	if err != nil || !strings.Contains(out, "large:30:16") {
		t.Fatalf("configured tier: %v\n%s", err, out)
	}
	out, err = callWorkflow(t, context.Background(), rt, map[string]any{"script": script, "size": "small"})
	if err != nil || !strings.Contains(out, "small:5:3") {
		t.Fatalf("per-call size: %v\n%s", err, out)
	}
	starts := log.find(workflowTraceName, "running")
	if len(starts) != 2 || starts[1]["size"] != "small" || starts[1]["size_agents"] != float64(5) {
		t.Fatalf("start trace should carry the size: %v", starts)
	}
	if _, err := callWorkflow(t, context.Background(), rt, map[string]any{"script": script, "size": "huge"}); err == nil {
		t.Fatal("an unknown size must be refused")
	}
}

// Declared params are published to hosts, listed when asking consent, and a
// saved script that can never vary gets a lint note.
func TestWorkflowParamsTraceConsentAndSaveLint(t *testing.T) {
	cwd := t.TempDir()
	rt, log := workflowTestRuntime(t, cwd, nil, NewWorkflowRuns())
	script := `export const meta = {name: 'greet', description: 'says hi', params: {who: {type: 'string', required: true, description: 'whom to greet'}}}
return 'hi ' + args.who`
	out, err := callWorkflow(t, context.Background(), rt, map[string]any{"script": script, "args": map[string]any{"who": "bob"}, "save_as": "greet"})
	if err != nil || !strings.Contains(out, "hi bob") {
		t.Fatalf("param run: %v\n%s", err, out)
	}
	if strings.Contains(out, "Saved without meta.params") {
		t.Fatalf("a parameterised template must not be linted:\n%s", out)
	}
	start := log.find(workflowTraceName, "running")[0]
	params, _ := start["params"].([]any)
	p0, _ := params[0].(map[string]any)
	if len(params) != 1 || p0["name"] != "who" || p0["type"] != "string" || p0["required"] != true || p0["description"] != "whom to greet" {
		t.Fatalf("start trace params = %v", start["params"])
	}

	fixed := `export const meta = {name: 'fixed', description: 'always the same'}
return 'same'`
	out, err = callWorkflow(t, context.Background(), rt, map[string]any{"script": fixed, "save_as": "fixed"})
	if err != nil || !strings.Contains(out, "Saved without meta.params") {
		t.Fatalf("a fixed saved script should get the lint note: %v\n%s", err, out)
	}

	var asked AskUserForm
	consent := &toolRuntime{askUser: func(_ context.Context, f AskUserForm) ([]AskUserAnswer, error) {
		asked = f
		return []AskUserAnswer{{Header: "Workflow", Selected: []string{"Run it"}}}, nil
	}}
	meta, err := workflow.ParseMeta(script)
	if err != nil {
		t.Fatal(err)
	}
	consent.confirmWorkflow(context.Background(), meta, workflowArgs{})
	if !strings.Contains(asked.Context, "Params: who (string, required) — whom to greet") {
		t.Fatalf("consent detail should list params: %q", asked.Context)
	}
}

func TestRenderWorkflowCheckpoint(t *testing.T) {
	logs := make([]string, 30)
	for i := range logs {
		logs[i] = fmt.Sprintf("line %d", i)
	}
	out := renderWorkflowCheckpoint("wf_1", workflow.Meta{Name: "audit"}, workflow.Checkpoint{
		ID: "cp-2", Message: "Which to fix?", Phase: "Verify",
		Data: map[string]any{"confirmed": []any{"a"}},
	}, workflow.Result{Agents: 4, Failed: 1, Cached: 2, Tokens: 900, Phases: []string{"Find", "Verify"}, Logs: logs})
	for _, want := range []string{
		`<workflow_checkpoint name="audit" run_id="wf_1" checkpoint_id="cp-2" phase="Verify">`,
		"<message>Which to fix?</message>",
		`"confirmed"`,
		"<progress>4 agents · 1 failed · 2 replayed · 900 tokens; phases: Find → Verify</progress>",
		"line 29",
		`{"continue_run_id":"wf_1","reply":<value>}`,
		`{"continue_run_id":"wf_1","stop":true}`,
		"30 minutes",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("checkpoint render lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "line 9\n") {
		t.Fatalf("only the latest %d log lines belong in a checkpoint:\n%s", workflowCheckpointLogLines, out)
	}

	auto := renderWorkflowCheckpoint("wf_1", workflow.Meta{Name: "audit"}, workflow.Checkpoint{
		ID: "cp-1", Message: "phase Find finished; next: Verify", Auto: true,
	}, workflow.Result{})
	if !strings.Contains(auto, `auto="true"`) || strings.Contains(auto, "<data>") || strings.Contains(auto, "; phases:") {
		t.Fatalf("auto checkpoint render:\n%s", auto)
	}
}

// Ultracode (keyword or toggle) gets the standing-mode guidance; a request in
// plain English gets the judge-it guidance; the two never mix.
func TestWorkflowGuidanceVariants(t *testing.T) {
	cases := []struct {
		name      string
		agent     LLMAgent
		task      string
		enabled   bool
		ultracode bool
		budget    int
	}{
		{"nothing", LLMAgent{}, "fix the typo", false, false, 0},
		{"keyword", LLMAgent{}, "ultracode: audit the parser", true, true, 0},
		{"toggle", LLMAgent{Ultracode: true}, "audit the parser", true, true, 0},
		{"plain english", LLMAgent{}, "use a workflow to audit the parser", true, false, 0},
		{"budget with workflows", LLMAgent{}, "ultracode audit it +500k", true, true, 500_000},
		{"budget without workflows", LLMAgent{}, "raise the salary +500k", false, false, 0},
		{"budget under the toggle", LLMAgent{Ultracode: true}, "go +1.5m", true, true, 1_500_000},
	}
	for _, c := range cases {
		g := c.agent.workflowGuidanceFor(c.task)
		if g.Enabled != c.enabled || g.Ultracode != c.ultracode || g.BudgetTokens != c.budget {
			t.Errorf("%s: guidance = %+v", c.name, g)
		}
	}

	judge := workflowGuidance{Enabled: true}.prompt()
	ultra := workflowGuidance{Enabled: true, Ultracode: true}.prompt()
	if !strings.Contains(judge, "Availability is not an instruction") || strings.Contains(judge, "ULTRACODE is on") {
		t.Fatalf("judge variant is wrong:\n%s", judge)
	}
	if !strings.Contains(ultra, "ULTRACODE is on") || !strings.Contains(ultra, "every substantive task") {
		t.Fatalf("ultracode variant is wrong:\n%s", ultra)
	}
	for _, contradicts := range []string{"Availability is not an instruction", "asks the user to confirm", "manufacture phases"} {
		if strings.Contains(ultra, contradicts) {
			t.Fatalf("the ultracode variant must not carry the judge policy %q", contradicts)
		}
	}
	// Both share the core: templates, checkpoints and the script API.
	for _, core := range []string{"TEMPLATES", "continue_run_id", "plan(prompt", "untilDry(round", "meta.params", "workflow({script, args})"} {
		if !strings.Contains(judge, core) || !strings.Contains(ultra, core) {
			t.Fatalf("both variants should explain %q", core)
		}
	}
	if !strings.Contains(judge, "Workflow size guideline: medium — keep each workflow under ~10 agents") {
		t.Fatalf("default size line missing:\n%s", judge)
	}
	if got := (workflowGuidance{Enabled: true, SizeTier: "unbounded"}).prompt(); !strings.Contains(got, "no agent guideline") {
		t.Fatalf("unbounded size line missing")
	}
	if strings.Contains(judge, "token budget") {
		t.Fatal("no directive, no budget line")
	}
	if got := (workflowGuidance{Enabled: true, BudgetTokens: 500_000}).prompt(); !strings.Contains(got, "token budget of 500000 tokens") {
		t.Fatalf("budget line missing:\n%s", got)
	}
	if (workflowGuidance{Ultracode: true}).prompt() != "" {
		t.Fatal("a disabled guidance renders nothing")
	}
}

// The variant reaches the real system prompt, and sub-agents never get it.
func TestRunInjectsWorkflowGuidanceVariant(t *testing.T) {
	manifest := config.DefaultAgentManifest()
	spec, ok := manifest.AgentByID("coding")
	if !ok {
		t.Fatal("no coding agent")
	}
	run := func(a LLMAgent, task string) (string, []string) {
		pm, url, ls := newLoopServer(t, loopReply{content: "done"})
		a.Spec = spec
		a.Manifest = &manifest
		a.ProviderManager = pm
		a.ProviderName = func() string { return url }
		a.ModelName = func() string { return "m" }
		a.CWD = t.TempDir()
		if _, err := a.Run(context.Background(), task); err != nil {
			t.Fatal(err)
		}
		body := ls.requests()[0]
		msgs, _ := body["messages"].([]any)
		first, _ := msgs[0].(map[string]any)
		system, _ := first["content"].(string)
		var tools []string
		list, _ := body["tools"].([]any)
		for _, tl := range list {
			fn, _ := tl.(map[string]any)["function"].(map[string]any)
			name, _ := fn["name"].(string)
			tools = append(tools, name)
		}
		return system, tools
	}

	system, tools := run(LLMAgent{Ultracode: true, WorkflowSize: "small"}, "audit the parser")
	if !strings.Contains(system, "ULTRACODE is on") || !contains(tools, workflowToolID) {
		t.Fatalf("the toggle should inject the ultracode variant and the tool (tools %v)", tools)
	}
	if !strings.Contains(system, "Workflow size guideline: small") {
		t.Fatal("the configured tier should reach the size line")
	}

	system, tools = run(LLMAgent{}, "use a workflow to audit the parser")
	if !strings.Contains(system, "Availability is not an instruction") || strings.Contains(system, "ULTRACODE is on") || !contains(tools, workflowToolID) {
		t.Fatal("a plain-English request should inject the judge variant")
	}

	system, tools = run(LLMAgent{Ultracode: true, DelegationDepth: 1}, "ultracode audit the parser")
	if strings.Contains(system, "WORKFLOWS are available") || contains(tools, workflowToolID) {
		t.Fatal("a sub-agent must never get workflow guidance or the tool")
	}
}
