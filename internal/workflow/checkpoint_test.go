package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// eventLog collects observer events for assertions.
type eventLog struct {
	mu     sync.Mutex
	events []Event
}

func (l *eventLog) observe(ev Event) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, ev)
}

func (l *eventLog) ofKind(kind EventKind) []Event {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []Event
	for _, ev := range l.events {
		if ev.Kind == kind {
			out = append(out, ev)
		}
	}
	return out
}

func startRun(t *testing.T, script string, opts Options) *Handle {
	t.Helper()
	if opts.Runner == nil {
		opts.Runner = echoRunner(0)
	}
	h, err := Start(context.Background(), script, opts)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() {
		h.Stop()
		<-h.Done()
	})
	return h
}

func nextStep(t *testing.T, h *Handle) Step {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	step, err := h.Next(ctx)
	if err != nil {
		t.Fatalf("next: %v", err)
	}
	return step
}

func wantCheckpoint(t *testing.T, step Step) Checkpoint {
	t.Helper()
	if step.Checkpoint == nil {
		if step.Result != nil {
			t.Fatalf("run settled (err %v, value %#v) instead of pausing", step.Err, step.Result.Value)
		}
		t.Fatal("step carries neither a checkpoint nor a result")
	}
	return *step.Checkpoint
}

func wantResult(t *testing.T, step Step) Result {
	t.Helper()
	if step.Result == nil {
		t.Fatalf("run paused at %+v instead of settling", step.Checkpoint)
	}
	if step.Err != nil {
		t.Fatalf("run failed: %v", step.Err)
	}
	return *step.Result
}

func TestCheckpointRoundTripsTheReply(t *testing.T) {
	events := &eventLog{}
	h := startRun(t, header(`
		phase('Work')
		const found = await agent('scan')
		const reply = await checkpoint('which ones should I fix?', {found, count: 2})
		return reply.fix.map(f => f.toUpperCase()).join(',') + '|' + reply.note + '|' + (reply.nested.deep === 1)
	`), Options{Checkpoints: true, Observer: events.observe})

	cp := wantCheckpoint(t, nextStep(t, h))
	if cp.ID != "cp-1" || cp.Message != "which ones should I fix?" || cp.Phase != "Work" || cp.Auto {
		t.Fatalf("checkpoint = %+v", cp)
	}
	data, ok := cp.Data.(map[string]any)
	if !ok || data["found"] != "done:scan" || data["count"] != float64(2) {
		t.Fatalf("data = %#v, want the script's object as JSON-decoded values", cp.Data)
	}
	if pending, ok := h.Pending(); !ok || pending.ID != "cp-1" {
		t.Fatalf("pending = %+v, %v", pending, ok)
	}
	// Asking again while paused returns the same checkpoint.
	if again := wantCheckpoint(t, nextStep(t, h)); again.ID != "cp-1" {
		t.Fatalf("second Next = %+v", again)
	}

	reply := map[string]any{"fix": []any{"a", "b"}, "note": "go", "nested": map[string]any{"deep": 1}}
	if err := h.Resume("cp-1", reply); err != nil {
		t.Fatalf("resume: %v", err)
	}
	res := wantResult(t, nextStep(t, h))
	if res.Value != "A,B|go|true" {
		t.Fatalf("value = %#v", res.Value)
	}

	cps, resumes := events.ofKind(EventCheckpoint), events.ofKind(EventResume)
	if len(cps) != 1 || cps[0].CheckpointID != "cp-1" || !strings.Contains(cps[0].Output, `"found":"done:scan"`) || cps[0].Cached {
		t.Fatalf("checkpoint events = %+v", cps)
	}
	if len(resumes) != 1 || !strings.Contains(resumes[0].Output, `"note":"go"`) {
		t.Fatalf("resume events = %+v", resumes)
	}
}

