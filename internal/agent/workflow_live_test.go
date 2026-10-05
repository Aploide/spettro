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
	// A deliberate stop is not a failure: hosts get a "stopped" lifecycle
	// status with the reason, never "error".
	if got := log.find(workflowTraceName, "error"); len(got) != 0 {
		t.Fatalf("a stop must not be reported as a failure: %v", got)
	}
	fin := log.find(workflowTraceName, "stopped")
	if len(fin) != 1 || fin[0]["reason"] != "stopped by the orchestrator" || fin[0]["run_id"] != runID || fin[0]["workflow"] != "triage" {
		t.Fatalf("want one stopped finish trace with the reason, got %v", log.find(workflowTraceName, ""))
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
	if body, _ := json.Marshal(ls.requests()[1]); !strings.Contains(string(body), "workflow_checkpoint") || !strings.Contains(string(body), "Answer it in this turn") {
		t.Fatalf("the checkpoint should reach the model as the tool result, saying it dies with the turn: %s", body)
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
	rt.workflowPool = newWorkflowPool(1000)
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
	spent := rt.workflowPool.spent()
	if spent <= 0 {
		t.Fatal("the run spent no tokens; the test cannot tell a shared pool from a fresh one")
	}
	if got, want := rt.workflowPool.left(), 1000-spent; got != want {
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
	if newWorkflowPool(0) != nil {
		t.Fatal("no directive, no pool")
	}
}

// A spent pool stops spending: the agent that would overrun it is refused
// inside the run, and a new run is refused outright rather than started with
// a token budget of 1 — which used to let its whole first fan-out wave
// dispatch before any spend was recorded.
func TestWorkflowSpentPoolRefusesAgentsAndRuns(t *testing.T) {
	srv := newSubagentServer(t, func(string) string { return "ok" })
	rt, log := workflowTestRuntime(t, t.TempDir(), srv, NewWorkflowRuns())
	rt.workflowPool = newWorkflowPool(10)
	// One wave, one agent at a time: every agent() passes the engine's own
	// budget check when the wave is built (nothing is spent yet), so only a
	// check where each agent actually starts can stop the ones queued behind
	// the first — which spends more than the whole pool.
	wave := `export const meta = {name: 'wave', description: 'a fan-out'}
return await parallel([1, 2, 3, 4].map(i => () => agent('item ' + i, {label: 'item'})))`
	out, err := callWorkflow(t, context.Background(), rt, map[string]any{"script": wave, "max_concurrency": 1})
	if err != nil {
		t.Fatal(err)
	}
	if hits := srv.hits.Load(); hits != 1 {
		t.Fatalf("only the agent that started before the pool ran out may run, got %d requests:\n%s", hits, out)
	}
	if !strings.Contains(out, "3 failed") {
		t.Fatalf("the refused agents should resolve to null and count as failed:\n%s", out)
	}
	var said bool
	for _, tr := range log.all() {
		if tr.Name == "agent" && tr.Status == "error" && strings.Contains(tr.Output, "token budget of 10 is spent") {
			said = true
		}
	}
	if !said {
		t.Fatal("a refused agent's trace should say the pool is spent")
	}

	_, err = callWorkflow(t, context.Background(), rt, map[string]any{"script": wave})
	if err == nil || !strings.Contains(err.Error(), "token budget of 10 is spent") || !strings.Contains(err.Error(), "budget_tokens") {
		t.Fatalf("a run started against a spent pool must be refused, got %v", err)
	}
	if hits := srv.hits.Load(); hits != 1 {
		t.Fatalf("a refused run must not dispatch anything, got %d requests", hits)
	}
	// The way out the error names still works.
	if _, err := callWorkflow(t, context.Background(), rt, map[string]any{"script": wave, "budget_tokens": 100000}); err != nil {
		t.Fatalf("an explicit budget_tokens runs past a spent pool: %v", err)
	}
}

// A pooled run paused at a checkpoint, then continued after a later run spent
// the rest of the pool, must not spend the pool a second time: the pool is
// checked when each agent starts, not only when the run started.
func TestWorkflowPoolBindsContinuedRuns(t *testing.T) {
	srv := newSubagentServer(t, func(task string) string {
		if strings.Contains(task, "fix:") {
			return "patched"
		}
		return "bug-1"
	})
	runs := NewWorkflowRuns()
	rt, _ := workflowTestRuntime(t, t.TempDir(), srv, runs)
	rt.workflowPool = newWorkflowPool(100)

	out, err := callWorkflow(t, context.Background(), rt, map[string]any{"script": checkpointScript})
	if err != nil {
		t.Fatal(err)
	}
	pausedRun := checkpointRunID(t, out)
	// A second run in the same turn spends what the first left, and more.
	spend := `export const meta = {name: 'spend', description: 'two agents'}
await agent('a', {label: 'a'})
await agent('b', {label: 'b'})
return 'spent'`
	if _, err := callWorkflow(t, context.Background(), rt, map[string]any{"script": spend}); err != nil {
		t.Fatal(err)
	}
	if left := rt.workflowPool.left(); left > 0 {
		t.Fatalf("the test needs a spent pool, %d left", left)
	}
	before := srv.hits.Load()

	// Continued from a later turn: the run stays bound to the pool it started
	// under, so its fix agent is refused.
	later, _ := workflowTestRuntime(t, rt.cwd, srv, runs)
	out, err = callWorkflow(t, context.Background(), later, map[string]any{"continue_run_id": pausedRun, "reply": map[string]any{"fix": true}})
	if err != nil {
		t.Fatal(err)
	}
	if hits := srv.hits.Load(); hits != before {
		t.Fatalf("a continued run spent past the pool: %d → %d requests\n%s", before, hits, out)
	}
	if !strings.Contains(out, "1 failed") || strings.Contains(out, "patched") {
		t.Fatalf("the refused fix agent should resolve to null:\n%s", out)
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
	}, workflow.Result{Agents: 4, Failed: 1, Cached: 2, Tokens: 900, Phases: []string{"Find", "Verify"}, Logs: logs}, false)
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
	}, workflow.Result{}, false)
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
		g := c.agent.workflowGuidanceFor(c.task, nil)
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

