package workflow

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/dop251/goja"
)

// pendingCheckpoint is a checkpoint raised by a script and not yet answered.
type pendingCheckpoint struct {
	cp     Checkpoint
	seq    int
	key    string
	nested bool
	// answer delivers the reply (as JSON) to the script that raised the
	// checkpoint. It must not block: it is called from Resume, on the host's
	// goroutine.
	answer func(encoded string)
}

// The checkpoint state lives on shared, guarded by shared.mu, because the
// queue is the run's, not a script's: a workflow() child raising a checkpoint
// pauses the whole run, and its pause has to wait for the parent's agents
// to finish like any other.
//
//   - queue holds every raised, unanswered checkpoint, oldest first. Next
//     surfaces only the head and each Resume answers exactly one, so two
//     branches that both ask get two separate answers, in the order they
//     asked.
//   - active counts agent dispatches past the pause gate (admitted when
//     agent() was called, running, or waiting for a concurrency slot, until
//     the answer is queued for the script). While the queue is non-empty no
//     new dispatch passes the gate, so active can only drain — which is what
//     lets a pause surface at all on a script that keeps fanning out.
//   - busy counts what could still run script code without anything new
//     being dispatched: every script loop (the top-level one and each
//     workflow() child) that is not blocked waiting for work with its promise
//     pending, plus every job queued for a loop — an agent's answer, a
//     child's result, a checkpoint reply — from before it is queued until it
//     has run. While busy is non-zero the script may still settle by itself,
//     or raise more, or start agents, so nothing surfaces.
//   - surfaced is the head once Next has handed it to the host. From then
//     until Resume the run is frozen: no script code runs, no agent runs.
//   - settled is set once the run has ended; nothing surfaces after it.
//   - wake is closed and replaced on every change, so waiters (Next, gated
//     dispatches, frozen script loops) re-check without polling.

func (s *shared) signalLocked() {
	close(s.wake)
	s.wake = make(chan struct{})
}

// hold takes a busy token. It is always taken by something that is itself
// busy or in flight (a running loop, an admitted dispatch, Resume under the
// lock), before that gives its own up, so busy and active never both read
// zero while work is on its way to a script.
func (s *shared) hold() {
	s.mu.Lock()
	s.busy++
	s.mu.Unlock()
}

// release gives a busy token back and wakes a Next that may now surface.
func (s *shared) release() {
	s.mu.Lock()
	s.busy--
	s.signalLocked()
	s.mu.Unlock()
}

// enqueue adds a raised checkpoint to the queue.
func (s *shared) enqueue(p *pendingCheckpoint) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.queue = append(s.queue, p)
	s.signalLocked()
}

// surface implements the quiescence rule: it hands out the head of the queue
// only when no agent dispatch is in flight anywhere in the run, and every
// script loop is parked on a pending promise with nothing queued for it.
//
// The rule exists because of what the host does with a pause. The tool call
// that was waiting returns the checkpoint to the model, and the turn may end
// there; a sub-agent still running would then report into a turn that is
// gone — its tool callbacks, approvals and questions all belong to it. Waiting
// for the in-flight agents (new ones are held at the gate) makes "paused"
// mean that nothing is running on anyone's behalf.
//
// Waiting for the script loops is what makes a surfaced checkpoint one the
// run actually stops at. A checkpoint can be queued by the same stretch of
// script that then returns — phase() raising an automatic checkpoint as the
// last phase begins, or a checkpoint() nobody awaits — and handing that one to
// the host would report "paused" for a run that was in fact about to finish
// on its own: the host detaches, the result goes nowhere, and the
// orchestrator's continue finds no paused run.
//
// When there is nothing to surface it returns the channel to wait on.
func (s *shared) surface() (*Checkpoint, <-chan struct{}) {
	s.mu.Lock()
	if s.surfaced != nil {
		cp := s.surfaced.cp
		s.mu.Unlock()
		return &cp, nil
	}
	if len(s.queue) == 0 || s.active > 0 || s.busy > 0 || s.settled {
		wake := s.wake
		s.mu.Unlock()
		return nil, wake
	}
	p := s.queue[0]
	s.surfaced = p
	s.signalLocked()
	s.mu.Unlock()

	cp := p.cp
	s.emit(Event{Kind: EventCheckpoint, Phase: cp.Phase, Message: cp.Message, Output: encodeJSON(cp.Data), CheckpointID: cp.ID, Auto: cp.Auto, Nested: p.nested})
	return &cp, nil
}

// settle drops whatever is still queued once the run has ended: a checkpoint
// the script raised but never awaited has nobody left to deliver to.
func (s *shared) settle() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.queue, s.surfaced = nil, nil
	s.settled = true
	s.signalLocked()
}

func (s *shared) pending() (Checkpoint, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.surfaced == nil {
		return Checkpoint{}, false
	}
	return s.surfaced.cp, true
}

