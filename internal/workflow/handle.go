package workflow

import (
	"context"
	"fmt"
)

// Handle is a running workflow. Start returns one; the host then drives the
// run with Next, answering each checkpoint the script raises with Resume,
// until Next reports the run settled.
//
// The run lives on its own goroutine and on the context passed to Start, not
// on any context passed to Next: a host can return from a tool call while the
// run sits paused at a checkpoint, and continue it from a later one.
type Handle struct {
	sh     *shared
	runCtx context.Context
	cancel context.CancelFunc
	done   chan struct{}

	// result and err are written once, before done is closed, and only read
	// after it is — the close is the synchronisation.
	result Result
	err    error
}

// Step is what Next returns: the run either paused at a checkpoint or settled.
type Step struct {
	// Checkpoint is non-nil when the run is paused waiting for Resume.
	Checkpoint *Checkpoint
	// Result is non-nil once the run has settled; Err then holds the run's
	// error (nil on success, the context's error after Stop).
	Result *Result
	Err    error
}

// Checkpoint is one pause the orchestrator has to answer.
type Checkpoint struct {
	// ID is "cp-1", "cp-2", …: the checkpoint's ordinal across the whole run,
	// nested workflow() children included.
	ID      string
	Message string
	// Data is the script's second argument to checkpoint(), as JSON-decoded
	// Go values (map[string]any, []any, float64, string, bool, nil).
	Data any
	// Phase is the phase the script was in when it raised the checkpoint.
	Phase string
	// Auto is true for an automatic phase-boundary checkpoint.
	Auto bool
}

// Start begins a run on its own goroutine and returns a handle to it.
//
// The only error Start itself reports is a missing Runner; everything else —
// a bad header, a script that does not compile, a missing param — settles the
// run, and Next reports it as the Step's Err exactly as Run would.
func Start(ctx context.Context, script string, opts Options) (*Handle, error) {
	opts = opts.withDefaults()
	if opts.Runner == nil {
		return nil, fmt.Errorf("workflow: no agent runner configured")
	}
	runCtx, cancel := context.WithCancel(ctx)
	sh := newShared(opts)
	h := &Handle{sh: sh, runCtx: runCtx, cancel: cancel, done: make(chan struct{})}
	go func() {
		value, meta, err := sh.execute(runCtx, script, opts.Args, 0)
		// Settling cancels whatever the script left running: an agent() it
		// never awaited has nobody left to report to, and a gated dispatch
		// would otherwise wait forever on a checkpoint nobody will answer.
		cancel()
		sh.settle()
		res := sh.snapshot()
		res.Meta = meta
		res.Value = value
		h.result, h.err = res, err
		close(h.done)
	}()
	return h, nil
}

// Done is closed once the run has settled.
func (h *Handle) Done() <-chan struct{} { return h.done }

// Stop cancels the run. It does not wait: Next then returns the settled
// result, whose error is the cancellation. Stopping a settled run is a no-op.
func (h *Handle) Stop() { h.cancel() }

// Next blocks until the run pauses at a checkpoint or settles.
//
// ctx bounds only the wait, not the run: when it expires Next returns its
// error and the run carries on. Calling Next again while a checkpoint is
// pending returns the same checkpoint; calling it after the run settled
// returns the same result.
//
// A pause is surfaced only once no agent dispatch is in flight anywhere in the
// run (see shared.surface), so when Next returns a checkpoint nothing is
// executing on the host's behalf until Resume.
func (h *Handle) Next(ctx context.Context) (Step, error) {
	for {
		select {
		case <-h.done:
			res := h.result
			return Step{Result: &res, Err: h.err}, nil
		default:
		}
		if h.runCtx.Err() != nil {
			// Stopped (or the Start context ended): the run is unwinding, and
			// a checkpoint still marked pending is not a pause any more.
			select {
			case <-h.done:
				continue
			case <-ctx.Done():
				return Step{}, ctx.Err()
			}
		}
		cp, wake := h.sh.surface()
		if cp != nil {
			return Step{Checkpoint: cp}, nil
		}
		select {
		case <-wake:
		case <-h.done:
		case <-ctx.Done():
			return Step{}, ctx.Err()
		}
	}
}

// Resume answers the pending checkpoint — the one the last Next returned —
// with value, which checkpoint() resolves to in the script. value is any
// JSON-encodable Go value (a json.RawMessage passes through as-is); nil
// becomes null. id may be empty to mean "the pending one"; otherwise it must
// match it, so an orchestrator answering a stale checkpoint is told rather
// than silently answering a different question.
func (h *Handle) Resume(id string, value any) error {
	select {
	case <-h.done:
		return fmt.Errorf("workflow: the run has already finished")
	default:
	}
	return h.sh.resume(id, value)
}

// Pending returns the checkpoint the run is paused at, if it is paused.
func (h *Handle) Pending() (Checkpoint, bool) {
	return h.sh.pending()
}

// Snapshot returns the run's counters so far: agents, failed, cached, tokens,
// logs and phases (and the meta, once the header has parsed). After the run
// settles it returns the final result, Value included.
func (h *Handle) Snapshot() Result {
	select {
	case <-h.done:
		return h.result
	default:
		return h.sh.snapshot()
	}
}
