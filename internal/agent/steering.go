package agent

import (
	"strings"
	"sync"
)

// steeringMessagePrefix marks an injected mid-run user message so the model
// treats it as course-correction for the task in flight, not a new task.
const steeringMessagePrefix = "[user steering — mid-run guidance for the CURRENT task; apply it from your next step onward, do not restart the task]\n"

// SteeringQueue is a thread-safe queue of user guidance messages pushed while
// a run is in progress. The tool loop drains it at every step boundary (right
// before building the next LLM request) and appends each message to the
// conversation as a user turn. Appending — never rewriting — keeps the carried
// prompt prefix byte-stable, so provider prompt caching is unaffected.
//
// A single queue may outlive one run: goal mode reuses the same queue across
// iterations so a message typed between iterations is delivered to the next
// one instead of being lost.
type SteeringQueue struct {
	mu   sync.Mutex
	msgs []string
}

// NewSteeringQueue returns an empty queue.
func NewSteeringQueue() *SteeringQueue {
	return &SteeringQueue{}
}

// Push enqueues a steering message. Blank messages are ignored.
func (q *SteeringQueue) Push(msg string) {
	msg = strings.TrimSpace(msg)
	if msg == "" || q == nil {
		return
	}
	q.mu.Lock()
	q.msgs = append(q.msgs, msg)
	q.mu.Unlock()
}

// Drain removes and returns all pending messages in FIFO order.
func (q *SteeringQueue) Drain() []string {
	if q == nil {
		return nil
	}
	q.mu.Lock()
	out := q.msgs
	q.msgs = nil
	q.mu.Unlock()
	return out
}

// Len reports how many messages are waiting for delivery.
func (q *SteeringQueue) Len() int {
	if q == nil {
		return 0
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.msgs)
}
