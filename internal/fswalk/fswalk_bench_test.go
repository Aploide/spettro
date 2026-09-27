package fswalk

// Benchmarks for the parallel walker (perf plan unit E: the "Walker with
// rules" harness row, and the read-ahead budget's cost). The tree is a
// synthetic 4k-file one, or the directory in FSWALK_BENCH_ROOT (the perf
// corpus) when that is set.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func benchRoot(b *testing.B) string {
	b.Helper()
	if root := os.Getenv("FSWALK_BENCH_ROOT"); root != "" {
		return root
	}
	root := b.TempDir()
	files := map[string]string{".gitignore": "*.log\nbuild/\n"}
	for i := range 4000 {
		files[filepath.Join(fmt.Sprintf("p%02d", i%40), fmt.Sprintf("s%d", i%5), fmt.Sprintf("f%04d.go", i))] = ""
	}
	writeTree(b, root, files)
	return root
}

// BenchmarkWalk walks the tree with the search tools' filters and a
// consumer that does nothing: the walk's own cost.
func BenchmarkWalk(b *testing.B) {
	root := benchRoot(b)
	b.ReportAllocs()
	for b.Loop() {
		_ = testWalker(root, false).Walk(context.Background(), root, func(Entry) error { return nil })
	}
}

// BenchmarkWalkSlowConsumer adds 2 µs per file on the consumer (about what
// grep's hand-off costs), the case where the workers used to list the whole
// tree ahead of it.
func BenchmarkWalkSlowConsumer(b *testing.B) {
	root := benchRoot(b)
	b.ReportAllocs()
	for b.Loop() {
		_ = testWalker(root, false).Walk(context.Background(), root, func(Entry) error {
			for start := time.Now(); time.Since(start) < 2*time.Microsecond; {
			}
			return nil
		})
	}
}