func (s *shared) resume(id string, value any) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("workflow: the checkpoint reply is not JSON-encodable: %w", err)
	}
	s.mu.Lock()
	p := s.surfaced
	switch {
	case p == nil:
		s.mu.Unlock()
		return fmt.Errorf("workflow: no checkpoint is pending")
	case id != "" && id != p.cp.ID:
		s.mu.Unlock()
		return fmt.Errorf("workflow: checkpoint %q is not the pending one (%s)", id, p.cp.ID)
	}
	s.surfaced = nil
	s.queue = s.queue[1:]
	// The reply's job is busy from this instant (p.answer queues it with
	// this token): a checkpoint queued behind this one must not surface
	// before the script has run on with the reply — the reply may be what
	// settles the run.
	s.busy++
	s.signalLocked()
	s.mu.Unlock()

	text := string(encoded)
	_ = s.opts.Journal.Append(JournalEntry{
		Kind: JournalKindCheckpoint, Key: p.key, Index: p.seq, Instance: p.cp.ID,
		Label: p.cp.Message, Phase: p.cp.Phase, Output: text,
	})
	s.emit(Event{Kind: EventResume, Phase: p.cp.Phase, Message: p.cp.Message, Output: text, CheckpointID: p.cp.ID, Auto: p.cp.Auto, Nested: p.nested})
	p.answer(text)
	return nil
}

// admit is the pause gate's fast path, taken synchronously when agent() is
// called: with no checkpoint queued the dispatch counts as in flight from that
// moment. Doing it on the script goroutine, rather than when the dispatch
// goroutine gets scheduled, is what makes the order exact — an agent() called
// before checkpoint() is one the pause waits for, one called after it waits
// for the pause.
func (s *shared) admit() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.queue) > 0 {
		return false
	}
	s.active++
	return true
}

// enterDispatch is the pause gate's slow path: a dispatch that was not
// admitted waits here before it does anything — before journal replay, before
// the concurrency slot, before the Runner — until no checkpoint is queued. A
// pause therefore cannot be starved by a script that keeps starting agents,
// and nothing new runs while the orchestrator is deciding.
//
// A run that settles empties the queue after cancelling its context, so a
// gated dispatch woken by that must see the cancellation rather than the empty
// queue: it reports the context's error instead of taking a slot.
func (s *shared) enterDispatch(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		s.mu.Lock()
		if len(s.queue) == 0 {
			s.active++
			s.mu.Unlock()
			return nil
		}
		wake := s.wake
		s.mu.Unlock()
		select {
		case <-wake:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (s *shared) leaveDispatch() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.active--
	s.signalLocked()
}

// frozen returns a channel to wait on while the run is paused at a surfaced
// checkpoint, or nil when script code may run. Freezing the script loops is
// what keeps a paused run from emitting progress into a turn that has ended.
func (s *shared) frozen() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.surfaced == nil {
		return nil
	}
	return s.wake
}

