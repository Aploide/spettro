package agent

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"strings"

	"spettro/internal/config"
)

// loopAction is the detector's verdict after observing one LLM step.
type loopAction int

const (
	// loopOK: no repetition detected, keep going.
	loopOK loopAction = iota
	// loopNudge: repetition detected; inject a system nudge telling the
	// agent to change approach and keep going.
	loopNudge
	// loopAbort: the repetition is sustained (it kept tripping after
	// maxLoopNudges nudges, or the same call produced the same result
	// hardLoopRepeats times in a row); stop the turn.
	loopAbort
)

const (
	// maxLoopNudges is how many nudges a run gets before the next trip
	// aborts it.
	maxLoopNudges = 3
	// hardLoopRepeats aborts regardless of the remaining nudge budget once
	// the very same (call, result) pair repeats this many times back to back
	// (after at least one nudge).
	hardLoopRepeats = 8
	// loopClearAfter forgives earlier nudges once this many consecutive tool
	// calls went by without a trip: the agent recovered, and a much later,
	// unrelated repetition should start from a nudge again, not an abort.
	loopClearAfter = 10
)

// loopStopMessage is the user-facing message when a run is stopped because
// the agent kept repeating itself after being nudged.
const loopStopMessage = "Stopped: the agent was repeating the same actions without making progress. Rephrase the task or narrow its scope and try again."

// loopNudgeMessage is injected into the conversation on detection.
const loopNudgeMessage = "system: you appear to be repeating the same action and getting the same result. Do not repeat it again — change your approach: re-read the relevant context, try a different tool or different arguments, or explain why you are stuck."

// loopDetector tracks a rolling window of recent tool-call outcomes and
// consecutive assistant text outputs to spot a stuck agent.
//
// A tool call's signature covers the call (name + normalized args) AND its
// result (status + normalized output hash). Re-running the same test command
// while its output changes is progress (the edit→test cycle), not a loop;
// only the same call producing the same result again counts as repetition.
// It is used from a single goroutine (the run loop); no locking needed.
type loopDetector struct {
	enabled               bool
	consecutiveThreshold  int
	windowSize            int
	windowRepeatThreshold int
	textRepeatThreshold   int

	window      []string // ring of recent call signatures, newest last
	lastSig     string
	consecutive int
	lastText    string
	textRepeats int

	// identicalRun counts back-to-back identical signatures and, unlike
	// consecutive, survives a nudge (see hardLoopRepeats).
	identicalRun int
	// nudges given since the last clear; cleanCalls counts calls since the
	// last trip (see loopClearAfter).
	nudges     int
	cleanCalls int
}

// newLoopDetector builds a detector from the manifest policy, applying
// built-in defaults for zero thresholds. Returns nil when disabled.
func newLoopDetector(p config.LoopDetectionPolicy) *loopDetector {
	if p.Disabled {
		return nil
	}
	d := &loopDetector{
		enabled:               true,
		consecutiveThreshold:  p.ConsecutiveThreshold,
		windowSize:            p.WindowSize,
		windowRepeatThreshold: p.WindowRepeatThreshold,
		textRepeatThreshold:   p.TextRepeatThreshold,
	}
	if d.consecutiveThreshold <= 0 {
		d.consecutiveThreshold = 3
	}
	if d.windowSize <= 0 {
		d.windowSize = 20
	}
	if d.windowRepeatThreshold <= 0 {
		d.windowRepeatThreshold = 5
	}
	if d.textRepeatThreshold <= 0 {
		d.textRepeatThreshold = 3
	}
	return d
}