// stoppedCalls records SetOnStopped notifications.
type stoppedCalls struct {
	ch chan [3]string
}

func watchStopped(runs *WorkflowRuns) *stoppedCalls {
	s := &stoppedCalls{ch: make(chan [3]string, 8)}
	runs.SetOnStopped(func(runID, name, reason string) {
		// The run must already be gone when hosts hear about it, so a host
		// redrawing from Paused() in the callback does not see it again.
		if len(runs.Paused()) != 0 {
			reason = "STILL LISTED: " + reason
		}
		s.ch <- [3]string{runID, name, reason}
	})
	return s
}

func (s *stoppedCalls) next(t *testing.T) [3]string {
	t.Helper()
	select {
	case got := <-s.ch:
		return got
	case <-time.After(5 * time.Second):
		t.Fatal("SetOnStopped was not called")
	}
	return [3]string{}
}

func (s *stoppedCalls) none(t *testing.T) {
	t.Helper()
	select {
	case got := <-s.ch:
		t.Fatalf("SetOnStopped called for a run a tool call was driving: %v", got)
	case <-time.After(100 * time.Millisecond):
	}
}

// A paused run stopped by the host has no turn to hear its finish trace, so
// the host is told through SetOnStopped — after the run has finalized.
func TestWorkflowStopAllNotifiesDetachedRuns(t *testing.T) {
	srv := newSubagentServer(t, func(string) string { return "bug-1" })
	runs := NewWorkflowRuns()
	stopped := watchStopped(runs)
	rt, log := workflowTestRuntime(t, t.TempDir(), srv, runs)
	out, err := callWorkflow(t, context.Background(), rt, map[string]any{"script": checkpointScript})
	if err != nil {
		t.Fatal(err)
	}
	runID := checkpointRunID(t, out)
	tracesBefore := len(log.all())

	runs.StopAll()
	if got := stopped.next(t); got != [3]string{runID, "triage", "the session ended"} {
		t.Fatalf("SetOnStopped got %v", got)
	}
	if n := len(log.all()); n != tracesBefore {
		t.Fatalf("the finished turn's callback was called for a detached run (%d → %d traces)", tracesBefore, n)
	}
}

