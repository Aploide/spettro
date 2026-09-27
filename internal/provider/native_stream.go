package provider

import (
	"cmp"
	"strconv"

	"charm.land/fantasy"

	wire "spettro/internal/provider/wire/chatcompletions"
)

// compatStream turns decoded chat-completion chunks into streamCollector
// calls, following fantasy's OpenAI-compatible stream handling rule for
// rule, so a reply decodes to the same Response on both clients:
//
//   - a tool call starts at the first fragment of a new index that has a
//     name or arguments (Ollama sends empty ones, which are skipped), with
//     "tool-call-<index>" as its ID when the server sends none; a type
//     other than "function" fails the stream;
//   - a call completes at the first fragment boundary where its arguments
//     are valid JSON; later fragments of it are ignored (wire.ToolArgs does
//     this in linear time, fantasy re-validates the whole text on every
//     fragment);
//   - at the end, a call that never completed completes with "{}" when it
//     received no arguments, or with its arguments when they are valid;
//   - reasoning_content deltas form one reasoning block per choice;
//   - usage is that of the last chunk (zero when it carries none, as
//     fantasy reports it); the finish reason is the last one sent, reported
//     as tool-calls whenever the reply contained a tool call fragment (see
//     observeForFinish).
//
// One deliberate difference: fantasy drops tool-call fragments that share a
// delta with text, while this handles both.
//
// It is used by the goroutine reading the stream.
type compatStream struct {
	c     *streamCollector
	calls map[int64]*compatToolCall
	// order lists the calls by first appearance, for the end-of-stream
	// pass.
	order        []*compatToolCall
	finishReason string
	usage        fantasy.Usage
	// accID and sawToolCalls reproduce the part of the SDK's stream
	// accumulator fantasy consults for the finish reason.
	accID        string
	sawToolCalls bool
}

// compatToolCall is one streamed tool call, by delta index.
type compatToolCall struct {
	id, name string
	args     wire.ToolArgs
}

func newCompatStream(c *streamCollector) *compatStream {
	return &compatStream{c: c, calls: map[int64]*compatToolCall{}}
}

// chunk applies one decoded chunk.
func (s *compatStream) chunk(ch *wire.Chunk) error {
	s.usage = compatUsage(ch.Usage)
	s.observeForFinish(ch)
	for pos, choice := range ch.Choices {
		if choice.FinishReason != "" {
			s.finishReason = choice.FinishReason
		}
		if choice.Delta.Content != "" {
			s.c.textDelta(choice.Delta.Content)
		}
		for _, td := range choice.Delta.ToolCalls {
			if err := s.toolDelta(td); err != nil {
				return err
			}
		}
		if rc := choice.Delta.ReasoningContent; rc != "" {
			s.c.reasoning(reasoningPartID(pos), rc, "", "", true)
		}
	}
	return nil
}

// observeForFinish tracks whether the reply contains a tool call the way
// the SDK's ChatCompletionAccumulator records one: a tool_calls entry in
// choice 0 of a chunk whose ID matches the first chunk's (chunks with
// another ID are not accumulated at all). fantasy reports the finish
// reason as tool-calls when there is one, whatever the server said.
func (s *compatStream) observeForFinish(ch *wire.Chunk) {
	if s.accID == "" {
		s.accID = ch.ID
	} else if s.accID != ch.ID {
		return
	}
	for _, choice := range ch.Choices {
		if choice.Index == 0 && len(choice.Delta.ToolCalls) > 0 {
			s.sawToolCalls = true
		}
	}
}

// toolDelta applies one tool-call fragment.
func (s *compatStream) toolDelta(td wire.ToolCallDelta) error {
	if tc, ok := s.calls[td.Index]; ok {
		if tc.args.Finished() {
			return nil
		}
		s.c.toolInputDelta(tc.id, td.Function.Arguments)
		if tc.args.Add(td.Function.Arguments) {
			s.c.toolCallComplete(tc.id, tc.name, tc.args.String())
		}
		return nil
	}
	if td.Function.Name == "" && td.Function.Arguments == "" {
		return nil
	}
	if cmp.Or(td.Type, "function") != "function" {
		return &fantasy.Error{Title: "invalid provider response", Message: "expected 'function' type."}
	}
	tc := &compatToolCall{id: cmp.Or(td.ID, toolCallIDForIndex(td.Index)), name: td.Function.Name}
	s.calls[td.Index] = tc
	s.order = append(s.order, tc)
	s.c.toolCall(tc.id, tc.name)
	if td.Function.Arguments != "" {
		s.c.toolInputDelta(tc.id, td.Function.Arguments)
		if tc.args.Add(td.Function.Arguments) {
			s.c.toolCallComplete(tc.id, tc.name, tc.args.String())
		}
	}
	return nil
}

// end finishes a stream that ended cleanly: open calls are settled and the
// finish part recorded.
func (s *compatStream) end() {
	for _, tc := range s.order {
		switch {
		case tc.args.Finished():
		case tc.args.Len() == 0:
			s.c.toolCallComplete(tc.id, tc.name, "{}")
		case tc.args.Valid():
			s.c.toolCallComplete(tc.id, tc.name, tc.args.String())
		}
	}
	reason := compatFinishReason(s.finishReason)
	if s.sawToolCalls {
		reason = fantasy.FinishReasonToolCalls
	}
	s.c.finishPart(s.usage, reason)
}

// compatFinishReason maps a chat-completions finish_reason like fantasy.
func compatFinishReason(reason string) fantasy.FinishReason {
	switch reason {
	case "stop":
		return fantasy.FinishReasonStop
	case "length":
		return fantasy.FinishReasonLength
	case "content_filter":
		return fantasy.FinishReasonContentFilter
	case "function_call", "tool_calls":
		return fantasy.FinishReasonToolCalls
	}
	return fantasy.FinishReasonUnknown
}

// compatUsage maps a chunk's usage like fantasy: zero when the chunk
// carries none, and cached prompt tokens split out of the input count.
func compatUsage(u wire.Usage) fantasy.Usage {
	if u.TotalTokens == 0 {
		return fantasy.Usage{}
	}
	cached := u.PromptTokensDetails.CachedTokens
	return fantasy.Usage{
		InputTokens:     max(u.PromptTokens-cached, 0),
		OutputTokens:    u.CompletionTokens,
		TotalTokens:     u.TotalTokens,
		ReasoningTokens: u.CompletionTokensDetails.ReasoningTokens,
		CacheReadTokens: cached,
	}
}

// reasoningPartID is the stream part ID fantasy gives the reasoning of the
// choice at position pos.
func reasoningPartID(pos int) string {
	if pos == 0 {
		return "0"
	}
	return strconv.Itoa(pos)
}
