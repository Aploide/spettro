package agent

import "spettro/internal/provider"

// UsageEvent is the token accounting of one completed LLM request inside a
// run. TotalTokens and ContextTokens mirror the run-level semantics of
// RunResult (cumulative cost vs. largest-single-request occupancy) as they
// stand after this request, so a host can drive its counters and context
// gauge live instead of waiting for the run to finish.
type UsageEvent struct {
	StepTokens    int // tokens this request consumed (prompt + completion)
	TotalTokens   int // cumulative run cost so far
	ContextTokens int // approximate context occupancy so far
	// Usage is the provider-reported accounting for this request (cache
	// read/write split included); zero when the provider reports none.
	Usage provider.Usage
}

// UsageCallback receives a UsageEvent after every LLM request in a run. It is
// invoked on the agent goroutine; implementations must not block.
type UsageCallback func(UsageEvent)