func TestWorkflowIdleReaperNotifiesHost(t *testing.T) {
	saved := workflowIdleTimeout
	workflowIdleTimeout = 30 * time.Millisecond
	t.Cleanup(func() { workflowIdleTimeout = saved })

	srv := newSubagentServer(t, func(string) string { return "bug-1" })
	runs := NewWorkflowRuns()
	stopped := watchStopped(runs)
	rt, _ := workflowTestRuntime(t, t.TempDir(), srv, runs)
	out, err := callWorkflow(t, context.Background(), rt, map[string]any{"script": checkpointScript})
	if err != nil {
		t.Fatal(err)
	}
	runID := checkpointRunID(t, out)
	got := stopped.next(t)
	if got[0] != runID || got[1] != "triage" || got[2] != "paused for over 30ms with no continue" {
		t.Fatalf("SetOnStopped got %v", got)
	}
}

// A run a tool call is driving when the host stops it reports through its
// finish trace — "stopped", with the reason — and not through SetOnStopped.
func TestWorkflowStopAllWhileDrivenTracesStopped(t *testing.T) {
	srv := newSubagentServer(t, nil) // members never answer
	runs := NewWorkflowRuns()
	stopped := watchStopped(runs)
	rt, log := workflowTestRuntime(t, t.TempDir(), srv, runs)
	script := `export const meta = {name: 'slow', description: 'one slow agent'}
return await agent('take forever', {label: 'slow'})`
	done := make(chan error, 1)
	go func() {
		_, err := callWorkflow(t, context.Background(), rt, map[string]any{"script": script})
		done <- err
	}()
	select {
	case <-srv.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the agent never started")
	}
	runs.StopAll()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "was stopped (the session ended)") {
			t.Fatalf("the driving call should say the run was stopped, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("StopAll did not stop the driven run")
	}
	fin := log.find(workflowTraceName, "stopped")
	if len(fin) != 1 || fin[0]["reason"] != "the session ended" || fin[0]["workflow"] != "slow" {
		t.Fatalf("want a stopped finish trace, got %v", log.find(workflowTraceName, ""))
	}
	if got := log.find(workflowTraceName, "error"); len(got) != 0 {
		t.Fatalf("a host stop is not a failure: %v", got)
	}
	stopped.none(t)
}

// The checkpoint result must not promise a turn-local run half an hour: the
// run dies with the turn, so the model has to answer it before ending it.
func TestRenderWorkflowCheckpointTurnLocal(t *testing.T) {
	cp := workflow.Checkpoint{ID: "cp-1", Message: "go on?"}
	local := renderWorkflowCheckpoint("wf_1", workflow.Meta{Name: "x"}, cp, workflow.Result{}, true)
	if strings.Contains(local, "30 minutes") || !strings.Contains(local, "Answer it in this turn") || !strings.Contains(local, "stopped when your turn ends") {
		t.Fatalf("turn-local checkpoint text:\n%s", local)
	}
	hosted := renderWorkflowCheckpoint("wf_1", workflow.Meta{Name: "x"}, cp, workflow.Result{}, false)
	if !strings.Contains(hosted, "30 minutes") || strings.Contains(hosted, "Answer it in this turn") {
		t.Fatalf("host-owned checkpoint text:\n%s", hosted)
	}
}