func TestCheckpointReplyAcceptsRawJSONAndNull(t *testing.T) {
	h := startRun(t, header(`
		const a = await checkpoint('raw')
		const b = await checkpoint('nothing')
		return [a.ok, b === null]
	`), Options{Checkpoints: true})
	wantCheckpoint(t, nextStep(t, h))
	if err := h.Resume("", json.RawMessage(`{"ok": "yes"}`)); err != nil {
		t.Fatalf("resume: %v", err)
	}
	wantCheckpoint(t, nextStep(t, h))
	if err := h.Resume("cp-2", nil); err != nil {
		t.Fatalf("resume: %v", err)
	}
	res := wantResult(t, nextStep(t, h))
	if fmt.Sprint(res.Value) != "[yes true]" {
		t.Fatalf("value = %#v", res.Value)
	}
}

func TestResumeRejectsNoPendingAndStaleIDs(t *testing.T) {
	gate := make(chan struct{})
	runner := &fakeRunner{fn: func(req Request) (Response, error) {
		<-gate
		return Response{Text: "ok"}, nil
	}}
	h := startRun(t, header(`
		await agent('wait')
		return await checkpoint('q')
	`), Options{Checkpoints: true, Runner: runner})
	if err := h.Resume("", "x"); err == nil || !strings.Contains(err.Error(), "no checkpoint is pending") {
		t.Fatalf("want a no-pending error, got %v", err)
	}
	close(gate)
	wantCheckpoint(t, nextStep(t, h))
	if err := h.Resume("cp-9", "x"); err == nil || !strings.Contains(err.Error(), "not the pending one") {
		t.Fatalf("want a stale-id error, got %v", err)
	}
	if err := h.Resume("cp-1", func() {}); err == nil || !strings.Contains(err.Error(), "JSON") {
		t.Fatalf("want an encoding error, got %v", err)
	}
	if err := h.Resume("cp-1", "fine"); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if res := wantResult(t, nextStep(t, h)); res.Value != "fine" {
		t.Fatalf("value = %#v", res.Value)
	}
}

// The quiescence rule: a checkpoint raised while an agent runs is surfaced only
// once that agent has finished, and an agent started after the checkpoint
// waits for the answer instead of running while the orchestrator decides.
func TestCheckpointWaitsForInFlightAgents(t *testing.T) {
	release := make(chan struct{})
	runner := &fakeRunner{fn: func(req Request) (Response, error) {
		if req.Prompt == "slow" {
			<-release
		}
		return Response{Text: "done:" + req.Prompt}, nil
	}}
	h := startRun(t, header(`
		const slow = agent('slow')
		const answer = checkpoint('decide')
		const extra = agent('extra')
		const a = await answer
		return [await slow, await extra, a.choice]
	`), Options{Checkpoints: true, Runner: runner})

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	_, err := h.Next(ctx)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Next surfaced the pause while an agent was still running (err %v)", err)
	}
	if got := runner.count(); got != 1 {
		t.Fatalf("runner calls = %d, want only the slow agent — the later one must wait for the answer", got)
	}

	close(release)
	cp := wantCheckpoint(t, nextStep(t, h))
	if cp.Message != "decide" {
		t.Fatalf("checkpoint = %+v", cp)
	}
	time.Sleep(100 * time.Millisecond)
	if got := runner.count(); got != 1 {
		t.Fatalf("runner calls = %d while paused, want 1", got)
	}
	snap := h.Snapshot()
	if snap.Agents != 2 || snap.Meta.Name != "test-flow" {
		t.Fatalf("snapshot while paused = %+v", snap)
	}

	if err := h.Resume(cp.ID, map[string]any{"choice": "x"}); err != nil {
		t.Fatalf("resume: %v", err)
	}
	res := wantResult(t, nextStep(t, h))
	if fmt.Sprint(res.Value) != "[done:slow done:extra x]" {
		t.Fatalf("value = %#v", res.Value)
	}
}

