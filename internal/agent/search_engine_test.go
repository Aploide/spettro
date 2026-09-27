package agent

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// sequentialWalk is the walker as it was before the parallel one (a
// filepath.WalkDir with the same filters), kept as the oracle for the
// parallel walker's order and filtering.
func sequentialWalk(w workspaceWalker, root string) []string {
	var out []string
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(w.cwd, path)
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if path == root {
				return nil
			}
			if skipDirs[d.Name()] || w.ignores.ignored(path, true) {
				return filepath.SkipDir
			}
			return nil
		}
		if !w.keepFile(path, d) || w.ignores.ignored(path, false) {
			return nil
		}
		out = append(out, rel)
		return nil
	})
	return out
}

// randomWorkspace writes a random tree with nested .gitignore files, skipDirs
// names, symlinks and names chosen to make walk order differ from plain
// string order ("a" < "a.go" < "a0", "a/b" before "a.go").
func randomWorkspace(t *testing.T, rng *rand.Rand) string {
	t.Helper()
	root := t.TempDir()
	names := []string{"a", "a.go", "a0", "b", "C", "build", "node_modules", "x.log", "keep.log", "z.txt", ".hidden", "sub"}
	files := map[string]string{}
	for range 150 {
		var parts []string
		for range 1 + rng.Intn(4) {
			parts = append(parts, names[rng.Intn(len(names))])
		}
		files[strings.Join(parts, "/")] = "match me\n"
	}
	// Drop files whose path is a directory of another file.
	for p := range files {
		for q := range files {
			if strings.HasPrefix(q, p+"/") {
				delete(files, p)
				break
			}
		}
	}
	writeTree(t, root, files)
	ignores := []string{"*.log\n!keep.log\n", "a0/\n", "/z.txt\n", "sub/a\n", "C\n"}
	writeTree(t, root, map[string]string{".gitignore": ignores[rng.Intn(len(ignores))]})
	for d := range files {
		if dir := filepath.Dir(filepath.FromSlash(d)); dir != "." && rng.Intn(6) == 0 {
			writeTree(t, root, map[string]string{filepath.ToSlash(filepath.Join(dir, ".gitignore")): ignores[rng.Intn(len(ignores))]})
		}
	}
	_ = os.Symlink("a.go", filepath.Join(root, "link.go"))
	_ = os.Symlink("sub", filepath.Join(root, "linkdir"))
	return root
}

// The parallel walker visits exactly what a sequential WalkDir walk with the
// same filters visits, in the same order.
func TestWalkMatchesSequentialOracle(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	for i := range 25 {
		root := randomWorkspace(t, rng)
		for _, symlinked := range []bool{false, true} {
			w := workspaceWalker{cwd: root, ignores: newGitignoreSet(), symlinkedFiles: symlinked}
			var got []string
			if err := w.walk(context.Background(), root, func(_, rel string, _ fs.DirEntry) error {
				got = append(got, rel)
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			want := sequentialWalk(workspaceWalker{cwd: root, ignores: newGitignoreSet(), symlinkedFiles: symlinked}, root)
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("tree %d symlinked=%v:\nparallel   %v\nsequential %v", i, symlinked, got, want)
			}
		}
	}
}

func TestWalkStopsEarlyAndHonoursCancellation(t *testing.T) {
	root := randomWorkspace(t, rand.New(rand.NewSource(1)))
	w := workspaceWalker{cwd: root, ignores: newGitignoreSet()}
	n := 0
	err := w.walk(context.Background(), root, func(_, _ string, _ fs.DirEntry) error {
		n++
		if n == 3 {
			return errStopWalk
		}
		return nil
	})
	if err != nil || n != 3 {
		t.Fatalf("stop: err=%v visited=%d", err, n)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := w.walk(ctx, root, func(_, _ string, _ fs.DirEntry) error { return nil }); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled walk returned %v", err)
	}
}

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

func TestCompareWalkOrder(t *testing.T) {
	ordered := []string{"B.go", "a/b.go", "a/c/d.go", "a.go", "a0.go", "b/x"}
	for i := range ordered {
		for j := range ordered {
			got := compareWalkOrder(ordered[i], ordered[j])
			want := 0
			switch {
			case i < j:
				want = -1
			case i > j:
				want = 1
			}
			if got != want {
				t.Errorf("compareWalkOrder(%q, %q) = %d, want %d", ordered[i], ordered[j], got, want)
			}
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
