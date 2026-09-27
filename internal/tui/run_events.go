package tui

import (
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"

	"spettro/internal/agent"
)

// runEvent is one item a running agent reports to the UI: a stream chunk or
// a tool trace (exactly one is set).
type runEvent struct {
	chunk *agent.StreamChunk
	trace *agent.ToolTrace
}

// runEventsMsg delivers every run event queued since the last delivery, in
// the order the agent produced them. queue identifies the run: a batch from
// a run that was stopped is dropped even if another run has started since.
type runEventsMsg struct {
	queue  *runEventQueue
	events []runEvent
}

// runEventQueue carries a run's stream chunks and tool traces from the agent
// goroutine to the Bubble Tea Update goroutine.
//
// Ordering guarantee: events are delivered in the order push was called,
// across both kinds, so a tool trace never overtakes the answer text that
// preceded it (two channels, as before, gave no such order). Every event
// pushed before close is delivered.
//
// push never blocks: the queue is an unbounded slice under a mutex, so the
// agent never waits on rendering (with the old 256-slot channel, a UI busy
// rendering made the model's stream back up into the agent). Memory is
// bounded by what the run produces between two deliveries.
//
// Delivery is batched: waitForRunEvents takes everything queued at once, so
// when rendering falls behind, the chunks that arrived meanwhile are applied
// together and cost one refresh and one frame instead of one each. That is
// the frame budget: it adapts to the render cost without a timer, and adds
// no latency when the UI keeps up.
//
// The run's done message comes after its events: the run's command closes
// the queue and waits (waitDrained) until the reader has found it closed and
// empty. The reader only asks again after Update applied the previous batch
// (the handler re-arms it), so by then every event is on screen, and a tool
// row can no longer be left "running" by a trace that arrived after the
// run ended.
//
// Concurrency: push and close run on agent goroutines, take on the one
// goroutine running waitForRunEvents' command. mu guards items and closed;
// wake (capacity 1) only signals that something changed; drained is closed
// once, by the reader, when it has delivered everything.
type runEventQueue struct {
	mu        sync.Mutex
	items     []runEvent
	closed    bool
	wake      chan struct{}
	drained   chan struct{}
	drainOnce sync.Once
}

func newRunEventQueue() *runEventQueue {
	return &runEventQueue{wake: make(chan struct{}, 1), drained: make(chan struct{})}
}

// waitDrained blocks until the reader has delivered every event and found
// the queue closed, or until timeout: a UI that stopped reading (the run was
// interrupted) must not hold the run's goroutine forever.
func (q *runEventQueue) waitDrained(timeout time.Duration) {
	select {
	case <-q.drained:
	case <-time.After(timeout):
	}
}

// push queues ev. Events pushed after close are dropped: the run is over and
// nothing reads them.
func (q *runEventQueue) push(ev runEvent) {
	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()
		return
	}
	q.items = append(q.items, ev)
	q.mu.Unlock()
	q.signal()
}

// close marks the end of the run; the reader returns once it has delivered
// everything queued before it.
func (q *runEventQueue) close() {
	q.mu.Lock()
	q.closed = true
	q.mu.Unlock()
	q.signal()
}

func (q *runEventQueue) signal() {
	select {
	case q.wake <- struct{}{}:
	default: // a wake-up is already pending
	}
}

// take removes and returns everything queued, and whether the queue is
// closed and drained.
func (q *runEventQueue) take() ([]runEvent, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	items := q.items
	q.items = nil
	return items, q.closed && len(items) == 0
}

// waitForRunEvents waits for the next batch of run events. It returns nil
// once the queue is closed and drained, which ends the chain of reads.
func waitForRunEvents(q *runEventQueue) tea.Cmd {
	return func() tea.Msg {
		for {
			items, done := q.take()
			if len(items) > 0 {
				return runEventsMsg{queue: q, events: items}
			}
			if done {
				q.drainOnce.Do(func() { close(q.drained) })
				return nil
			}
			<-q.wake
		}
	}
}
