package agent

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// The Go grep backend reports matches in walk order and cuts max_results at
// the same place a one-file-at-a-time search would, however its workers
// finish: files are committed in walk order.
func TestGrepGoBackendIsDeterministic(t *testing.T) {
	orig := lookRipgrep
	t.Cleanup(func() { lookRipgrep = orig })
	lookRipgrep = func() (string, bool) { return "", false }
	r := newShellTestRuntime(t)
	files := map[string]string{}
	for i := range 120 {
		// Uneven sizes so workers finish out of order.
		files[fmt.Sprintf("d%d/f%03d.txt", i%7, i)] = strings.Repeat("filler line\n", (i*37)%500) + "hit one\nhit two\n"
	}
	writeTree(t, r.cwd, files)
	var first string
	for range 20 {
		out, err := r.runGrep(context.Background(), grepArgs{Pattern: "hit", MaxResults: 25})
		if err != nil {
			t.Fatal(err)
		}
		if first == "" {
			first = out
			if !strings.HasPrefix(out, "25 matches in 13 files:\nd0/f000.txt:") {
				t.Fatalf("unexpected first result:\n%s", out)
			}
			continue
		}
		if out != first {
			t.Fatalf("run differs:\n%s\nvs\n%s", out, first)
		}
	}
}

// The whole-file prefilter must never hide a line match: anchors are line
// anchors per line, \A and \z still work, and (?i) literals find the
// non-ASCII runes Unicode folds to k and s.
func TestGrepGoPrefilterKeepsLineSemantics(t *testing.T) {
	orig := lookRipgrep
	t.Cleanup(func() { lookRipgrep = orig })
	lookRipgrep = func() (string, bool) { return "", false }
	r := newShellTestRuntime(t)
	writeTree(t, r.cwd, map[string]string{
		"a.go":     "package a\n\nfunc Foo() {}\n  func Bar() {}\n",
		"crlf.txt": "end here\r\nnext\r\n",
		"fold.txt": "plain\n\u017fpecial and \u212aelvin\n",
		"span.txt": "alpha\nbeta\n",
	})
	cases := []struct {
		args grepArgs
		want string
	}{
		{grepArgs{Pattern: "^func"}, "a.go:3: func Foo() {}"},
		{grepArgs{Pattern: `\Afunc Foo`}, "a.go:3: func Foo() {}"},
		{grepArgs{Pattern: `Foo\(\) \{\}\z`}, "a.go:3: func Foo() {}"},
		{grepArgs{Pattern: "here$"}, "no matches"},
		{grepArgs{Pattern: "SPECIAL", CaseInsensitive: true}, "fold.txt:2:"},
		{grepArgs{Pattern: "KELVIN", CaseInsensitive: true}, "fold.txt:2:"},
		{grepArgs{Pattern: "alpha\nbeta"}, "no matches"},
		{grepArgs{Pattern: "PLAIN", CaseInsensitive: true}, "fold.txt:1: plain"},
	}
	for _, c := range cases {
		out, err := r.runGrep(context.Background(), c.args)
		if err != nil {
			t.Fatalf("%+v: %v", c.args, err)
		}
		if !strings.Contains(out, c.want) {
			t.Errorf("grep %q (i=%v): want %q in\n%s", c.args.Pattern, c.args.CaseInsensitive, c.want, out)
		}
	}
}

// Both grep backends list files in walk order ("a/b" before "a.go"), rg
// included now that it runs unsorted.
func TestGrepBackendsAgreeOnOrder(t *testing.T) {
	grepBackends(t, func(t *testing.T) {
		r := newShellTestRuntime(t)
		writeTree(t, r.cwd, map[string]string{"a.go": "x\n", "a/b.go": "x\n", "a0.go": "x\n", "B.go": "x\n", "b/c/d.go": "x\n"})
		got := grepFiles(t, r, grepArgs{Pattern: "x"})
		want := []string{"B.go", "a/b.go", "a.go", "a0.go", "b/c/d.go"}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("files = %v, want %v", got, want)
		}
	})
}

// A glob with a literal directory prefix walks only that directory, but
// still sees nothing below a pruned (skipDirs or ignored) directory.
func TestGlobLiteralPrefixStart(t *testing.T) {
	r := newShellTestRuntime(t)
	writeTree(t, r.cwd, map[string]string{
		".gitignore":            "gen/\n",
		"pkg/api/a.go":          "",
		"pkg/api/sub/b.go":      "",
		"pkg/other/c.go":        "",
		"build/pkg/d.go":        "",
		"gen/pkg/e.go":          "",
		"internal/pkg/api/f.go": "",
	})
	cases := []struct{ pattern, path, want string }{
		{"pkg/api/**/*.go", "", "2 files:\npkg/api/a.go\npkg/api/sub/b.go"},
		{"pkg/{api,other}/*.go", "", "2 files:\npkg/api/a.go\npkg/other/c.go"},
		{"build/pkg/*.go", "", `no files match "build/pkg/*.go"`},
		{"gen/**/*.go", "", `no files match "gen/**/*.go"`},
		{"pkg/api/*.go", "internal", "1 files:\ninternal/pkg/api/f.go"},
		{"internal/pkg/api/*.go", "internal", "1 files:\ninternal/pkg/api/f.go"},
		{"nope/*.go", "", `no files match "nope/*.go"`},
	}
	for _, c := range cases {
		out, err := r.runGlob(context.Background(), c.pattern, c.path)
		if err != nil {
			t.Fatalf("%s: %v", c.pattern, err)
		}
		if out != c.want {
			t.Errorf("glob %q path %q = %q, want %q", c.pattern, c.path, out, c.want)
		}
	}
}

// Glob matching and walk-order comparison run once per walked file: no
// allocations (deterministic guard for the E4 glob rework).
func TestGlobMatchDoesNotAllocate(t *testing.T) {
	pats := compileGlobs([]string{"**/*.go", "internal/**/testdata/*.json", "*.md"})
	paths := []string{"internal/agent/glob.go", "internal/x/testdata/a.json", "README.md", "a/b/c/d/e.txt"}
	allocs := testing.AllocsPerRun(100, func() {
		for _, p := range paths {
			globMatchesAny(pats, p, ".")
			compareWalkOrder(p, "internal/agent")
		}
	})
	if allocs != 0 {
		t.Fatalf("glob matching allocates %.1f times per run, want 0", allocs)
	}
}
