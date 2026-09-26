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

// res is the single successful result of a one-call step.
func res(output string) []parallelResult {
	return []parallelResult{{status: "success", output: output}}
}

func TestLoopDetectorSameCallSameResultNudgesThenAborts(t *testing.T) {
	d := newLoopDetector(config.LoopDetectionPolicy{})
	step := func() loopAction {
		return d.observe(call("shell", `{"cmd":"go test"}`), res("FAIL: TestX"), "")
	}
	var got []loopAction
	for range hardLoopRepeats {
		got = append(got, step())
	}
	// Trips at the 3rd and 6th identical (call, result) pair nudge; the 8th
	// back-to-back repeat aborts regardless of the nudge budget.
	want := []loopAction{loopOK, loopOK, loopNudge, loopOK, loopOK, loopNudge, loopOK, loopAbort}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("step %d: got %v, want %v (all: %v)", i+1, got[i], want[i], got)
		}
	}
}

// The edit→test cycle: the same test command re-run after each edit, with a
// changing result, is progress and must never trip the detector.
func TestLoopDetectorSameCallChangingResultIsProgress(t *testing.T) {
	d := newLoopDetector(config.LoopDetectionPolicy{})
	for i := range 30 {
		if got := d.observe(call("shell", `{"cmd":"go test ./..."}`), res(fmt.Sprintf("FAIL: %d tests failing", 30-i)), ""); got != loopOK {
			t.Fatalf("run %d: got %v, want loopOK", i, got)
		}
		if got := d.observe(call("file-edit", fmt.Sprintf(`{"path":"a.go","old":"x%d","new":"y%d"}`, i, i)), res("ok"), ""); got != loopOK {
			t.Fatalf("edit %d: got %v, want loopOK", i, got)
		}
	}
}

func TestLoopDetectorVolatileOutputStillMatches(t *testing.T) {
	d := newLoopDetector(config.LoopDetectionPolicy{})
	outs := []string{"--- FAIL: TestX (0.01s)\nFAIL pkg 0.123s", "--- FAIL: TestX (0.02s)\nFAIL pkg 0.456s", "--- FAIL: TestX (0.03s)\nFAIL pkg 1.2s"}
	var last loopAction
	for _, o := range outs {
		last = d.observe(call("shell", `{"cmd":"go test"}`), res(o), "")
	}
	if last != loopNudge {
		t.Fatalf("durations must not hide an identical failure: got %v, want loopNudge", last)
	}
}

func TestLoopDetectorResultStatusMatters(t *testing.T) {
	d := newLoopDetector(config.LoopDetectionPolicy{})
	for i := range 6 {
		status := "success"
		if i%2 == 0 {
			status = "error"
		}
		got := d.observe(call("shell", `{"cmd":"flaky"}`), []parallelResult{{status: status, output: "same"}}, "")
		// Alternating outcomes are never consecutive repeats; the window
		// threshold (5) is not reached by either outcome within 6 calls.
		if got != loopOK {
			t.Fatalf("call %d: got %v, want loopOK", i, got)
		}
	}
}

func TestLoopDetectorArgsNormalization(t *testing.T) {
	d := newLoopDetector(config.LoopDetectionPolicy{})
	// Same JSON args with different whitespace must count as identical.
	d.observe(call("file-read", `{"path":"a.go"}`), res("package a"), "")
	d.observe(call("file-read", `{ "path" : "a.go" }`), res("package a"), "")
	if got := d.observe(call("file-read", "{\n  \"path\": \"a.go\"\n}"), res("package a"), ""); got != loopNudge {
		t.Fatalf("got %v, want loopNudge", got)
	}
}

