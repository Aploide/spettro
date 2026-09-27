package agent

// Benchmarks for the per-step prompt estimate (performance plan, Unit D2;
// corresponds to the profiling harness's PerfEstimateRequestTokens). The run
// loop measures its request three or four times per step; before the sizer
// each measurement was a full count of the history.

import (
	"testing"

	"spettro/internal/provider"
)

// BenchmarkPromptEstimateFull is the old per-measurement cost: a full count
// of a 500-message history.
func BenchmarkPromptEstimateFull(b *testing.B) {
	msgs := sizerTestHistory(500, 1500)
	b.ReportAllocs()
	for b.Loop() {
		_ = provider.EstimateRequestTokens(provider.Request{System: "system", Messages: msgs, Tools: sizerTestTools})
	}
}

// BenchmarkPromptSizerStep is one run-loop step with the sizer: two new
// messages appended, then the history measured.
func BenchmarkPromptSizerStep(b *testing.B) {
	base := sizerTestHistory(500, 1500)
	extra := sizerTestHistory(3, 1500)[1:]
	var s promptSizer
	msgs := append(make([]provider.Message, 0, len(base)+2), base...)
	s.requestTokens("system", msgs, sizerTestTools)
	b.ReportAllocs()
	for b.Loop() {
		msgs = append(msgs[:len(base)], extra...)
		_ = s.requestTokens("system", msgs, sizerTestTools)
		msgs = msgs[:len(base)]
		_ = s.requestTokens("system", msgs, sizerTestTools)
	}
}

// BenchmarkPromptSizerUnchanged is a re-measurement of an unchanged
// 500-message history (the compaction trigger, then the budget check).
func BenchmarkPromptSizerUnchanged(b *testing.B) {
	msgs := sizerTestHistory(500, 1500)
	var s promptSizer
	s.requestTokens("system", msgs, sizerTestTools)
	b.ReportAllocs()
	for b.Loop() {
		_ = s.requestTokens("system", msgs, sizerTestTools)
	}
}
