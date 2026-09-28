//go:build !windows

package hooks

import (
	"context"
	"testing"
	"time"
)

// A hook's timeout ends the whole command, not just its shell: a child
// still holding the output pipes must not keep Run blocked past the limit.
func TestRunEnforcesTimeoutOnChildren(t *testing.T) {
	rule := EffectiveRule{Rule: Rule{ID: "slow", Event: EventPreToolUse, Command: "sleep 20; echo done", TimeoutSec: 1}, Enabled: true}
	start := time.Now()
	_, err := Run(context.Background(), rule, RunInput{Event: EventPreToolUse, ToolID: "bash"})
	if took := time.Since(start); took > 6*time.Second {
		t.Fatalf("Run took %s with a 1s timeout", took)
	}
	if err == nil {
		t.Fatal("a timed-out hook must report an error")
	}
}

// Cancelling the caller's context (the user interrupting the run) stops a
// running hook promptly as well.
func TestRunStopsOnCancel(t *testing.T) {
	rule := EffectiveRule{Rule: Rule{ID: "slow", Event: EventPreToolUse, Command: "sleep 20; echo done", TimeoutSec: 60}, Enabled: true}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, _ = Run(ctx, rule, RunInput{Event: EventPreToolUse, ToolID: "bash"})
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("Run took %s after its context was cancelled", took)
	}
}
