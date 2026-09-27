package agent

// Benchmarks for the search tools on a synthetic tree (perf plan unit E:
// the "Walker with rules", "Go grep full scan / case-insensitive" and
// "glob **/*.go / prefixed glob" rows; the plan's numbers come from the
// 61k-file corpus harness, these track the same code paths in CI-sized
// form).

import (
	"context"
	"fmt"
	"io/fs"
	"strings"
	"testing"
)

func benchWorkspace(b *testing.B) *toolRuntime {
	b.Helper()
	root := b.TempDir()
	files := map[string]string{".gitignore": "*.log\nbuild/\n/tmp/\n*.py[cod]\n**/testdata/*.golden\n"}
	body := strings.Repeat("func handler(ctx context.Context) error { return nil }\n", 60)
	for i := range 2000 {
		files[fmt.Sprintf("pkg%02d/sub%d/file%04d.go", i%40, i%5, i)] = body
		if i%10 == 0 {
			files[fmt.Sprintf("pkg%02d/sub%d/file%04d.log", i%40, i%5, i)] = body
		}
	}
	writeTree(b, root, files)
	return &toolRuntime{cwd: root, readSet: map[string]struct{}{}, requiredReads: map[string]struct{}{}}
}

func BenchmarkWorkspaceWalk(b *testing.B) {
	r := benchWorkspace(b)
	b.ReportAllocs()
	for b.Loop() {
		n := 0
		_ = r.newWorkspaceWalker().walk(context.Background(), r.cwd, func(_, _ string, _ fs.DirEntry) error { n++; return nil })
		if n != 2001 { // the .go files and .gitignore
			b.Fatalf("walked %d files", n)
		}
	}
}

func benchGrepGo(b *testing.B, args grepArgs) {
	r := benchWorkspace(b)
	orig := lookRipgrep
	b.Cleanup(func() { lookRipgrep = orig })
	lookRipgrep = func() (string, bool) { return "", false }
	b.ReportAllocs()
	for b.Loop() {
		if _, err := r.runGrep(context.Background(), args); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkGrepGoFullScan(b *testing.B) {
	benchGrepGo(b, grepArgs{Pattern: "zzqx_no_such_token"})
}

func BenchmarkGrepGoCaseInsensitive(b *testing.B) {
	benchGrepGo(b, grepArgs{Pattern: "DEADLINE EXCEEDED", CaseInsensitive: true})
}

func BenchmarkGrepGoRegexCount(b *testing.B) {
	benchGrepGo(b, grepArgs{Pattern: `ctx\s+context\.Context`, OutputMode: "count", MaxResults: 100000})
}

func BenchmarkGlobAll(b *testing.B) {
	r := benchWorkspace(b)
	b.ReportAllocs()
	for b.Loop() {
		if _, err := r.runGlob(context.Background(), "**/*.go", ""); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkGlobLiteralPrefix(b *testing.B) {
	r := benchWorkspace(b)
	b.ReportAllocs()
	for b.Loop() {
		if _, err := r.runGlob(context.Background(), "pkg07/sub2/*.go", ""); err != nil {
			b.Fatal(err)
		}
	}
}
