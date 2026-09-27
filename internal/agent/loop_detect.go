package agent

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"hash"
	"slices"
	"strings"

	"spettro/internal/config"
	"spettro/internal/provider"
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

// callSignature normalizes one executed tool call to
// "name\x00hash(args)\x00hash(status+output)". JSON args are compacted first
// so whitespace differences don't defeat detection. The output is hashed
// with its volatile fragments (durations, timestamps, pointer addresses, and
// the spool / background-job ids a fresh run is always given: every
// oversized output's truncation footer carries a new "spool:N" and the file
// backing it, ".../spettro-spool-XXXX/N.txt") replaced by "#", so a failing
// test that prints "FAIL pkg 0.012s" then "FAIL pkg 0.015s" still hashes the
// same (see normalizeVolatile). The normalized text is streamed into the
// hash, never built.
func callSignature(name string, args json.RawMessage, status, output string) string {
	norm := bytes.TrimSpace(args)
	var buf bytes.Buffer
	if json.Compact(&buf, norm) == nil {
		norm = buf.Bytes()
	}
	ah := sha256.Sum256(norm)

	rh := newStringHasher()
	rh.writeString(status)
	rh.writeString("\x00")
	normalizeVolatile(output, rh.writeString)
	rsum := rh.sum()

	var ahex, rhex [16]byte
	hex.Encode(ahex[:], ah[:8])
	hex.Encode(rhex[:], rsum[:8])
	return name + "\x00" + string(ahex[:]) + "\x00" + string(rhex[:])
}

// stringHasher feeds strings to a SHA-256 hash through a fixed buffer, so
// hashing a string never converts it to a fresh []byte.
type stringHasher struct {
	h   hash.Hash
	buf [512]byte
}

func newStringHasher() *stringHasher {
	return &stringHasher{h: sha256.New()}
}

func (s *stringHasher) writeString(str string) {
	for len(str) > 0 {
		n := copy(s.buf[:], str)
		s.h.Write(s.buf[:n])
		str = str[n:]
	}
}

// sum returns the hash of everything written. It reuses the copy buffer (so
// the digest needs no allocation of its own): write nothing after it.
func (s *stringHasher) sum() []byte {
	return s.h.Sum(s.buf[:0])
}

// loopCalls converts a step's native tool calls to the calls the detector
// signs. A call whose arguments could not be decoded carries "{}" and a fixed
// error text, so it is signed by its raw argument text instead: truncated
// writes to different files are different calls.
func (r *toolRuntime) loopCalls(tcs []provider.NativeTool) []toolCall {
	out := make([]toolCall, len(tcs))
	for i, tc := range tcs {
		args := tc.Args
		if tc.ArgsError != "" && tc.RawArgs != "" {
			if raw, err := json.Marshal(map[string]string{"unparsed_args": tc.RawArgs}); err == nil {
				args = raw
			}
		}
		call := toolCall{Tool: tc.Name, Args: args}
		// A retired name and its canonical tool are one action: shell-exec
		// then bash with the same command is a repeat. A name that is not
		// an alias in this run (a tool of the operator's own, an unfolded
		// built-in; see tool_names.go) is signed under its own name.
		if canon, err := r.canonicalCall(call); err == nil {
			call = canon
		}
		out[i] = call
	}
	return out
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
	sigs := make([]string, len(calls))
	progress := false
	for i, c := range calls {
		var status, output string
		if i < len(results) {
			status, output = results[i].status, results[i].output
		}
		sigs[i] = callSignature(c.Tool, c.Args, status, output)
		if sigs[i] != d.lastSig && !slices.Contains(d.window, sigs[i]) {
			progress = true
		}
	}
	// Repeated narration ("Let me run the tests again.") only signals a loop
	// when the step made no progress either: a text-only step, or calls
	// whose (call, result) pairs were all seen already.
	tripped := false
	if progress {
		d.lastText, d.textRepeats = strings.TrimSpace(text), 0
	} else {
		tripped = d.recordText(text)
	}
	hard := false
	for _, sig := range sigs {
		if d.recordCall(sig) {
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