// volatileOutput matches result fragments that change between otherwise
// identical runs (durations, timestamps, pointer addresses, and the spool /
// background-job ids a fresh run is always given — every oversized output's
// truncation footer carries a new "spool:N" and the file backing it,
// ".../spettro-spool-XXXX/N.txt"), so a failing test that prints
// "FAIL pkg 0.012s" then "FAIL pkg 0.015s" still hashes the same.
var volatileOutput = regexp.MustCompile(`\b\d+(?:\.\d+)?(?:ns|µs|us|ms|s|m|h)\b|\b\d{4}-\d{2}-\d{2}[T ]\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:?\d{2})?\b|\b\d{2}:\d{2}:\d{2}(?:\.\d+)?\b|0x[0-9a-fA-F]+|\bspool:\d+|spettro-spool-[^\s/\\]*[/\\]\d+\.txt|\bjob-\d+`)

// callSignature normalizes one executed tool call to
// "name\x00hash(args)\x00hash(status+output)". JSON args are compacted first
// so whitespace differences don't defeat detection.
func callSignature(name string, args json.RawMessage, status, output string) string {
	norm := bytes.TrimSpace(args)
	var buf bytes.Buffer
	if json.Compact(&buf, norm) == nil {
		norm = buf.Bytes()
	}
	ah := sha256.Sum256(norm)
	rh := sha256.Sum256([]byte(status + "\x00" + volatileOutput.ReplaceAllString(output, "#")))
	return name + "\x00" + hex.EncodeToString(ah[:8]) + "\x00" + hex.EncodeToString(rh[:8])
}

// observe records one executed LLM step — its tool calls with their results
// (results[i] belongs to calls[i]; missing results hash as empty) and the
// assistant text — and returns the action to take. Trips nudge (resetting
// the repetition counters so the agent is judged on fresh behavior) until
// maxLoopNudges nudges were spent, or hardLoopRepeats identical outcomes ran
// back to back after a nudge; then it aborts.
func (d *loopDetector) observe(calls []toolCall, results []parallelResult, text string) loopAction {
	if d == nil || !d.enabled {
		return loopOK
	}
	tripped := d.recordText(text)
	hard := false
	for i, c := range calls {
		var status, output string
		if i < len(results) {
			status, output = results[i].status, results[i].output
		}
		if d.recordCall(callSignature(c.Tool, c.Args, status, output)) {
			tripped = true
		}
		if d.identicalRun >= hardLoopRepeats {
			hard = true
		}
	}
	if !tripped && !hard {
		d.cleanCalls += len(calls)
		if d.cleanCalls >= loopClearAfter {
			d.nudges = 0
		}
		return loopOK
	}
	d.cleanCalls = 0
	// The hard limit ends the run only once the agent was warned: a single
	// response with 8+ identical parallel calls gets its nudge first.
	if (hard && d.nudges > 0) || d.nudges >= maxLoopNudges {
		return loopAbort
	}
	d.nudges++
	d.reset()
	return loopNudge
}

func (d *loopDetector) recordCall(sig string) bool {
	if sig == d.lastSig {
		d.consecutive++
		d.identicalRun++
	} else {
		d.lastSig = sig
		d.consecutive = 1
		d.identicalRun = 1
	}
	d.window = append(d.window, sig)
	if len(d.window) > d.windowSize {
		d.window = d.window[1:]
	}
	if d.consecutive >= d.consecutiveThreshold {
		return true
	}
	occurrences := 0
	for _, s := range d.window {
		if s == sig {
			occurrences++
		}
	}
	return occurrences >= d.windowRepeatThreshold
}

func (d *loopDetector) recordText(text string) bool {
	text = strings.TrimSpace(text)
	if text == "" {
		return false
	}
	if text == d.lastText {
		d.textRepeats++
	} else {
		d.lastText = text
		d.textRepeats = 1
	}
	return d.textRepeats >= d.textRepeatThreshold
}

// reset clears the repetition counters (kept: thresholds, nudge count and
// the identical-run streak) so a nudged agent is judged on fresh behavior.
// lastSig is kept too, so the identical-run streak can keep growing across
// the nudge.
func (d *loopDetector) reset() {
	d.window = d.window[:0]
	d.consecutive = 0
	d.lastText = ""
	d.textRepeats = 0
}
