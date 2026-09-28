package jobs

// Benchmark for spooling a tool result on the agent's step path
// (performance plan, Unit D3; corresponds to the profiling harness's
// PerfEnsureSpooled and PerfSpoolAddOnly). Add only queues the write, so this
// is the cost the step pays; the file is written in the background.

import (
	"fmt"
	"strings"
	"testing"
)

func BenchmarkSpoolAdd(b *testing.B) {
	for _, size := range []int{3000, 30000} {
		content := strings.Repeat("0123456789abcdef\n", size/17+1)[:size]
		b.Run(fmt.Sprint("out=", size), func(b *testing.B) {
			s := NewSpoolStore()
			b.Cleanup(s.Cleanup)
			b.ReportAllocs()
			n := 0
			for b.Loop() {
				if _, err := s.Add(content); err != nil {
					b.Fatal(err)
				}
				// Bound the disk the benchmark uses: every 256 entries,
				// wait for the writes and delete them (off the clock).
				if n++; n%256 == 0 {
					b.StopTimer()
					s.Cleanup()
					b.StartTimer()
				}
			}
		})
	}
}
