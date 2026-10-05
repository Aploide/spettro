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
}

// NewWorkflowRuns returns an empty registry.
func NewWorkflowRuns() *WorkflowRuns {
	return &WorkflowRuns{runs: map[string]*liveWorkflow{}}
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

func (w *WorkflowRuns) remove(runID string) {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.runs, runID)
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

// StopAll stops every live run and empties the registry. Hosts call it when
// the session the runs belong to goes away: /clear, a session switch, the
// process exiting, an ACP session closing.
func (w *WorkflowRuns) StopAll() {
	if w == nil {
		return
	}
	w.mu.Lock()
	runs := w.runs
	w.runs = map[string]*liveWorkflow{}
	w.mu.Unlock()
	for _, r := range runs {
		r.stop()
	}
}