func TestQueuedCheckpointsSurfaceInOrder(t *testing.T) {
	h := startRun(t, header(`
		const answers = await parallel([
			() => checkpoint('first', {i: 1}),
			() => checkpoint('second', {i: 2}),
			() => checkpoint('third', {i: 3}),
		])
		return answers.join(',')
	`), Options{Checkpoints: true})
	for i, want := range []string{"first", "second", "third"} {
		cp := wantCheckpoint(t, nextStep(t, h))
		if cp.Message != want || cp.ID != fmt.Sprintf("cp-%d", i+1) {
			t.Fatalf("checkpoint %d = %+v, want %q", i, cp, want)
		}
		if err := h.Resume(cp.ID, "a"+want); err != nil {
			t.Fatalf("resume: %v", err)
		}
	}
	res := wantResult(t, nextStep(t, h))
	if res.Value != "afirst,asecond,athird" {
		t.Fatalf("value = %#v", res.Value)
	}
}

func TestStopWhilePaused(t *testing.T) {
	h := startRun(t, header(`
		await agent('one')
		await checkpoint('wait for me')
		await agent('never')
	`), Options{Checkpoints: true})
	wantCheckpoint(t, nextStep(t, h))
	h.Stop()
	step := nextStep(t, h)
	if step.Result == nil || !errors.Is(step.Err, context.Canceled) {
		t.Fatalf("after Stop: result %v, err %v — want the settled result with the cancellation", step.Result, step.Err)
	}
	if step.Result.Agents != 1 {
		t.Fatalf("agents = %d, want 1 (the stopped run never got past the checkpoint)", step.Result.Agents)
	}
	select {
	case <-h.Done():
	default:
		t.Fatal("Done is not closed after the run settled")
	}
	if err := h.Resume("", nil); err == nil {
		t.Fatal("resuming a stopped run must fail")
	}
}

// A pending checkpoint counts as in flight, so a run paused for longer than
// the idle poll is not mistaken for a script awaiting a promise that never
// settles.
func TestPausedRunIsNotADeadlock(t *testing.T) {
	h := startRun(t, header(`return await checkpoint('slow human')`), Options{Checkpoints: true})
	cp := wantCheckpoint(t, nextStep(t, h))
	time.Sleep(10 * idlePoll)
	select {
	case <-h.Done():
		t.Fatalf("the paused run settled on its own: %v", h.Snapshot())
	default:
	}
	if err := h.Resume(cp.ID, "late"); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if res := wantResult(t, nextStep(t, h)); res.Value != "late" {
		t.Fatalf("value = %#v", res.Value)
	}
}

func TestNextWaitIsBoundedByItsOwnContext(t *testing.T) {
	gate := make(chan struct{})
	runner := &fakeRunner{fn: func(req Request) (Response, error) {
		<-gate
		return Response{Text: "ok"}, nil
	}}
	h := startRun(t, header(`return await agent('x')`), Options{Checkpoints: true, Runner: runner})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := h.Next(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the wait's deadline", err)
	}
	// The run itself is untouched by the expired wait.
	close(gate)
	if res := wantResult(t, nextStep(t, h)); res.Value != "ok" {
		t.Fatalf("value = %#v", res.Value)
	}
}

func TestRunSkipsCheckpointsWithoutAnOrchestrator(t *testing.T) {
	events := &eventLog{}
	res := run(t, `
		const r = await checkpoint('anyone there?', {x: 1})
		return r === null
	`, Options{Observer: events.observe})
	if res.Value != true {
		t.Fatalf("checkpoint() under Run must resolve to null, got %#v", res.Value)
	}
	want := "checkpoint skipped (no orchestrator): anyone there?"
	if len(res.Logs) != 1 || res.Logs[0] != want {
		t.Fatalf("logs = %v, want %q", res.Logs, want)
	}
	if len(events.ofKind(EventCheckpoint)) != 0 {
		t.Fatal("a skipped checkpoint must not emit a pause")
	}
}

