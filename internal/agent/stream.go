package agent

import "strings"

// Stream chunk kinds delivered to the UI while a run is in progress.
const (
	StreamKindThinking = "thinking"
	StreamKindAnswer   = "answer"
)

// StreamChunk is an incremental UI update emitted while the model streams. It is
// already demultiplexed from the raw provider deltas: protocol markers
// (TOOL_CALL/FINAL) and <think> tags have been resolved into clean thinking and
// answer text.
type StreamChunk struct {
	Kind  string // StreamKindThinking or StreamKindAnswer
	Delta string
	// Reset, on an answer chunk, tells the UI to discard the current live answer
	// draft before applying Delta. Emitted at step boundaries and when a step
	// turns out to be a tool call (whose prose is not shown as an answer).
	Reset bool
}

// StreamCallback receives demultiplexed stream chunks. It is invoked on the
// agent goroutine; implementations must not block.
type StreamCallback func(StreamChunk)

// streamDemux converts the raw text/reasoning deltas of a single LLM step into
// clean thinking/answer chunks. The text channel still carries the tool-loop
// protocol (TOOL_CALL/FINAL) and may contain inline <think>...</think> tags, so
// the demux strips both before surfacing answer text. It is single-goroutine;
// construct one per step.
type streamDemux struct {
	cb       StreamCallback
	inThink  bool            // currently inside a <think>...</think> span
	tagCarry string          // text held back to detect a tag boundary across deltas
	pending  strings.Builder // outside-think text awaiting classification
	decided  int             // 0 = undecided, 1 = answer, 2 = suppressed (tool step)
}

func newStreamDemux(cb StreamCallback) *streamDemux { return &streamDemux{cb: cb} }

// reasoning forwards a native reasoning/extended-thinking delta as thinking.
func (d *streamDemux) reasoning(delta string) {
	if d == nil || d.cb == nil || delta == "" {
		return
	}
	d.cb(StreamChunk{Kind: StreamKindThinking, Delta: delta})
}

// text feeds a visible-text delta through the <think> splitter and answer
// classifier.
func (d *streamDemux) text(delta string) {
	if d == nil || d.cb == nil || delta == "" {
		return
	}
	s := d.tagCarry + delta
	d.tagCarry = ""
	for s != "" {
		if d.inThink {
			idx := strings.Index(s, "</think>")
			if idx < 0 {
				if tail := partialTagSuffix(s, "</think>"); tail != "" {
					d.emitThinking(s[:len(s)-len(tail)])
					d.tagCarry = tail
					return
				}
				d.emitThinking(s)
				return
			}
			d.emitThinking(s[:idx])
			s = s[idx+len("</think>"):]
			d.inThink = false
			continue
		}
		idx := strings.Index(s, "<think>")
		if idx < 0 {
			if tail := partialTagSuffix(s, "<think>"); tail != "" {
				d.outside(s[:len(s)-len(tail)])
				d.tagCarry = tail
				return
			}
			d.outside(s)
			return
		}
		d.outside(s[:idx])
		s = s[idx+len("<think>"):]
		d.inThink = true
	}
}

// flush resolves anything still buffered when the step's stream ends.
func (d *streamDemux) flush() {
	if d == nil || d.cb == nil {
		return
	}
	if d.tagCarry != "" {
		carry := d.tagCarry
		d.tagCarry = ""
		if d.inThink {
			d.emitThinking(carry)
		} else {
			d.outside(carry)
		}
	}
	if d.decided == 0 {
		d.classify(true)
	}
}

func (d *streamDemux) emitThinking(s string) {
	if s == "" {
		return
	}
	d.cb(StreamChunk{Kind: StreamKindThinking, Delta: s})
}

// outside handles text outside any <think> span: it classifies the step as an
// answer or a (suppressed) tool call, then streams answer text.
func (d *streamDemux) outside(s string) {
	if s == "" {
		return
	}
	switch d.decided {
	case 2: // tool step — answer text is not shown
		return
	case 1: // already streaming the answer
		d.cb(StreamChunk{Kind: StreamKindAnswer, Delta: s})
		return
	}
	d.pending.WriteString(s)
	d.classify(false)
}

// classify decides, from the buffered leading text, whether this step is a tool
// call (suppress), a FINAL answer (strip the marker), or plain prose. While the
// leading token could still grow into a marker it waits for more input, unless
// final is set (end of stream).
func (d *streamDemux) classify(final bool) {
	raw := d.pending.String()
	trimmed := strings.TrimLeft(raw, " \t\r\n")
	if trimmed == "" {
		if final {
			d.pending.Reset()
		}
		return
	}

	tokEnd := strings.IndexAny(trimmed, " \t\r\n")
	haveFullTok := tokEnd >= 0
	tok := trimmed
	if haveFullTok {
		tok = trimmed[:tokEnd]
	}

	if !final && !haveFullTok {
		// The first token is still arriving. Keep waiting while it could become
		// a TOOL_CALL/FINAL marker, or while it is shorter than the longest
		// marker (so we don't prematurely stream a marker's first characters).
		if markerPrefix(tok) || len(trimmed) < len(toolCallPrefix) {
			return
		}
	}

	switch {
	case tok == toolCallPrefix:
		d.decided = 2
		d.pending.Reset()
		d.cb(StreamChunk{Kind: StreamKindAnswer, Reset: true})
	case tok == finalPrefix || tok == finalPrefix+":":
		d.decided = 1
		rest := strings.TrimSpace(strings.TrimPrefix(trimmed, tok))
		d.pending.Reset()
		if rest != "" {
			d.cb(StreamChunk{Kind: StreamKindAnswer, Delta: rest})
		}
	default:
		d.decided = 1
		d.pending.Reset()
		d.cb(StreamChunk{Kind: StreamKindAnswer, Delta: trimmed})
	}
}

// markerPrefix reports whether tok is a (possibly partial) prefix of one of the
// protocol markers.
func markerPrefix(tok string) bool {
	return strings.HasPrefix(toolCallPrefix, tok) || strings.HasPrefix(finalPrefix, tok)
}

// partialTagSuffix returns the longest non-empty suffix of s that is a proper
// prefix of tag, i.e. the part of a tag that may be completed by the next delta.
func partialTagSuffix(s, tag string) string {
	max := len(tag) - 1
	if max > len(s) {
		max = len(s)
	}
	for n := max; n >= 1; n-- {
		if strings.HasPrefix(tag, s[len(s)-n:]) {
			return s[len(s)-n:]
		}
	}
	return ""
}