// waitThawed blocks while the run is frozen at a surfaced checkpoint.
func (s *shared) waitThawed(ctx context.Context) error {
	for {
		wait := s.frozen()
		if wait == nil {
			return nil
		}
		select {
		case <-wait:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// jsCheckpoint implements the checkpoint(message, data?) global: pause the run
// and hand message and data to the orchestrator, resolving with its reply.
func (r *vmRun) jsCheckpoint(ctx context.Context) func(goja.FunctionCall) goja.Value {
	return func(call goja.FunctionCall) goja.Value {
		vm := r.vm
		msgArg := call.Argument(0)
		msg := ""
		if !goja.IsUndefined(msgArg) && !goja.IsNull(msgArg) {
			msg = strings.TrimSpace(msgArg.String())
		}
		if msg == "" {
			panic(vm.NewTypeError("checkpoint(): a message string is required"))
		}
		data, encoded, err := exportJSON(call.Argument(1))
		if err != nil {
			panic(vm.NewTypeError(fmt.Sprintf("checkpoint(): data must be JSON-encodable: %v", err)))
		}
		promise, resolve, _ := vm.NewPromise()
		r.raiseCheckpoint(ctx, msg, data, checkpointKey(msg, encoded), false, func(v goja.Value) { resolve(v) })
		return vm.ToValue(promise)
	}
}

// raiseCheckpoint runs on the script goroutine. settle receives the value the
// checkpoint resolves to, on the script goroutine too; it may be nil for an
// automatic checkpoint nobody awaits.
func (r *vmRun) raiseCheckpoint(ctx context.Context, msg string, data any, key string, auto bool, settle func(goja.Value)) {
	s := r.sh
	if settle == nil {
		settle = func(goja.Value) {}
	}
	seq := int(s.cpSeq.Add(1))
	id := fmt.Sprintf("cp-%d", seq)
	phase := r.currentPhase()

	// A resumed run replays the orchestrator's earlier answers without
	// pausing, as it replays agents: the point of resuming is not to be asked
	// the same thing twice.
	if entry, ok := s.opts.Journal.TakeCheckpoint(key); ok {
		// The answer goes into this run's journal too. Resuming is routinely
		// chained — the idle reaper stops a run paused too long and points at
		// resume_from_run_id for it — and the next link loads only this
		// journal: an answer replayed but not re-recorded would be asked again.
		_ = s.opts.Journal.Append(JournalEntry{
			Kind: JournalKindCheckpoint, Key: key, Index: seq, Instance: id,
			Label: msg, Phase: phase, Output: entry.Output,
		})
		ev := Event{Phase: phase, Message: msg, CheckpointID: id, Auto: auto, Cached: true, Nested: r.nested}
		ev.Kind, ev.Output = EventCheckpoint, encodeJSON(data)
		s.emit(ev)
		ev.Kind, ev.Output = EventResume, entry.Output
		s.emit(ev)
		settle(r.decodeJSON(entry.Output))
		return
	}

	if !s.opts.Checkpoints {
		// A non-interactive host has nobody to ask. Resolving to null keeps
		// the script running — it is written to treat null as "no steer" —
		// and the log line keeps the skip visible in the result.
		line := "checkpoint skipped (no orchestrator): " + msg
		s.addLog(line)
		s.emit(Event{Kind: EventLog, Phase: phase, Message: line, Nested: r.nested})
		settle(goja.Null())
		return
	}

	// A pending checkpoint counts as in flight for the deadlock detector: a
	// script awaiting the orchestrator is waiting on something that will
	// settle, however long the orchestrator takes.
	r.inflight.Add(1)
	s.enqueue(&pendingCheckpoint{
		cp:     Checkpoint{ID: id, Message: msg, Data: data, Phase: phase, Auto: auto},
		seq:    seq,
		key:    key,
		nested: r.nested,
		answer: func(encoded string) {
			go func() {
				defer r.inflight.Add(-1)
				// resume already holds this job's busy token.
				r.send(ctx, func() { settle(r.decodeJSON(encoded)) })
			}()
		},
	})
}

// autoCheckpoint raises the phase-boundary checkpoint Options.AutoCheckpoint
// asks for. Its journal key is the message alone: the agent counts in its
// data can legitimately differ on a resumed run (a failed agent re-run), and
// that must not make the orchestrator answer the same boundary twice.
func (r *vmRun) autoCheckpoint(ctx context.Context, prev, next string) {
	finished := prev
	if finished == "" {
		finished = "(unnamed)"
	}
	msg := fmt.Sprintf("phase %s finished; next: %s", finished, next)
	data := map[string]any{
		"finished_phase": prev,
		"next_phase":     next,
		"agents":         float64(r.sh.agents.Load()),
		"failed":         float64(r.sh.failed.Load()),
	}
	r.raiseCheckpoint(ctx, msg, data, checkpointKey(msg, "auto"), true, nil)
}

// exportJSON exports a script value to plain JSON-decoded Go values and its
// canonical encoding. json.Marshal sorts object keys, so two checkpoints with
// the same data hash the same however the script built the object.
func exportJSON(v goja.Value) (any, string, error) {
	if v == nil || goja.IsUndefined(v) || goja.IsNull(v) {
		return nil, "null", nil
	}
	encoded, err := json.Marshal(v.Export())
	if err != nil {
		return nil, "", err
	}
	var data any
	if err := json.Unmarshal(encoded, &data); err != nil {
		return nil, "", err
	}
	return data, string(encoded), nil
}

func encodeJSON(v any) string {
	encoded, err := json.Marshal(v)
	if err != nil {
		return "null"
	}
	return string(encoded)
}

// plainValue hands a Go value — a structured agent answer, a workflow()
// child's result — to the script as plain JS objects and arrays, through its
// canonical JSON.
//
// vm.ToValue would wrap a Go map as a map-backed object whose keys enumerate
// in Go's randomised map order, so the same answer stringified differently
// from one call to the next: untilDry's dedupe key, a prompt built from the
// value, a journal key derived from it — anything order-sensitive — stopped
// being reproducible. json.Marshal sorts object keys, so the script sees one
// order, every time, on a live answer and on a replayed one alike. A value
// that has no JSON form (a child returning NaN or a function) keeps the old
// wrapping rather than being lost.
func (r *vmRun) plainValue(v any) goja.Value {
	if v == nil {
		return goja.Null()
	}
	encoded, err := json.Marshal(v)
	if err != nil {
		return r.vm.ToValue(v)
	}
	return r.decodeJSON(string(encoded))
}

// decodeJSON turns a reply into a script value with the runtime's own
// JSON.parse, captured before the script ran, so the script gets real JS
// objects and arrays — the same thing on a live answer and on a replayed one.
func (r *vmRun) decodeJSON(encoded string) goja.Value {
	if strings.TrimSpace(encoded) == "" || r.jsonParse == nil {
		return goja.Null()
	}
	v, err := r.jsonParse(goja.Undefined(), r.vm.ToValue(encoded))
	if err != nil {
		return r.vm.ToValue(encoded)
	}
	return v
}
