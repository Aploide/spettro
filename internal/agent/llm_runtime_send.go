package agent

import (
	"context"

	"spettro/internal/provider"
)

// Loop-control limits and the synthetic user turns the run loop injects when
// a reply cannot be used as-is. The announce-only and dropped-tool-call
// nudges, for replies that would end a turn without doing anything, live in
// llm_runtime_nudge.go.
const (
	// maxEmptyReplies ends the turn with an error once this many consecutive
	// replies carried neither text nor tool calls. Earlier empty replies get
	// emptyReplyNudge appended, so the request is never resent unchanged.
	maxEmptyReplies = 3
	// maxContinuations bounds how many times a text answer cut at the output
	// token limit is continued before the partial answer is returned.
	maxContinuations = 3
)

const (
	emptyReplyNudge = "Your last response was empty. Continue the task, or give your final answer."
	// emptyTruncatedNudge replaces emptyReplyNudge when the empty reply hit the
	// output limit: the model spent its whole budget (typically thinking, or a
	// tool call too large to stream) without producing anything usable.
	emptyTruncatedNudge    = "Your last response hit the output token limit before producing any usable text or tool call. Be more concise: if you were writing a large file or edit, split it into several smaller tool calls."
	continueTruncatedNudge = "Your response was cut off at the output token limit. Continue exactly where you left off, without repeating anything you already wrote."
)

// usageCalibration corrects the local chars/4 prompt estimate with what the
// provider actually reported for the previous request. The ratio
// reported/estimated is applied to later estimates, so appended messages and
// compaction (which shrinks the estimate) are both tracked while the absolute
// level follows the provider's real tokenizer — code and JSON tokenize well
// below 4 chars/token, which made the raw estimate trigger compaction late.
type usageCalibration struct {
	reported  int // provider-reported prompt tokens of the last request
	estimated int // local estimate of that same request
}

// observe records one completed request.
func (c *usageCalibration) observe(reported, estimated int) {
	if reported <= 0 || estimated <= 0 {
		return
	}
	c.reported, c.estimated = reported, estimated
}

// apply returns the calibrated value of a local estimate. The ratio is
// clamped so one odd usage report (e.g. a proxy reporting only uncached
// tokens) cannot swing the trigger wildly.
func (c *usageCalibration) apply(estimate int) int {
	if c.reported <= 0 || c.estimated <= 0 {
		return estimate
	}
	ratio := float64(c.reported) / float64(c.estimated)
	ratio = min(max(ratio, 0.5), 3.0)
	return int(float64(estimate) * ratio)
}

// execToolCalls runs one step's tool calls. Calls whose arguments could not
// be decoded (provider.NativeTool.ArgsError: cut at the output limit, or
// unrepairable JSON) are never executed; their error text becomes the tool
// result so the model learns exactly what went wrong. The rest run through
// parallelExec. Results are returned in call order.
func (r *toolRuntime) execToolCalls(ctx context.Context, calls []provider.NativeTool, allowed map[string]struct{}, callback func(ToolTrace)) []parallelResult {
	results := make([]parallelResult, len(calls))
	runnable := make([]toolCall, 0, len(calls))
	idx := make([]int, 0, len(calls))
	for i, tc := range calls {
		if tc.ArgsError != "" {
			results[i] = parallelResult{agentID: r.traceID(), name: tc.Name, args: string(tc.Args), output: tc.ArgsError, status: "error"}
			if callback != nil {
				callback(ToolTrace{AgentID: r.traceID(), Name: tc.Name, Status: "error", Args: string(tc.Args), Output: truncate(tc.ArgsError, 600)})
			}
			continue
		}
		runnable = append(runnable, toolCall{Tool: tc.Name, Args: tc.Args})
		idx = append(idx, i)
	}
	if len(runnable) > 0 {
		for j, res := range r.parallelExec(ctx, runnable, allowed, callback) {
			results[idx[j]] = res
		}
	}
	return results
}
