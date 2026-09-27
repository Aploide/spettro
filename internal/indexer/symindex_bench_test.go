package indexer

// Benchmarks for the symbol index (perf plan unit E, E5: the "warm symbol
// lookup", "first lookup per run" and symbol-grep harness rows). The
// numbers quoted in symbols.go and symindex_cache.go come from these and
// from the 60k-file perf corpus harness.

import (
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// goSources reads the Go files of the standard library's net/http tree.
func goSources(b *testing.B) [][]byte {
	b.Helper()
	var srcs [][]byte
	_ = filepath.WalkDir(filepath.Join(runtime.GOROOT(), "src", "net", "http"), func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(p, ".go") {
			if data, err := os.ReadFile(p); err == nil {
				srcs = append(srcs, data)
			}
		}
		return nil
	})
	if len(srcs) == 0 {
		b.Skip("no Go sources under GOROOT")
	}
	return srcs
}

// BenchmarkExtractGo is the keyword-prefiltered extraction the index uses.
func BenchmarkExtractGo(b *testing.B) {
	srcs := goSources(b)
	b.ReportAllocs()
	for b.Loop() {
		for _, src := range srcs {
			(goExtractor{}).Extract("x.go", src)
		}
	}
}

// BenchmarkExtractGoOracle is the extraction it replaced (every line
// through every rule), for comparison with BenchmarkExtractGo.
func BenchmarkExtractGoOracle(b *testing.B) {
	srcs := goSources(b)
	b.ReportAllocs()
	for b.Loop() {
		for _, src := range srcs {
			oracleRegexExtract("x.go", src, goRules)
		}
	}
}

// BenchmarkCacheRoundTrip writes and reads the cache of a 2000-file index.
func BenchmarkCacheRoundTrip(b *testing.B) {
	srcs := goSources(b)
	files := make([]cachedFile, 0, 2000)
	for i := range 2000 {
		src := srcs[i%len(srcs)]
		files = append(files, cachedFile{path: filepath.Join("p", strings.Repeat("d", i%7), string(rune('a'+i%26))+".go") + string(rune('0'+i%10)), entries: newFileSymbols(int64(i), int64(len(src)), (goExtractor{}).Extract("x.go", src))})
	}
	path := filepath.Join(b.TempDir(), "symbols.idx")
	b.ReportAllocs()
	for b.Loop() {
		if err := writeCache(path, "/root", files); err != nil {
			b.Fatal(err)
		}
		if _, err := readCache(path, "/root"); err != nil {
			b.Fatal(err)
		}
	}
}
