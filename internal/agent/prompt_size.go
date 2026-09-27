package agent

import (
	"bytes"
	"slices"
	"unicode/utf8"

	"spettro/internal/provider"
)

// promptSizer computes provider.EstimateRequestTokens for the run loop's
// conversation without re-counting every message at every step.
//
// The estimate is chars/4 over the system prompt and every message's text,
// plus the tool definitions (see provider.EstimateRequestTokens). The run
// loop needs it three or four times per step (the compaction trigger, the
// input budget, the calibration sample), and at 500 messages each full
// count walked about 750 KB of text. Only the newest messages change between
// steps, so the sizer keeps each message's count and re-counts only the
// messages that differ from the ones it counted last time.
//
// Cache: msgs[i] holds the rune count of the i-th message measured last,
// together with the exact values it counted. tools and system hold the same
// for the tool definitions and the system prompt.
//
// Key and invalidation: an entry is reused only when the message at the same
// position still carries equal values. Strings are compared with ==, which
// is O(1) when both sides share storage (the common case: the loop appends
// and never copies) and a plain comparison otherwise. Tool-call arguments
// are a []byte, and are compared by identity: the same backing array and
// length. Neither the run loop nor compaction ever edits a message's
// argument bytes in place (they build new messages or new slices), so
// identity implies equal content; comparing the bytes instead would mean
// keeping a private copy of every argument for the whole run (file-write
// calls carry whole files) or hashing them all on every measurement, which
// measured 4x slower than the 110 µs a byte comparison of 4 MB takes (see
// BenchmarkPromptSizerWriteHeavy). So compaction, truncation, a replaced
// message or a replaced value re-counts exactly the messages it touched, and
// the result equals a full count.
//
// Owner: the run loop goroutine (runToolLoop). Not safe for concurrent use.
type promptSizer struct {
	system      string
	systemRunes int

	msgs []sizedMessage

	tools      []provider.ToolSpec // private copies of the counted definitions
	toolTokens int
	toolsValid bool
}

// sizedMessage is one message's counted values and their rune count.
type sizedMessage struct {
	// texts are the counted strings in countedTexts order.
	texts []string
	// args are the tool calls' arguments as counted (the caller's slices,
	// compared by identity; see promptSizer).
	args  [][]byte
	runes int
}

// requestTokens returns provider.EstimateRequestTokens for a request with
// this system prompt, history and tools (and an empty Prompt).
func (s *promptSizer) requestTokens(system string, msgs []provider.Message, tools []provider.ToolSpec) int {
	chars := s.systemRuneCount(system)
	for i := range msgs {
		if i < len(s.msgs) && s.msgs[i].matches(&msgs[i]) {
			chars += s.msgs[i].runes
			continue
		}
		rec := newSizedMessage(&msgs[i])
		if i < len(s.msgs) {
			s.msgs[i] = rec
		} else {
			s.msgs = append(s.msgs, rec)
		}
		chars += rec.runes
	}
	clear(s.msgs[len(msgs):]) // drop references to messages no longer measured
	s.msgs = s.msgs[:len(msgs)]
	return tokensForChars(chars) + s.toolTokensFor(tools)
}

// tokensForChars is budget.EstimateTokens' rounding: chars/4, plus one for
// any non-empty text.
func tokensForChars(chars int) int {
	if chars == 0 {
		return 0
	}
	return chars/4 + 1
}

func (s *promptSizer) systemRuneCount(system string) int {
	if system != s.system {
		s.system = system
		s.systemRunes = utf8.RuneCountInString(system)
	}
	return s.systemRunes
}

// toolTokensFor returns provider.EstimateToolTokens(tools), re-counting only
// when a definition changed (the tool surface changes on an activation).
func (s *promptSizer) toolTokensFor(tools []provider.ToolSpec) int {
	if s.toolsValid && sameToolSpecs(s.tools, tools) {
		return s.toolTokens
	}
	s.tools = s.tools[:0]
	for _, t := range tools {
		s.tools = append(s.tools, provider.ToolSpec{Name: t.Name, Description: t.Description, Schema: bytes.Clone(t.Schema)})
	}
	s.toolTokens = provider.EstimateToolTokens(tools)
	s.toolsValid = true
	return s.toolTokens
}

func sameToolSpecs(a, b []provider.ToolSpec) bool {
	return slices.EqualFunc(a, b, func(x, y provider.ToolSpec) bool {
		return x.Name == y.Name && x.Description == y.Description && bytes.Equal(x.Schema, y.Schema)
	})
}

// countedTextCount is how many strings countedTexts yields for m.
func countedTextCount(m *provider.Message) int {
	return 1 + len(m.Reasoning) + len(m.ToolCalls) + len(m.ToolResults)
}

// countedText returns the i-th string EstimateRequestTokens counts for m
// (tool-call arguments aside): the content, each reasoning block's text,
// each tool call's name, then each tool result's output.
func countedText(m *provider.Message, i int) string {
	if i == 0 {
		return m.Content
	}
	i--
	if i < len(m.Reasoning) {
		return m.Reasoning[i].Text
	}
	i -= len(m.Reasoning)
	if i < len(m.ToolCalls) {
		return m.ToolCalls[i].Name
	}
	i -= len(m.ToolCalls)
	return m.ToolResults[i].Output
}

func newSizedMessage(m *provider.Message) sizedMessage {
	rec := sizedMessage{texts: make([]string, countedTextCount(m))}
	for i := range rec.texts {
		rec.texts[i] = countedText(m, i)
		rec.runes += utf8.RuneCountInString(rec.texts[i])
	}
	if len(m.ToolCalls) > 0 {
		rec.args = make([][]byte, len(m.ToolCalls))
		for i, tc := range m.ToolCalls {
			rec.args[i] = tc.Args
			rec.runes += utf8.RuneCount(tc.Args)
		}
	}
	return rec
}

// matches reports whether m still carries exactly the values rec counted.
func (rec *sizedMessage) matches(m *provider.Message) bool {
	if len(rec.texts) != countedTextCount(m) || len(rec.args) != len(m.ToolCalls) {
		return false
	}
	for i, s := range rec.texts {
		if s != countedText(m, i) {
			return false
		}
	}
	for i, a := range rec.args {
		if !sameSlice(a, m.ToolCalls[i].Args) {
			return false
		}
	}
	return true
}

// sameSlice reports whether a and b are the same bytes in memory: equal
// length and, when not empty, the same first element.
func sameSlice(a, b []byte) bool {
	return len(a) == len(b) && (len(a) == 0 || &a[0] == &b[0])
}