// Run never pauses even when the caller asked for checkpoints: it has nobody
// to hand the pause to.
func TestRunForcesCheckpointsOff(t *testing.T) {
	res := run(t, `
		phase('A'); await agent('a'); phase('B')
		return (await checkpoint('q')) === null
	`, Options{Checkpoints: true, AutoCheckpoint: true})
	if res.Value != true {
		t.Fatalf("value = %#v", res.Value)
	}
}

func TestCheckpointRejectsBadArguments(t *testing.T) {
	for _, body := range []string{
		`await checkpoint()`,
		`await checkpoint('  ')`,
		`await checkpoint('x', {f: () => 1, big: 10n})`,
	} {
		_, err := Run(context.Background(), header(body), Options{Runner: echoRunner(0)})
		if err == nil || !strings.Contains(err.Error(), "checkpoint()") {
			t.Fatalf("%s: want a checkpoint() error, got %v", body, err)
		}
	}
}

func TestCheckpointInsideANestedWorkflow(t *testing.T) {
	child := "export const meta = {name: 'child', description: 'asks'}\n" +
		"await agent('child work')\nreturn 'child got ' + (await checkpoint('child asks', {from: 'child'}))"
	events := &eventLog{}
	h := startRun(t, header(`return await workflow('child')`), Options{
		Checkpoints: true,
		Observer:    events.observe,
		Resolve:     func(string) (string, error) { return child, nil },
	})
	cp := wantCheckpoint(t, nextStep(t, h))
	if cp.Message != "child asks" {
		t.Fatalf("checkpoint = %+v", cp)
	}
	if err := h.Resume(cp.ID, "yes"); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if res := wantResult(t, nextStep(t, h)); res.Value != "child got yes" {
		t.Fatalf("value = %#v", res.Value)
	}
	if cps := events.ofKind(EventCheckpoint); len(cps) != 1 || !cps[0].Nested {
		t.Fatalf("checkpoint events = %+v, want one marked nested", cps)
	}
}

