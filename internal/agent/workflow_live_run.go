package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"spettro/internal/workflow"
)

// workflowIdleTimeout is how long a run may sit paused at a checkpoint with no
// continue before it is stopped. A paused run holds a goroutine, a journal
// file and possibly worktrees; an orchestrator that walked away — the user
// changed topic, the model forgot the run id — must not leak them for the
// rest of the session. A variable so tests can shorten it.
var workflowIdleTimeout = 30 * time.Minute

// workflowStopAllGrace bounds how long StopAll waits for stopped runs to
// write their result and close their journal. A paused run unwinds at once
// (nothing is in flight while paused), so the bound only matters for a run
// whose agents ignore cancellation, and it must not hang a host's /clear or
// exit on one.
var workflowStopAllGrace = 5 * time.Second

// liveWorkflow is one run held across tool calls: the engine handle and
// everything the workflow tool needs to keep driving it, render its result
// and clean up after it once it settles.
type liveWorkflow struct {
	runID string
	name  string

	handle   *workflow.Handle
	journal  *workflow.Journal
	observer *workflowObserver
	runner   *workflowRunner
	meta     workflow.Meta
	script   string
	origin   string
	dir      string
	savedAt  string
	saveName string
	registry *WorkflowRuns

	mu         sync.Mutex
	paused     bool
	checkpoint string
	message    string
	pausedAt   time.Time
	cancel     func()
	// busy is set while a tool call is driving the run. Two calls continuing
	// the same run at once would race on Resume and both wait on the same
	// Next; the second is refused instead.
	busy bool
	// pauseGen counts pauses so an idle timer armed for an earlier pause
	// cannot stop the run after it was continued and paused again.
	pauseGen int
	idle     *time.Timer
	// stopReason, when set, turns the cancellation error into a "stopped"
	// finish trace carrying it: a run the orchestrator, the idle reaper or
	// the host stopped did not fail.
	stopReason string

	finalOnce sync.Once
	finalized chan struct{}
	finalErr  error
}

func (l *liveWorkflow) pausedInfo() (PausedWorkflow, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.paused {
		return PausedWorkflow{}, false
	}
	return PausedWorkflow{RunID: l.runID, Name: l.name, CheckpointID: l.checkpoint, Message: l.message, PausedAt: l.pausedAt}, true
}

func (l *liveWorkflow) stop() {
	l.mu.Lock()
	cancel := l.cancel
	l.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// stopWith stops the run, recording why for the finish trace. The first
// reason wins: a reaped run that StopAll then also stops was still reaped.
func (l *liveWorkflow) stopWith(reason string) {
	l.mu.Lock()
	if l.stopReason == "" {
		l.stopReason = reason
	}
	l.mu.Unlock()
	l.stop()
}

// stoppedBecause is the reason the run was stopped on purpose; "" if it was
// not.
func (l *liveWorkflow) stoppedBecause() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.stopReason
}

// wait blocks until the run has settled and been finalized, or d elapses.
func (l *liveWorkflow) wait(d time.Duration) bool {
	if l.finalized == nil {
		return true
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-l.finalized:
		return true
	case <-t.C:
		return false
	}
}

// watch finalizes the run as soon as it settles, whoever caused it. A run
// stopped while no tool call was driving it — the idle reaper, a host's
// StopAll — still has to write its result and close its journal.
func (l *liveWorkflow) watch() {
	go func() {
		<-l.handle.Done()
		l.finalize()
	}()
}

// finalize runs once, after the run settled: the finish trace, result.json,
// the journal close and the registry entry. The tool call that saw the run
// settle calls it before rendering, so hosts get the finish trace before the
// tool result; the watcher calls it for runs that settled unattended.
func (l *liveWorkflow) finalize() {
	l.finalOnce.Do(func() {
		defer close(l.finalized)
		<-l.handle.Done()
		// Next returns at once on a settled run; it is the only accessor for
		// the run's error.
		step, _ := l.handle.Next(context.Background())
		res := l.handle.Snapshot()
		err := step.Err
		l.mu.Lock()
		if l.idle != nil {
			l.idle.Stop()
		}
		l.paused = false
		reason := l.stopReason
		l.mu.Unlock()
		l.finalErr = err
		if err != nil && reason != "" {
			// Stopped on purpose — by the orchestrator, the idle reaper or
			// the host — so the error is the stop's own cancellation, not a
			// failure, and hosts must not paint it red. They get a status
			// of its own, with the reason.
			l.observer.stopped(res, reason)
		} else {
			l.observer.finish(res, err)
		}
		if l.journal != nil {
			if encoded, err := json.MarshalIndent(res.Value, "", "  "); err == nil {
				_ = l.journal.WriteFile("result.json", string(encoded))
			}
			_ = l.journal.Close()
		}
		l.registry.removeRun(l)
	})
}

// claim marks the run as driven by the calling tool call.
func (l *liveWorkflow) claim() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.busy {
		return fmt.Errorf("workflow: run %s is already being driven by another workflow call", l.runID)
	}
	l.busy = true
	return nil
}

// claimDetached claims the run for the caller if no tool call is driving it
// and it sits paused — the state in which its finish trace can reach no host
// (see WorkflowRuns.SetOnStopped). Claiming it in the same critical section
// refuses a continue racing the stop instead of resuming a run being torn
// down.
func (l *liveWorkflow) claimDetached() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.paused || l.busy {
		return false
	}
	l.busy = true
	return true
}

func (l *liveWorkflow) release() {
	l.mu.Lock()
	l.busy = false
	l.mu.Unlock()
}

// bind points the run's callbacks at rt — the turn now driving it — or, with
// nil, at nobody. A paused run is detached the moment its tool call returns:
// the turn that started it may end before anyone continues it, and its
// callbacks (traces, approvals, questions) must never be called after that.
// Quiescence guarantees nothing runs while paused, so nothing misses them.
func (l *liveWorkflow) bind(rt *toolRuntime) {
	l.observer.rebind(rt)
	l.runner.rebind(rt)
}

// markPaused records the surfaced checkpoint and arms the idle reaper.
func (l *liveWorkflow) markPaused(cp workflow.Checkpoint) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.paused = true
	l.checkpoint = cp.ID
	l.message = cp.Message
	l.pausedAt = time.Now()
	l.pauseGen++
	gen := l.pauseGen
	if l.idle != nil {
		l.idle.Stop()
	}
	l.idle = time.AfterFunc(workflowIdleTimeout, func() { l.reap(gen) })
}

// markRunning clears the pause once the checkpoint is answered.
func (l *liveWorkflow) markRunning() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.paused = false
	l.pauseGen++
	if l.idle != nil {
		l.idle.Stop()
		l.idle = nil
	}
}

// reap stops a run nobody continued. The journal is kept: the run's answered
// checkpoints and finished agents replay through resume_from_run_id, so the
// work is not lost, only the live goroutine.
func (l *liveWorkflow) reap(gen int) {
	l.mu.Lock()
	stale := !l.paused || l.busy || gen != l.pauseGen
	if !stale {
		// Claim the run in the same critical section, so a continue racing
		// the reaper is refused rather than resuming a run being stopped.
		l.busy = true
	}
	l.mu.Unlock()
	if stale {
		return
	}
	idle := formatIdle(workflowIdleTimeout)
	l.registry.noteEnded(l.runID, fmt.Sprintf("stopped after %s paused at a checkpoint with no continue", idle))
	reason := fmt.Sprintf("paused for over %s with no continue", idle)
	l.stopWith(reason)
	// A reaped run is detached by definition: no turn hears its finish
	// trace, so the host is told directly.
	l.registry.notifyStopped(l, reason)
}