// pausedRunAgent runs one LLMAgent turn of the stock agent id against
// scripted replies and returns the requests the model saw.
func runAgentTurn(t *testing.T, a LLMAgent, agentID, cwd, task string, replies ...loopReply) []map[string]any {
	t.Helper()
	manifest := config.DefaultAgentManifest()
	spec, ok := manifest.AgentByID(agentID)
	if !ok {
		t.Fatalf("no %s agent", agentID)
	}
	pm, url, ls := newLoopServer(t, replies...)
	a.Spec = spec
	a.Manifest = &manifest
	a.ProviderManager = pm
	a.ProviderName = func() string { return url }
	a.ModelName = func() string { return "m" }
	a.CWD = cwd
	if _, err := a.Run(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	return ls.requests()
}

func requestSystemAndTools(body map[string]any) (string, []string) {
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

// The cross-turn answer the orchestrator-in-the-loop design hinges on: a run
// paused in a keyword turn is continued in a later turn whose message ("yes,
// fix them") carries no keyword, with the toggle off. The paused run alone
// brings the tool back, and the prompt names it; sub-agents never get it.
func TestPausedRunIsContinuableOnAKeywordFreeTurn(t *testing.T) {
	cwd := t.TempDir()
	runs := NewWorkflowRuns()
	t.Cleanup(runs.StopAll)
	script := "export const meta = {name: 'ask', description: 'asks once'}\nconst a = await checkpoint('Fix these?', {found: ['bug-1']})\nreturn {answer: a}"
	start, _ := json.Marshal(map[string]any{"script": script})

	runAgentTurn(t, LLMAgent{WorkflowRuns: runs}, "coding", cwd, "ultracode: triage the parser",
		loopReply{toolName: workflowToolID, toolArgs: string(start)},
		loopReply{content: "Found bug-1. Fix it?"})
	paused := runs.Paused()
	if len(paused) != 1 {
		t.Fatalf("the first turn should leave the run paused: %+v", paused)
	}
	runID := paused[0].RunID

	cont, _ := json.Marshal(map[string]any{"continue_run_id": runID, "reply": map[string]any{"fix": true}})
	reqs := runAgentTurn(t, LLMAgent{WorkflowRuns: runs}, "coding", cwd, "yes, fix them",
		loopReply{toolName: workflowToolID, toolArgs: string(cont)},
		loopReply{content: "done"})
	system, tools := requestSystemAndTools(reqs[0])
	if !contains(tools, workflowToolID) {
		t.Fatalf("a paused run must bring the workflow tool back on a keyword-free turn (tools %v)", tools)
	}
	for _, want := range []string{"run_id " + runID, "checkpoint_id cp-1", "Fix these?", "paused at a checkpoint, waiting for you", "Availability is not an instruction"} {
		if !strings.Contains(system, want) {
			t.Fatalf("the prompt should carry %q:\n%s", want, system)
		}
	}
	if strings.Contains(system, "ULTRACODE is on") || strings.Contains(system, "the user asked for one in their own words") {
		t.Fatal("a paused run alone gets the judge variant, without claiming the user asked")
	}
	result, _ := json.Marshal(reqs[1])
	if !strings.Contains(string(result), "workflow_result") || !strings.Contains(string(result), `\"fix\": true`) {
		t.Fatalf("the continue should reach the run and finish it: %s", result)
	}
	if p := runs.Paused(); len(p) != 0 {
		t.Fatalf("the continued run should have settled: %+v", p)
	}

	// Nothing paused any more: the next keyword-free turn is back to normal.
	reqs = runAgentTurn(t, LLMAgent{WorkflowRuns: runs}, "coding", cwd, "thanks", loopReply{content: "ok"})
	if _, tools := requestSystemAndTools(reqs[0]); contains(tools, workflowToolID) {
		t.Fatal("with no paused run and no request, the tool stays off")
	}
}

func TestPausedRunNeverGrantsSubagentsTheTool(t *testing.T) {
	srv := newSubagentServer(t, func(string) string { return "bug-1" })
	runs := NewWorkflowRuns()
	t.Cleanup(runs.StopAll)
	rt, _ := workflowTestRuntime(t, t.TempDir(), srv, runs)
	if _, err := callWorkflow(t, context.Background(), rt, map[string]any{"script": checkpointScript}); err != nil {
		t.Fatal(err)
	}
	if len(runs.Paused()) != 1 {
		t.Fatal("setup: no paused run")
	}
	reqs := runAgentTurn(t, LLMAgent{WorkflowRuns: runs, DelegationDepth: 1}, "coding", t.TempDir(), "yes, fix them", loopReply{content: "ok"})
	system, tools := requestSystemAndTools(reqs[0])
	if contains(tools, workflowToolID) || strings.Contains(system, "WORKFLOWS are available") || strings.Contains(system, "paused at a checkpoint") {
		t.Fatal("a sub-agent must never get the workflow tool or guidance, paused runs or not")
	}
}

// The ultracode toggle reaches every agent the user talks to; an agent that
// cannot change files is pointed at research and review, and one that cannot
// read is not told to scout inline.
func TestUltracodeGuidanceFollowsTheAgentsRole(t *testing.T) {
	cwd := t.TempDir()
	prompt := func(agentID string) string {
		reqs := runAgentTurn(t, LLMAgent{Ultracode: true}, agentID, cwd, "look into the parser", loopReply{content: "ok"})
		system, tools := requestSystemAndTools(reqs[0])
		if !contains(tools, workflowToolID) {
			t.Fatalf("%s: the toggle should grant the tool", agentID)
		}
		return system
	}
	coding := prompt("coding")
	if !strings.Contains(coding, "understand → design → implement → review") || !strings.Contains(coding, "Scout inline first") {
		t.Fatal("the coding agent keeps the implement guidance")
	}
	for _, id := range []string{"plan", "ask"} {
		system := prompt(id)
		if strings.Contains(system, "implement → review") || !strings.Contains(system, "never to implement") || !strings.Contains(system, "must not modify anything") {
			t.Fatalf("%s: ultracode must not tell a non-implementing agent to implement through workflows", id)
		}
	}
	if plan := prompt("plan"); strings.Contains(plan, "Scout inline first") || !strings.Contains(plan, "no file tools of your own") {
		t.Fatal("the planner has no read tools and must not be told to scout inline")
	}
	if ask := prompt("ask"); !strings.Contains(ask, "Scout inline first") {
		t.Fatal("the ask agent can read and scouts inline")
	}
}

func TestWorkflowGuidanceCheckpointAndTemplateWording(t *testing.T) {
	for _, g := range []workflowGuidance{{Enabled: true}, {Enabled: true, Ultracode: true}} {
		text := g.prompt()
		if strings.Contains(text, "non-interactive hosts") || strings.Contains(text, "resume replays, ") {
			t.Fatal("checkpoint() does not resolve to null on resume or on any host the model runs under")
		}
		if !strings.Contains(text, "replays your earlier reply without pausing") {
			t.Fatal("the prompt should say what a resumed run does with answered checkpoints")
		}
		if strings.Contains(text, "/workflows show") || !strings.Contains(text, `"show": true`) {
			t.Fatal("the model cannot run /workflows show; it reads templates through the tool")
		}
	}
	paused := workflowGuidance{Enabled: true, Paused: []PausedWorkflow{{RunID: "wf_1", Name: "audit", CheckpointID: "cp-2", Message: "Fix\nthese?"}}}.prompt()
	if !strings.Contains(paused, `- run_id wf_1 ("audit") at checkpoint_id cp-2: Fix these?`) {
		t.Fatalf("paused line:\n%s", paused)
	}
}

// A saved template is readable through the tool even where the file tools
// cannot reach it (a global template, outside the workspace), without running
// anything and under any permission.
func TestWorkflowShowReturnsTemplateSource(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cwd := t.TempDir()
	script := `export const meta = {name: 'audit', description: 'audits a package', params: {pkg: {type: 'string', required: true, description: 'package to audit'}}}
return await agent('audit ' + args.pkg)`
	path, err := workflow.Save(cwd, "audit", "global", script)
	if err != nil {
		t.Fatal(err)
	}
	rt, log := workflowTestRuntime(t, cwd, nil, NewWorkflowRuns())
	rt.permission = config.PermissionAskFirst
	if _, _, err := rt.resolvePath(path); err == nil {
		t.Fatal("setup: the global template should be outside the workspace")
	}
	for _, args := range []map[string]any{{"name": "audit", "show": true}, {"script_path": path, "show": true}} {
		out, err := callWorkflow(t, context.Background(), rt, args)
		if err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		for _, want := range []string{"<workflow_template name=\"audit\"", "pkg (string, required)", "agent('audit ' + args.pkg)", "Nothing was run"} {
			if !strings.Contains(out, want) {
				t.Fatalf("%v: show lacks %q:\n%s", args, want, out)
			}
		}
	}
	if len(log.all()) != 0 {
		t.Fatal("show must not start a run")
	}
	if _, err := callWorkflow(t, context.Background(), rt, map[string]any{"script": script, "show": true}); err == nil {
		t.Fatal("show needs a saved script to read")
	}
}