// A resumed run replays the orchestrator's answers from the journal instead of
// pausing again — identical checkpoints in order — and still pauses for a
// checkpoint whose data changed.
func TestCheckpointAnswersReplayOnResume(t *testing.T) {
	dir := t.TempDir()
	first := filepath.Join(dir, "run-1")
	script := header(`
		const a = await checkpoint('again?')
		const b = await checkpoint('again?')
		const c = await checkpoint('pick', {n: args.n})
		const d = await agent('use ' + a + b + c)
		return d
	`)

	j1, err := OpenJournal(first)
	if err != nil {
		t.Fatal(err)
	}
	h1 := startRun(t, script, Options{Checkpoints: true, Journal: j1, Args: map[string]any{"n": 1}})
	for _, reply := range []string{"x", "y", "z"} {
		cp := wantCheckpoint(t, nextStep(t, h1))
		if err := h1.Resume(cp.ID, reply); err != nil {
			t.Fatal(err)
		}
	}
	if res := wantResult(t, nextStep(t, h1)); res.Value != "done:use xyz" {
		t.Fatalf("first run value = %#v", res.Value)
	}
	_ = j1.Close()

	raw, err := os.ReadFile(filepath.Join(first, "journal.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(raw), `"kind":"checkpoint"`) != 3 || strings.Count(string(raw), `"kind":"agent"`) != 1 {
		t.Fatalf("journal = %s", raw)
	}

	// Same args: everything replays, nothing pauses, the agent is cached.
	j2, _ := OpenJournal(filepath.Join(dir, "run-2"))
	defer j2.Close()
	if err := j2.LoadCache(first); err != nil {
		t.Fatal(err)
	}
	events := &eventLog{}
	runner := echoRunner(0)
	h2 := startRun(t, script, Options{Checkpoints: true, Journal: j2, Runner: runner, Observer: events.observe, Args: map[string]any{"n": 1}})
	res := wantResult(t, nextStep(t, h2))
	if res.Value != "done:use xyz" || runner.count() != 0 {
		t.Fatalf("resumed value = %#v with %d live calls", res.Value, runner.count())
	}
	if res.Cached != 1 {
		t.Fatalf("cached = %d, want 1 — replayed checkpoints are not replayed agents", res.Cached)
	}
	cps := events.ofKind(EventCheckpoint)
	if len(cps) != 3 || !cps[0].Cached || !cps[2].Cached || cps[2].CheckpointID != "cp-3" {
		t.Fatalf("replayed checkpoint events = %+v", cps)
	}
	if len(events.ofKind(EventResume)) != 3 {
		t.Fatal("every replayed checkpoint must also report its answer")
	}

	// Changed data: the first two replay, the third pauses again.
	j3, _ := OpenJournal(filepath.Join(dir, "run-3"))
	defer j3.Close()
	_ = j3.LoadCache(first)
	h3 := startRun(t, script, Options{Checkpoints: true, Journal: j3, Args: map[string]any{"n": 2}})
	cp := wantCheckpoint(t, nextStep(t, h3))
	if cp.Message != "pick" || cp.ID != "cp-3" {
		t.Fatalf("checkpoint = %+v, want only the changed one to pause", cp)
	}
	if err := h3.Resume(cp.ID, "w"); err != nil {
		t.Fatal(err)
	}
	if res := wantResult(t, nextStep(t, h3)); res.Value != "done:use xyw" {
		t.Fatalf("value = %#v", res.Value)
	}
}

func TestAutoCheckpointAtPhaseBoundaries(t *testing.T) {
	h := startRun(t, header(`
		phase('Scout')          // no agent has run yet: no pause
		await agent('look')
		phase('Scout')          // not a new phase: no pause
		phase('Fix')            // new phase after an agent: pause
		await agent('mend')
		phase('Fix')
		return 'ok'
	`), Options{Checkpoints: true, AutoCheckpoint: true})
	cp := wantCheckpoint(t, nextStep(t, h))
	if !cp.Auto || cp.Message != "phase Scout finished; next: Fix" {
		t.Fatalf("checkpoint = %+v", cp)
	}
	data, _ := cp.Data.(map[string]any)
	if data["finished_phase"] != "Scout" || data["next_phase"] != "Fix" || data["agents"] != float64(1) || data["failed"] != float64(0) {
		t.Fatalf("data = %#v", cp.Data)
	}
	if err := h.Resume(cp.ID, map[string]any{"ok": true}); err != nil {
		t.Fatal(err)
	}
	res := wantResult(t, nextStep(t, h))
	if res.Value != "ok" || res.Agents != 2 {
		t.Fatalf("result = %+v", res)
	}
}

// Without AutoCheckpoint, phase() never pauses.
func TestNoAutoCheckpointByDefault(t *testing.T) {
	h := startRun(t, header(`
		phase('A'); await agent('a'); phase('B'); await agent('b')
		return 'ok'
	`), Options{Checkpoints: true})
	if res := wantResult(t, nextStep(t, h)); res.Value != "ok" {
		t.Fatalf("value = %#v", res.Value)
	}
}

func TestJournalCheckpointEntriesAreNotAgentAnswers(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "run")
	j, _ := OpenJournal(dir)
	_ = j.Append(JournalEntry{Kind: JournalKindCheckpoint, Key: "k", Output: `"reply"`})
	// An entry written before kinds existed is an agent entry.
	_ = j.Append(JournalEntry{Key: "old", Output: "agent text"})
	_ = j.Close()

	j2, _ := OpenJournal(filepath.Join(t.TempDir(), "run2"))
	defer j2.Close()
	if err := j2.LoadCache(dir); err != nil {
		t.Fatal(err)
	}
	if _, ok := j2.Take("k"); ok {
		t.Fatal("a checkpoint answer must not replay as an agent's")
	}
	if e, ok := j2.TakeCheckpoint("k"); !ok || e.Output != `"reply"` {
		t.Fatalf("checkpoint entry = %+v, %v", e, ok)
	}
	if _, ok := j2.TakeCheckpoint("old"); ok {
		t.Fatal("a kind-less entry is an agent entry")
	}
	if e, ok := j2.Take("old"); !ok || e.Output != "agent text" {
		t.Fatalf("legacy entry = %+v, %v", e, ok)
	}
	if j2.Hits() != 1 {
		t.Fatalf("hits = %d, want 1 (checkpoint replays are not counted)", j2.Hits())
	}
}

