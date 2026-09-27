package agent

// Benchmarks for the loop detector's per-tool-result signature (performance
// plan, Unit D1; corresponds to the profiling harness's
// BenchmarkPerfCallSignature). The oracle variant is the regex version the
// normalizer replaced, kept for comparison.

import (
	"encoding/json"
	"fmt"
	"testing"
)

func BenchmarkCallSignature(b *testing.B) {
	args := json.RawMessage(`{"command":"go test ./...","timeout":30}`)
	for _, size := range []int{3000, 30000} {
		log := loopTestLog(size/60 + 1)[:size]
		listing := loopTestGoListing(size/50 + 1)[:size]
		b.Run(fmt.Sprintf("testlog=%d", size), func(b *testing.B) {
			b.SetBytes(int64(size))
			b.ReportAllocs()
			for b.Loop() {
				_ = callSignature("bash", args, "success", log)
			}
		})
		b.Run(fmt.Sprintf("listing=%d", size), func(b *testing.B) {
			b.SetBytes(int64(size))
			b.ReportAllocs()
			for b.Loop() {
				_ = callSignature("file-read", args, "success", listing)
			}
		})
	}
}

func BenchmarkCallSignatureRegexOracle(b *testing.B) {
	args := json.RawMessage(`{"command":"go test ./...","timeout":30}`)
	log := loopTestLog(501)[:30000]
	b.SetBytes(int64(len(log)))
	b.ReportAllocs()
	for b.Loop() {
		_ = oracleCallSignature("bash", args, "success", log)
	}
}
