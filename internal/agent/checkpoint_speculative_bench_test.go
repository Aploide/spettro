package agent

// Benchmarks for the known-file check on a prepared checkpoint's claim path
// (performance plan, Unit D4; the review harness TestReviewKnownFileCost).
// The claim stats every stamped file once; the pass before staging runs on
// the preparation's goroutine, off the step's path.

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func BenchmarkKnownFilesUnchanged(b *testing.B) {
	for _, n := range []int{128, 1024} {
		b.Run(fmt.Sprintf("stamps=%d", n), func(b *testing.B) {
			dir := b.TempDir()
			r := &toolRuntime{fileStamps: map[string][32]byte{}}
			for i := range n {
				p := filepath.Join(dir, fmt.Sprintf("d%d", i%40), fmt.Sprintf("f%d.go", i))
				if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
					b.Fatal(err)
				}
				if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
					b.Fatal(err)
				}
				r.fileStamps[p] = [32]byte{}
			}
			sc := &speculativeCheckpoint{started: time.Now()}
			sc.known, sc.knownIncomplete = r.knownFileStates()
			b.ResetTimer()
			for b.Loop() {
				if !r.knownFilesUnchanged(sc) {
					b.Fatal("unchanged files reported changed")
				}
			}
		})
	}
}