// The replay key is the canonical JSON of the data, so the order a script
// happened to build an object in does not stop an answer from replaying.
func TestCheckpointReplayIgnoresKeyOrder(t *testing.T) {
	dir := t.TempDir()
	first := filepath.Join(dir, "run-1")
	j1, _ := OpenJournal(first)
	h1 := startRun(t, header(`return await checkpoint('m', {a: 1, b: [2, {c: 3, d: 4}]})`), Options{Checkpoints: true, Journal: j1})
	cp := wantCheckpoint(t, nextStep(t, h1))
	if err := h1.Resume(cp.ID, "answered"); err != nil {
		t.Fatal(err)
	}
	wantResult(t, nextStep(t, h1))
	_ = j1.Close()

	j2, _ := OpenJournal(filepath.Join(dir, "run-2"))
	defer j2.Close()
	_ = j2.LoadCache(first)
	h2 := startRun(t, header(`return await checkpoint('m', {b: [2, {d: 4, c: 3}], a: 1})`), Options{Checkpoints: true, Journal: j2})
	if res := wantResult(t, nextStep(t, h2)); res.Value != "answered" {
		t.Fatalf("value = %#v", res.Value)
	}
}

// A run resumed from a resumed run still replays everything: each run's
// journal re-records what it replayed, so a chain of resumes — the idle reaper
// stops a paused run and points at resume_from_run_id for it — never asks the
// same question twice nor pays for the same agent again.
func TestChainedResumesReplayEverything(t *testing.T) {
	dir := t.TempDir()
	script := header(`
		const a = await checkpoint('q')
		const b = await agent('use ' + a)
		const c = await checkpoint('later')
		return b + c
	`)
	stopAtLater := func(prev, name string, runner *fakeRunner) string {
		t.Helper()
		runDir := filepath.Join(dir, name)
		j, err := OpenJournal(runDir)
		if err != nil {
			t.Fatal(err)
		}
		defer j.Close()
		if prev != "" {
			if err := j.LoadCache(filepath.Join(dir, prev)); err != nil {
				t.Fatal(err)
			}
		}
		h := startRun(t, script, Options{Checkpoints: true, Journal: j, Runner: runner})
		cp := wantCheckpoint(t, nextStep(t, h))
		if cp.Message == "q" {
			if prev != "" {
				t.Fatalf("%s: asked %q again after resuming from %s", name, cp.Message, prev)
			}
			if err := h.Resume(cp.ID, "yes"); err != nil {
				t.Fatal(err)
			}
			cp = wantCheckpoint(t, nextStep(t, h))
		}
		if cp.Message != "later" {
			t.Fatalf("%s: paused at %+v", name, cp)
		}
		h.Stop()
		<-h.Done()
		return name
	}

	r1 := stopAtLater("", "run-1", echoRunner(0))
	r2 := stopAtLater(r1, "run-2", echoRunner(0))
	live := echoRunner(0)
	stopAtLater(r2, "run-3", live)
	if live.count() != 0 {
		t.Fatalf("run-3 re-ran %d agents that run-2 had replayed", live.count())
	}
	raw, err := os.ReadFile(filepath.Join(dir, r2, "journal.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"kind":"checkpoint"`) || !strings.Contains(string(raw), `"kind":"agent"`) || !strings.Contains(string(raw), `"instance":"cp-1"`) {
		t.Fatalf("run-2's journal does not carry what it replayed:\n%s", raw)
	}
}

// A checkpoint queued by the same stretch of script that then returns is not
// one the run stops at, so it must never be handed to the host: the host would
// report "paused" and detach while the run finished on its own, its result
// going nowhere and the orchestrator's continue finding no paused run.
func TestACheckpointTheRunOutlivesNeverSurfaces(t *testing.T) {
	child := "export const meta = {name: 'child', description: 'c'}\ncheckpoint('fyi from the child'); return 1"
	cases := map[string]struct {
		body string
		auto bool
	}{
		"auto checkpoint on a final phase with no agents": {`await agent('a'); phase('Report'); return 'done'`, true},
		"unawaited checkpoint before returning":           {`checkpoint('fyi', {n: 1}); return 'done'`, false},
		"unawaited checkpoint after an agent":             {`await agent('a'); checkpoint('fyi'); return 'done'`, false},
		"unawaited checkpoint in a child":                 {`await workflow({script: args.child}); return 'done'`, false},
	}
	for label, tc := range cases {
		t.Run(label, func(t *testing.T) {
			for i := 0; i < 200; i++ {
				h, err := Start(context.Background(), header(tc.body), Options{
					Runner: echoRunner(0), Checkpoints: true, AutoCheckpoint: tc.auto,
					Args: map[string]any{"child": child},
				})
				if err != nil {
					t.Fatal(err)
				}
				step := nextStep(t, h)
				if step.Checkpoint != nil {
					h.Stop()
					<-h.Done()
					t.Fatalf("run %d: surfaced %+v for a run that settles by itself", i, *step.Checkpoint)
				}
				if res := wantResult(t, step); res.Value != "done" {
					t.Fatalf("value = %#v", res.Value)
				}
			}
		})
	}
}

// A checkpoint queued behind the one being answered surfaces only once the
// script has run on with the reply — the reply may be what settles the run.
func TestAQueuedCheckpointWaitsForTheReplyToRun(t *testing.T) {
	for i := 0; i < 200; i++ {
		h := startRun(t, header(`
			const first = checkpoint('one')
			checkpoint('two')
			return await first
		`), Options{Checkpoints: true})
		cp := wantCheckpoint(t, nextStep(t, h))
		if cp.Message != "one" {
			t.Fatalf("first checkpoint = %+v", cp)
		}
		if err := h.Resume(cp.ID, "answered"); err != nil {
			t.Fatal(err)
		}
		step := nextStep(t, h)
		if step.Checkpoint != nil {
			t.Fatalf("run %d: surfaced %+v while the reply that settles the run was on its way", i, *step.Checkpoint)
		}
		if res := wantResult(t, step); res.Value != "answered" {
			t.Fatalf("value = %#v", res.Value)
		}
	}
}

// An agent() held at the pause gate when the run settles never starts: no
// start event after the run's finish, and no Runner call on a dead context.
func TestGatedAgentDoesNotStartAfterTheRunSettles(t *testing.T) {
	for i := 0; i < 100; i++ {
		var mu sync.Mutex
		var events []Event
		runner := echoRunner(0)
		h, err := Start(context.Background(), header(`checkpoint('x'); agent('late'); return 1`), Options{
			Runner: runner, Checkpoints: true,
			Observer: func(ev Event) {
				mu.Lock()
				events = append(events, ev)
				mu.Unlock()
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		wantResult(t, nextStep(t, h))
		<-h.Done()
		// Give a straggling dispatch goroutine the time to misbehave.
		time.Sleep(5 * time.Millisecond)
		if n := runner.count(); n != 0 {
			t.Fatalf("run %d: the Runner was called %d times after the run settled", i, n)
		}
		mu.Lock()
		finished := false
		for _, ev := range events {
			if ev.Kind == EventFinish && !ev.Nested {
				finished = true
			}
			if finished && ev.Kind == EventAgentStart {
				mu.Unlock()
				t.Fatalf("run %d: an agent started after the run finished: %+v", i, ev)
			}
		}
		mu.Unlock()
	}
}
