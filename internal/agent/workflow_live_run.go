package agent

import (
	"sync"
	"time"
)

// liveWorkflow is one run held across tool calls. The fields below are the
// minimum the registry needs; the workflow tool fills in the rest.
type liveWorkflow struct {
	runID string
	name  string

	mu         sync.Mutex
	paused     bool
	checkpoint string
	message    string
	pausedAt   time.Time
	cancel     func()
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