func TestLoopDetectorWindowRepeatsAbortAfterMaxNudges(t *testing.T) {
	d := newLoopDetector(config.LoopDetectionPolicy{WindowRepeatThreshold: 3})
	// Non-consecutive recurrence of the same call within the window.
	var actions []loopAction
	for i := 0; i < 100; i++ {
		a := d.observe(call("grep", `{"q":"foo"}`), res("no matches"), "")
		actions = append(actions, a)
		if a == loopAbort {
			break
		}
		d.observe(call("file-read", fmt.Sprintf(`{"path":"f%d.go"}`, i)), res(fmt.Sprintf("file %d", i)), "")
	}
	nudges := 0
	for _, a := range actions {
		if a == loopNudge {
			nudges++
		}
	}
	if actions[len(actions)-1] != loopAbort {
		t.Fatalf("sustained window repetition must eventually abort; got %v", actions)
	}
	if nudges != maxLoopNudges {
		t.Fatalf("got %d nudges before abort, want %d", nudges, maxLoopNudges)
	}
}

// A nudge is forgiven after enough clean calls: a much later, unrelated
// repetition starts over from a nudge instead of aborting the run.
func TestLoopDetectorNudgesClearAfterProgress(t *testing.T) {
	d := newLoopDetector(config.LoopDetectionPolicy{})
	repeat := func(cmd string) loopAction {
		var a loopAction
		for range 3 {
			a = d.observe(call("shell", fmt.Sprintf(`{"cmd":%q}`, cmd)), res("same"), "")
		}
		return a
	}
	for round := range maxLoopNudges + 2 {
		if got := repeat(fmt.Sprintf("cmd-%d", round)); got != loopNudge {
			t.Fatalf("round %d: got %v, want loopNudge (nudges must clear after progress)", round, got)
		}
		for i := range loopClearAfter {
			if got := d.observe(call("file-read", fmt.Sprintf(`{"path":"r%d-%d.go"}`, round, i)), res("x"), ""); got != loopOK {
				t.Fatalf("round %d clean call %d: got %v", round, i, got)
			}
		}
	}
}

func TestLoopDetectorDistinctCallsNeverTrip(t *testing.T) {
	d := newLoopDetector(config.LoopDetectionPolicy{})
	for i := range 50 {
		got := d.observe(call("file-read", fmt.Sprintf(`{"path":"f%d.go"}`, i)), res("x"), fmt.Sprintf("reading file %d", i))
		if got != loopOK {
			t.Fatalf("step %d: got %v, want loopOK", i, got)
		}
	}
}

func TestLoopDetectorRepeatedText(t *testing.T) {
	d := newLoopDetector(config.LoopDetectionPolicy{})
	d.observe(nil, nil, "I will now fix the bug.")
	d.observe(nil, nil, "I will now fix the bug.")
	if got := d.observe(nil, nil, "I will now fix the bug."); got != loopNudge {
		t.Fatalf("got %v, want loopNudge", got)
	}
	// Changing the text after the nudge keeps the run alive.
	if got := d.observe(nil, nil, "Trying a different approach."); got != loopOK {
		t.Fatalf("got %v, want loopOK", got)
	}
}

func TestLoopDetectorDisabled(t *testing.T) {
	d := newLoopDetector(config.LoopDetectionPolicy{Disabled: true})
	if d != nil {
		t.Fatal("disabled policy must return a nil detector")
	}
	// A nil detector must be safe to observe and never trip.
	for range 10 {
		if got := d.observe(call("shell", `{"cmd":"x"}`), res("x"), "same text"); got != loopOK {
			t.Fatalf("got %v, want loopOK", got)
		}
	}
}

func TestLoopDetectorCustomThreshold(t *testing.T) {
	d := newLoopDetector(config.LoopDetectionPolicy{ConsecutiveThreshold: 2})
	d.observe(call("shell", `{"cmd":"ls"}`), res("a b"), "")
	if got := d.observe(call("shell", `{"cmd":"ls"}`), res("a b"), ""); got != loopNudge {
		t.Fatalf("got %v, want loopNudge at custom threshold 2", got)
	}
}
