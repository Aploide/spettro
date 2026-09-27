package provider

import (
	"strings"

	"charm.land/fantasy"
)

// streamCollector accumulates one streamed reply into a Response. Both
// streaming paths feed it: the fantasy SDK path translates stream parts
// into its methods, the native chat-completions client calls them straight
// from the decoded chunks. Keeping the accumulation in one place is what
// makes the two paths produce identical Responses (see
// TestNativeStreamMatchesFantasy).
//
// It is used by one goroutine, the one reading the stream.
type streamCollector struct {
	onStream func(StreamEvent)

	text strings.Builder
	// Tool calls in first-seen order, keyed by call ID. Inputs are
	// accumulated from the delta events too: the OpenAI-compatible stream
	// only reports a call complete once its arguments parse as JSON, so a
	// call cut off at the output limit (or malformed) would otherwise
	// vanish.
	calls     []*streamToolCall
	callsByID map[string]*streamToolCall
	// Reasoning blocks in first-seen order, keyed by stream part ID.
	thoughts     []*streamReasoning
	thoughtsByID map[string]*streamReasoning

	usage  fantasy.Usage
	finish FinishReason
	// finishReported is set once the provider said why the reply ended;
	// fantasy emits a Finish part on any clean EOF, with an unknown reason
	// when the stream simply stopped.
	finishReported bool
	// rawFinish and orphanDeltas feed Response.Diagnostics only.
	rawFinish    fantasy.FinishReason
	orphanDeltas int
}

// streamToolCall is a tool call being streamed. input accumulates argument
// fragments until the call completes; final then holds the arguments the
// provider reported complete.
type streamToolCall struct {
	id, name string
	input    strings.Builder
	final    string
	complete bool
}

type streamReasoning struct {
	text                strings.Builder
	signature, redacted string
}

func newStreamCollector(onStream func(StreamEvent)) *streamCollector {
	return &streamCollector{
		onStream:     onStream,
		callsByID:    map[string]*streamToolCall{},
		thoughtsByID: map[string]*streamReasoning{},
	}
}

// textDelta records a fragment of the visible answer.
func (c *streamCollector) textDelta(delta string) {
	c.text.WriteString(delta)
	if c.onStream != nil && delta != "" {
		c.onStream(StreamEvent{Kind: StreamText, Delta: delta})
	}
}

// reasoning records a fragment (or the start or end) of reasoning block id.
// emit forwards a non-empty delta to OnStream (reasoning deltas, not the
// start and end markers).
func (c *streamCollector) reasoning(id, delta, signature, redacted string, emit bool) {
	r, ok := c.thoughtsByID[id]
	if !ok {
		r = &streamReasoning{}
		c.thoughtsByID[id] = r
		c.thoughts = append(c.thoughts, r)
	}
	r.text.WriteString(delta)
	if signature != "" {
		r.signature = signature
	}
	if redacted != "" {
		r.redacted = redacted
	}
	if emit && c.onStream != nil && delta != "" {
		c.onStream(StreamEvent{Kind: StreamReasoning, Delta: delta})
	}
}

// toolCall returns the call with the given ID, introducing it (in
// first-seen order) when new. A later introduction may name an unnamed
// call.
func (c *streamCollector) toolCall(id, name string) *streamToolCall {
	if tc, ok := c.callsByID[id]; ok {
		if tc.name == "" {
			tc.name = name
		}
		return tc
	}
	tc := &streamToolCall{id: id, name: name}
	c.callsByID[id] = tc
	c.calls = append(c.calls, tc)
	return tc
}

// toolInputDelta appends an argument fragment to call id, unless the call
// already completed. A fragment for a call never introduced is counted and
// dropped.
func (c *streamCollector) toolInputDelta(id, fragment string) {
	tc, ok := c.callsByID[id]
	if !ok {
		c.orphanDeltas++
		return
	}
	if !tc.complete {
		tc.input.WriteString(fragment)
	}
}

// toolCallComplete records that call id finished with the given arguments.
func (c *streamCollector) toolCallComplete(id, name, input string) {
	tc := c.toolCall(id, name)
	tc.final = input
	tc.complete = true
}

// finishPart records the end of the reply: its usage and finish reason.
func (c *streamCollector) finishPart(usage fantasy.Usage, reason fantasy.FinishReason) {
	c.usage = usage
	c.rawFinish = reason
	c.finish = mapFinishReason(reason)
	c.finishReported = reason != "" && reason != fantasy.FinishReasonUnknown
}

// response builds the Response of a stream that ended cleanly. maxOut is
// the output cap the request carried (0 when none).
func (c *streamCollector) response(providerName, modelName string, maxOut int) Response {
	totalTokens := int(c.usage.TotalTokens)
	if totalTokens == 0 {
		totalTokens = int(c.usage.InputTokens + c.usage.OutputTokens)
	}
	raw := make([]rawToolCall, 0, len(c.calls))
	for _, tc := range c.calls {
		input := tc.final
		if !tc.complete {
			input = tc.input.String()
		}
		raw = append(raw, rawToolCall{id: tc.id, name: tc.name, input: input, complete: tc.complete})
	}
	finish := detectTruncation(c.finish, c.usage, maxOut)
	toolCalls, finish := finalizeToolCalls(raw, finish, maxOut, int(c.usage.OutputTokens))
	var reasoning []ReasoningBlock
	for _, r := range c.thoughts {
		reasoning = appendReasoning(reasoning, r.text.String(), r.signature, r.redacted, providerName, modelName)
	}
	return Response{
		Content:         c.text.String(),
		ToolCalls:       toolCalls,
		EstimatedTokens: totalTokens,
		Usage:           usageFromFantasy(c.usage),
		FinishReason:    finish,
		Reasoning:       reasoning,
		MaxOutputTokens: maxOut,
		Diagnostics:     rawDiagnostics(string(c.rawFinish), raw, c.orphanDeltas),
	}
}
