package agent

import (
	"encoding/json"
	"fmt"
	"testing"

	"spettro/internal/config"
)

func call(tool, args string) []toolCall {
	return []toolCall{{Tool: tool, Args: json.RawMessage(args)}}
}

func TestLoopDetectorConsecutiveCallsNudgeThenAbort(t *testing.T) {
	d := newLoopDetector(config.LoopDetectionPolicy{})
	// Two identical calls: below the default threshold of 3.
	for i := 0; i < 2; i++ {
		if got := d.observe(call("shell", `{"cmd":"go test"}`), ""); got != loopOK {
			t.Fatalf("step %d: got %v, want loopOK", i, got)
		}
	}
	// Third identical call trips the nudge.
	if got := d.observe(call("shell", `{"cmd":"go test"}`), ""); got != loopNudge {
		t.Fatalf("got %v, want loopNudge", got)
	}
	// After the nudge counters reset: it takes 3 more identical calls to trip
	// again, and the second trip aborts.
	for i := 0; i < 2; i++ {
		if got := d.observe(call("shell", `{"cmd":"go test"}`), ""); got != loopOK {
			t.Fatalf("post-nudge step %d: got %v, want loopOK", i, got)
		}
	}
	if got := d.observe(call("shell", `{"cmd":"go test"}`), ""); got != loopAbort {
		t.Fatalf("got %v, want loopAbort", got)
	}
}

func TestLoopDetectorArgsNormalization(t *testing.T) {
	d := newLoopDetector(config.LoopDetectionPolicy{})
	// Same JSON args with different whitespace must count as identical.
	d.observe(call("file-read", `{"path":"a.go"}`), "")
	d.observe(call("file-read", `{ "path" : "a.go" }`), "")
	if got := d.observe(call("file-read", "{\n  \"path\": \"a.go\"\n}"), ""); got != loopNudge {
		t.Fatalf("got %v, want loopNudge", got)
	}
}

func TestLoopDetectorWindowRepeats(t *testing.T) {
	d := newLoopDetector(config.LoopDetectionPolicy{WindowRepeatThreshold: 3})
	// Non-consecutive recurrence of the same call within the window.
	trip := loopOK
	for i := 0; i < 6 && trip == loopOK; i++ {
		trip = d.observe(call("grep", `{"q":"foo"}`), "")
		if trip == loopOK {
			trip = d.observe(call("file-read", fmt.Sprintf(`{"path":"f%d.go"}`, i)), "")
		}
	}
	if trip != loopNudge {
		t.Fatalf("got %v, want loopNudge from window recurrence", trip)
	}
}

func TestLoopDetectorDistinctCallsNeverTrip(t *testing.T) {
	d := newLoopDetector(config.LoopDetectionPolicy{})
	for i := 0; i < 50; i++ {
		got := d.observe(call("file-read", fmt.Sprintf(`{"path":"f%d.go"}`, i)), fmt.Sprintf("reading file %d", i))
		if got != loopOK {
			t.Fatalf("step %d: got %v, want loopOK", i, got)
		}
	}
}

func TestLoopDetectorRepeatedText(t *testing.T) {
	d := newLoopDetector(config.LoopDetectionPolicy{})
	d.observe(nil, "I will now fix the bug.")
	d.observe(nil, "I will now fix the bug.")
	if got := d.observe(nil, "I will now fix the bug."); got != loopNudge {
		t.Fatalf("got %v, want loopNudge", got)
	}
	// Changing the text after the nudge keeps the run alive.
	if got := d.observe(nil, "Trying a different approach."); got != loopOK {
		t.Fatalf("got %v, want loopOK", got)
	}
}

func TestLoopDetectorDisabled(t *testing.T) {
	d := newLoopDetector(config.LoopDetectionPolicy{Disabled: true})
	if d != nil {
		t.Fatal("disabled policy must return a nil detector")
	}
	// A nil detector must be safe to observe and never trip.
	for i := 0; i < 10; i++ {
		if got := d.observe(call("shell", `{"cmd":"x"}`), "same text"); got != loopOK {
			t.Fatalf("got %v, want loopOK", got)
		}
	}
}

func TestLoopDetectorCustomThreshold(t *testing.T) {
	d := newLoopDetector(config.LoopDetectionPolicy{ConsecutiveThreshold: 2})
	d.observe(call("shell", `{"cmd":"ls"}`), "")
	if got := d.observe(call("shell", `{"cmd":"ls"}`), ""); got != loopNudge {
		t.Fatalf("got %v, want loopNudge at custom threshold 2", got)
	}
}
