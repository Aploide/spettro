package agent

import (
	"sort"
	"sync"
	"time"
)

// WorkflowRuns is the registry of live workflow runs: ones that paused at a
// checkpoint and are waiting for the orchestrating model to continue them.
//
// A paused run is a goroutine holding a script mid-await, so it cannot live in
// the toolRuntime that started it — that runtime is gone when the turn ends,
// and the orchestrator may only answer in a later turn. Hosts therefore own
// one registry per session and pass it into every run, the same way they
// thread the steering queue through.
type WorkflowRuns struct {
	mu   sync.Mutex
	runs map[string]*liveWorkflow
	// ended holds tombstones for runs the registry stopped on its own,
	// keyed by run id: see noteEnded.
	ended map[string]string
	// onStopped is the host's hook for runs stopped while detached; see
	// SetOnStopped.
	onStopped func(runID, name, reason string)
	// turnLocal marks a registry runToolLoop made for one turn because the
	// host owns none. Its runs are stopped when the turn ends, so a
	// checkpoint must tell the model to answer it in the same turn rather
	// than promise it the half hour a host-owned run waits.
	turnLocal bool
}

// Reasons a run was stopped on purpose, carried by the "stopped" finish trace
// and SetOnStopped. Hosts show them after "stopped: ", so they read as the
// cause, not as a sentence of their own.
const (
	workflowStopOrchestrator = "at the orchestrator's request"
	workflowStopSessionEnded = "the session ended"
	workflowStopTurnEnded    = "the turn ended"
)

// NewWorkflowRuns returns an empty registry.
func NewWorkflowRuns() *WorkflowRuns {
	return &WorkflowRuns{runs: map[string]*liveWorkflow{}}
}

// SetOnStopped registers fn to be told when a run that is detached — paused
// at a checkpoint, its tool call long returned — is stopped by the idle
// reaper or by StopAll.
//
// Such a run's finish trace reaches nobody: no turn is bound to it, and a
// trace for a turn that ended must never be delivered. Without this hook a
// host's paused panel or card would keep saying "waiting for orchestrator"
// for a run that no longer exists. A run a tool call is driving when it
// stops is not reported here: its finish trace reaches the host as usual.
//
// fn is called on a goroutine of its own, after the run has finalized (its
// transcript is written and Paused no longer lists it), and never under the
// registry's lock — so a host may call back into the registry, or post into
// its own event loop, from fn without deadlocking against the StopAll it is
// itself running. nil unregisters.
func (w *WorkflowRuns) SetOnStopped(fn func(runID, name, reason string)) {
	if w == nil {
		return
	}
	w.mu.Lock()
	w.onStopped = fn
	w.mu.Unlock()
}

// notifyStopped reports a detached run that was stopped, once it has
// finalized; see SetOnStopped.
func (w *WorkflowRuns) notifyStopped(run *liveWorkflow, reason string) {
	if w == nil || run == nil {
		return
	}
	w.mu.Lock()
	fn := w.onStopped
	w.mu.Unlock()
	if fn == nil {
		return
	}
	go func() {
		run.wait(workflowStopAllGrace)
		fn(run.runID, run.name, reason)
	}()
}

// isTurnLocal reports whether the registry dies with the turn; see turnLocal.
func (w *WorkflowRuns) isTurnLocal() bool {
	return w != nil && w.turnLocal
}

// PausedWorkflow describes one run waiting on the orchestrator, for hosts
// that list or display them.
type PausedWorkflow struct {
	RunID        string
	Name         string
	CheckpointID string
	Message      string
	PausedAt     time.Time
}

func (w *WorkflowRuns) get(runID string) *liveWorkflow {
	if w == nil {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.runs[runID]
}

func (w *WorkflowRuns) put(run *liveWorkflow) {
	if w == nil || run == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.runs == nil {
		w.runs = map[string]*liveWorkflow{}
	}
	w.runs[run.runID] = run
}

// ids lists the live run IDs, sorted, for error messages.
func (w *WorkflowRuns) ids() []string {
	if w == nil {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]string, 0, len(w.runs))
	for id := range w.runs {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// Paused lists the runs currently waiting on the orchestrator, oldest first.
func (w *WorkflowRuns) Paused() []PausedWorkflow {
	if w == nil {
		return nil
	}
	w.mu.Lock()
	runs := make([]*liveWorkflow, 0, len(w.runs))
	for _, r := range w.runs {
		runs = append(runs, r)
	}
	w.mu.Unlock()
	var out []PausedWorkflow
	for _, r := range runs {
		if p, ok := r.pausedInfo(); ok {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PausedAt.Before(out[j].PausedAt) })
	return out
}

// removeRun drops run from the registry, unless the slot already holds a
// different run under the same id (it never should; ids are unique).
func (w *WorkflowRuns) removeRun(run *liveWorkflow) {
	if w == nil || run == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.runs[run.runID] == run {
		delete(w.runs, run.runID)
	}
}

// maxEndedRuns bounds the tombstones kept for runs the registry stopped on
// its own, so a long session cannot grow the map without limit.
const maxEndedRuns = 64

// noteEnded remembers why a run the registry stopped by itself (the idle
// reaper) is gone, so a later continue_run_id gets that reason instead of a
// bare "unknown run".
func (w *WorkflowRuns) noteEnded(runID, reason string) {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.ended == nil {
		w.ended = map[string]string{}
	}
	if len(w.ended) >= maxEndedRuns {
		for id := range w.ended {
			delete(w.ended, id)
			break
		}
	}
	w.ended[runID] = reason
}

// endedReason returns why the registry stopped runID, if it did.
func (w *WorkflowRuns) endedReason(runID string) (string, bool) {
	if w == nil {
		return "", false
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	reason, ok := w.ended[runID]
	return reason, ok
}

// StopAll stops every live run and empties the registry. Hosts call it when
// the session the runs belong to goes away: /clear, a session switch, the
// process exiting, an ACP session closing.
//
// It waits (briefly, see workflowStopAllGrace) for the stopped runs to write
// their result and close their journal, so a process exiting right after
// still leaves a complete transcript to resume from.
func (w *WorkflowRuns) StopAll() {
	w.stopAll(workflowStopSessionEnded)
}

// stopAll is StopAll with the reason the finish trace and SetOnStopped give:
// a host ending a session, or runToolLoop ending the turn a turn-local
// registry belonged to.
func (w *WorkflowRuns) stopAll(reason string) {
	if w == nil {
		return
	}
	w.mu.Lock()
	runs := w.runs
	w.runs = map[string]*liveWorkflow{}
	w.mu.Unlock()
	var detached []*liveWorkflow
	for _, r := range runs {
		if r.claimDetached() {
			detached = append(detached, r)
		}
		r.stopWith(reason)
	}
	for _, r := range detached {
		w.notifyStopped(r, reason)
	}
	deadline := time.Now().Add(workflowStopAllGrace)
	for _, r := range runs {
		if !r.wait(time.Until(deadline)) {
			return
		}
	}
}
