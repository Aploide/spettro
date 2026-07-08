package agent

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"

	"spettro/internal/config"
)

// loopAction is the detector's verdict after observing one LLM step.
type loopAction int

const (
	// loopOK: no repetition detected, keep going.
	loopOK loopAction = iota
	// loopNudge: repetition detected for the first time; inject a system
	// nudge telling the agent to change approach.
	loopNudge
	// loopAbort: repetition detected again after a nudge; stop the turn.
	loopAbort
)

// loopStopMessage is the user-facing message when a run is stopped because
// the agent kept repeating itself after being nudged.
const loopStopMessage = "Stopped: the agent was repeating the same actions without making progress. Rephrase the task or narrow its scope and try again."

// loopNudgeMessage is injected into the conversation on first detection.
const loopNudgeMessage = "system: you appear to be repeating the same action or output. Do not repeat it again — change your approach: re-read the relevant context, try a different tool or different arguments, or explain why you are stuck."

// loopDetector tracks a rolling window of recent tool calls (name + normalized
// args hash) and consecutive assistant text outputs to spot a stuck agent.
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
	nudged      bool
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

// callSignature normalizes a tool call to "name\x00hash(args)". JSON args are
// compacted first so whitespace differences don't defeat detection.
func callSignature(name string, args json.RawMessage) string {
	norm := bytes.TrimSpace(args)
	var buf bytes.Buffer
	if json.Compact(&buf, norm) == nil {
		norm = buf.Bytes()
	}
	h := sha256.Sum256(norm)
	return name + "\x00" + hex.EncodeToString(h[:])
}

// observe records one LLM step (its tool calls and assistant text) and
// returns the action to take. The first trip returns loopNudge and resets the
// counters so the agent gets a fresh chance; a second trip returns loopAbort.
func (d *loopDetector) observe(calls []toolCall, text string) loopAction {
	if d == nil || !d.enabled {
		return loopOK
	}
	tripped := d.recordText(text)
	for _, c := range calls {
		if d.recordCall(callSignature(c.Tool, c.Args)) {
			tripped = true
		}
	}
	if !tripped {
		return loopOK
	}
	if d.nudged {
		return loopAbort
	}
	d.nudged = true
	d.reset()
	return loopNudge
}

func (d *loopDetector) recordCall(sig string) bool {
	if sig == d.lastSig {
		d.consecutive++
	} else {
		d.lastSig = sig
		d.consecutive = 1
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

// reset clears the repetition counters (kept thresholds and nudged flag) so a
// nudged agent is judged on fresh behavior.
func (d *loopDetector) reset() {
	d.window = d.window[:0]
	d.lastSig = ""
	d.consecutive = 0
	d.lastText = ""
	d.textRepeats = 0
}
