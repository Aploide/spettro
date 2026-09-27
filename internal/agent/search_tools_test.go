package agent

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func writeTree(t testing.TB, root string, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func newSearchWorkspace(t *testing.T) *toolRuntime {
	t.Helper()
	r := newShellTestRuntime(t)
	writeTree(t, r.cwd, map[string]string{
		".gitignore":          "ignored/\n*.log\n",
		"a.go":                "package a\n\nfunc Foo() {}\n",
		"sub/b.go":            "package sub\n\n// Foo is documented here\nvar x = 1\n",
		"src/deep/x.ts":       "export const Foo = 1;\n",
		"src/deep/y.tsx":      "export const FooView = 2;\n",
		"ignored/c.go":        "Foo\n",
		"debug.log":           "Foo\n",
		"bin.dat":             "Foo\x00\x01\x02",
		"node_modules/m.js":   "Foo\n",
		"long.txt":            "Foo " + strings.Repeat("x", 5000) + "\n",
		"ctx.txt":             "one\ntwo\nFoo\nthree\nfour\nfive\nsix\nFoo\n",
		".hidden/config.yaml": "Foo: true\n",
	})
	return r
}

// grepBackends runs a test against the Go walk and, when rg is installed,
// against ripgrep too: both must produce the same answers.
func grepBackends(t *testing.T, fn func(t *testing.T)) {
	t.Helper()
	orig := lookRipgrep
	t.Cleanup(func() { lookRipgrep = orig })
	t.Run("go", func(t *testing.T) {
		lookRipgrep = func() (string, bool) { return "", false }
		fn(t)
	})
	t.Run("ripgrep", func(t *testing.T) {
		rg, err := exec.LookPath("rg")
		if err != nil {
			t.Skip("rg not installed")
		}
		lookRipgrep = func() (string, bool) { return rg, true }
		fn(t)
	})
}

func grepFiles(t *testing.T, r *toolRuntime, args grepArgs) []string {
	t.Helper()
	args.OutputMode = "files_with_matches"
	out, err := r.runGrep(context.Background(), args)
	if err != nil {
		t.Fatalf("grep %+v: %v", args, err)
	}
	if strings.HasPrefix(out, "no matches") {
		return nil
	}
	lines := strings.Split(out, "\n")[1:]
	var files []string
	for _, l := range lines {
		if !strings.HasPrefix(l, "(") {
			files = append(files, l)
		}
	}
	return files
}

func TestGrepRespectsGitignoreBinaryAndSkipDirs(t *testing.T) {
	grepBackends(t, func(t *testing.T) {
		r := newSearchWorkspace(t)
		got := grepFiles(t, r, grepArgs{Pattern: "Foo"})
		want := []string{".hidden/config.yaml", "a.go", "ctx.txt", "long.txt", "src/deep/x.ts", "src/deep/y.tsx", "sub/b.go"}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("files = %v, want %v", got, want)
		}
	})
}

func TestGrepPathAndFilters(t *testing.T) {
	grepBackends(t, func(t *testing.T) {
		r := newSearchWorkspace(t)
		cases := []struct {
			name string
			args grepArgs
			want []string
		}{
			{"dir path", grepArgs{Pattern: "Foo", Path: "sub"}, []string{"sub/b.go"}},
			{"file path", grepArgs{Pattern: "Foo", Path: "a.go"}, []string{"a.go"}},
			{"path glob", grepArgs{Pattern: "Foo", Glob: "src/**/*.ts"}, []string{"src/deep/x.ts"}},
			{"glob relative to path", grepArgs{Pattern: "Foo", Path: "src", Glob: "deep/*.tsx"}, []string{"src/deep/y.tsx"}},
			{"include alias", grepArgs{Pattern: "Foo", Include: "*.go"}, []string{"a.go", "sub/b.go"}},
			{"brace glob", grepArgs{Pattern: "Foo", Glob: "*.{ts,tsx}"}, []string{"src/deep/x.ts", "src/deep/y.tsx"}},
			{"type", grepArgs{Pattern: "Foo", Type: "ts"}, []string{"src/deep/x.ts", "src/deep/y.tsx"}},
			{"type and glob", grepArgs{Pattern: "Foo", Type: "ts", Glob: "x.*"}, []string{"src/deep/x.ts"}},
			{"case", grepArgs{Pattern: "foo", DashI: true, Path: "a.go"}, []string{"a.go"}},
			{"explicit ignored dir", grepArgs{Pattern: "Foo", Path: "ignored"}, []string{"ignored/c.go"}},
		}
		for _, c := range cases {
			if got := grepFiles(t, r, c.args); !reflect.DeepEqual(got, c.want) {
				t.Errorf("%s: files = %v, want %v", c.name, got, c.want)
			}
		}
	})
}

func TestGrepContentFormatContextAndClipping(t *testing.T) {
	grepBackends(t, func(t *testing.T) {
		r := newSearchWorkspace(t)
		out, err := r.runGrep(context.Background(), grepArgs{Pattern: "Foo", Path: "ctx.txt", DashC: 1})
		if err != nil {
			t.Fatal(err)
		}
		want := "2 matches in 1 files:\nctx.txt:2: two\nctx.txt:3: Foo\nctx.txt:4: three\n--\nctx.txt:7: six\nctx.txt:8: Foo"
		if out != want {
			t.Fatalf("content output:\n%s\nwant:\n%s", out, want)
		}
		out, err = r.runGrep(context.Background(), grepArgs{Pattern: "Foo", Path: "long.txt"})
		if err != nil {
			t.Fatal(err)
		}
		if len(out) > maxGrepLineChars+200 || !strings.Contains(out, "line truncated") {
			t.Fatalf("long line not clipped (%d chars): %.200q", len(out), out)
		}
	})
}

func TestGrepMaxResultsTruncates(t *testing.T) {
	grepBackends(t, func(t *testing.T) {
		r := newSearchWorkspace(t)
		out, err := r.runGrep(context.Background(), grepArgs{Pattern: "Foo", HeadLimit: 2})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(out, "2 matches in ") || !strings.Contains(out, "(results truncated at 2 matches") {
			t.Fatalf("truncation not reported: %s", out)
		}
	})
}

func TestGrepHonoursCancellation(t *testing.T) {
	grepBackends(t, func(t *testing.T) {
		r := newSearchWorkspace(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := r.runGrep(ctx, grepArgs{Pattern: "Foo"}); err == nil {
			t.Fatal("cancelled grep succeeded")
		}
	})
}

// Argument spellings from other harnesses decode through execute.
func TestGrepAcceptsForeignArgumentShapes(t *testing.T) {
	r := newSearchWorkspace(t)
	out, err := r.execute(context.Background(), toolCall{Tool: "grep", Args: []byte(`{"pattern":"Foo","path":"sub","-n":true,"-i":"true","head_limit":"5"}`)}, map[string]struct{}{"grep": {}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "sub/b.go:3: // Foo is documented here") {
		t.Fatalf("unexpected output: %s", out)
	}
	if _, err := r.execute(context.Background(), toolCall{Tool: "grep", Args: []byte(`{"pattern":"Foo","output_mode":"lines"}`)}, map[string]struct{}{"grep": {}}); err == nil {
		t.Fatal("unknown output_mode accepted")
	}
}

func TestGlobRespectsGitignoreAndPath(t *testing.T) {
	r := newSearchWorkspace(t)
	out, err := r.runGlob(context.Background(), "**/*.go", "")
	if err != nil {
		t.Fatal(err)
	}
	if out != "2 files:\na.go\nsub/b.go" {
		t.Fatalf("glob output: %q", out)
	}
	out, err = r.runGlob(context.Background(), "*.{ts,tsx}", "src/deep")
	if err != nil {
		t.Fatal(err)
	}
	if out != "2 files:\nsrc/deep/x.ts\nsrc/deep/y.tsx" {
		t.Fatalf("glob relative to path: %q", out)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := r.runGlob(ctx, "**/*", ""); err == nil {
		t.Fatal("cancelled glob succeeded")
	}
}

func TestGlobCapsResults(t *testing.T) {
	r := newShellTestRuntime(t)
	files := map[string]string{}
	for i := range maxGlobResults + 5 {
		files[fmt.Sprintf("f/%04d.txt", i)] = ""
	}
	writeTree(t, r.cwd, files)
	out, err := r.runGlob(context.Background(), "f/*.txt", "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out, fmt.Sprintf("%d files:\n", maxGlobResults+5)) {
		t.Fatalf("header: %.40q", out)
	}
	if got := strings.Count(out, ".txt"); got != maxGlobResults {
		t.Fatalf("listed %d files, want %d", got, maxGlobResults)
	}
	if !strings.Contains(out, fmt.Sprintf("showing the first %d of %d files", maxGlobResults, maxGlobResults+5)) {
		t.Fatalf("cap not reported: %s", out[len(out)-200:])
	}
}

func TestExpandBraces(t *testing.T) {
	cases := map[string][]string{
		"*.go":           {"*.go"},
		"*.{ts,tsx}":     {"*.ts", "*.tsx"},
		"{a,b}/{c,d}.go": {"a/c.go", "a/d.go", "b/c.go", "b/d.go"},
		"x.{a,{b,c}}":    {"x.a", "x.b", "x.c"},
		"unclosed{a,b":   {"unclosed{a,b"},
		"single{a}":      {"single{a}"},
	}
	for in, want := range cases {
		if got := expandBraces(in); !reflect.DeepEqual(got, want) {
			t.Errorf("expandBraces(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestCapGrepResultsKeepsContextOfKeptMatches(t *testing.T) {
	results := []grepFileResult{
		{path: "a", count: 1, lines: []grepLine{{num: 1, text: "m", match: true}}},
		{path: "b", count: 3, lines: []grepLine{
			{num: 1, text: "m", match: true}, {num: 2, text: "c"},
			{num: 5, text: "m", match: true}, {num: 6, text: "c"},
			{num: 9, text: "m", match: true},
		}},
	}
	got, truncated := capGrepResults(results, 2, 1)
	if !truncated || len(got) != 2 || got[1].count != 1 || len(got[1].lines) != 2 {
		t.Fatalf("capped = %+v truncated=%v", got, truncated)
	}
	if _, truncated := capGrepResults(results, 4, 1); truncated {
		t.Fatal("exact fit reported as truncated")
	}
}

// Include globs and type filters must not resurrect gitignored files. rg lets
// a --glob override win over ignore files, so its results are re-filtered.
func TestGrepFiltersDoNotBypassGitignore(t *testing.T) {
	grepBackends(t, func(t *testing.T) {
		r := newSearchWorkspace(t)
		cases := []struct {
			name string
			args grepArgs
			want []string
		}{
			{"glob on ignored files", grepArgs{Pattern: "Foo", Glob: "*.log"}, nil},
			{"glob into ignored dir", grepArgs{Pattern: "Foo", Glob: "*.go"}, []string{"a.go", "sub/b.go"}},
			{"type into ignored dir", grepArgs{Pattern: "Foo", Type: "go"}, []string{"a.go", "sub/b.go"}},
			{"explicit ignored dir with glob", grepArgs{Pattern: "Foo", Path: "ignored", Glob: "*.go"}, []string{"ignored/c.go"}},
		}
		for _, c := range cases {
			if got := grepFiles(t, r, c.args); !reflect.DeepEqual(got, c.want) {
				t.Errorf("%s: files = %v, want %v", c.name, got, c.want)
			}
		}
	})
}

// skipDirs names prune directories only: a file called build is searched.
func TestGrepSkipDirsExcludeOnlyDirectories(t *testing.T) {
	grepBackends(t, func(t *testing.T) {
		r := newShellTestRuntime(t)
		writeTree(t, r.cwd, map[string]string{
			"scripts/build": "Foo\n",
			"build/out.txt": "Foo\n",
			"dist":          "Foo\n",
		})
		got := grepFiles(t, r, grepArgs{Pattern: "Foo"})
		if want := []string{"dist", "scripts/build"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("files = %v, want %v", got, want)
		}
	})
}

// Spettro's own state dir never shows up in a workspace-wide glob or grep.
func TestSearchToolsHideSpettroDir(t *testing.T) {
	setup := func(t *testing.T) *toolRuntime {
		r := newShellTestRuntime(t)
		writeTree(t, r.cwd, map[string]string{
			"main.go":                   "Foo\n",
			".spettro/cache/symbols.go": "Foo\n",
			".spettro/memory.md":        "Foo\n",
		})
		return r
	}
	r := setup(t)
	out, err := r.runGlob(context.Background(), "**/*", "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, ".spettro") || !strings.Contains(out, "main.go") {
		t.Fatalf("glob output: %q", out)
	}
	grepBackends(t, func(t *testing.T) {
		r := setup(t)
		if got, want := grepFiles(t, r, grepArgs{Pattern: "Foo"}), []string{"main.go"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("files = %v, want %v", got, want)
		}
	})
}

// glob lists symlinks to files (the CLAUDE.md -> AGENTS.md pair) but does not
// descend into symlinked directories; grep, like ripgrep, skips symlinks met
// while walking, so both of its backends agree.
func TestSearchToolsAndSymlinks(t *testing.T) {
	setup := func(t *testing.T) *toolRuntime {
		r := newShellTestRuntime(t)
		writeTree(t, r.cwd, map[string]string{"AGENTS.md": "Foo\n", "real/x.md": "Foo\n"})
		if err := os.Symlink("AGENTS.md", filepath.Join(r.cwd, "CLAUDE.md")); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		if err := os.Symlink("real", filepath.Join(r.cwd, "linked")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("missing.md", filepath.Join(r.cwd, "dangling.md")); err != nil {
			t.Fatal(err)
		}
		return r
	}
	r := setup(t)
	out, err := r.runGlob(context.Background(), "**/*.md", "")
	if err != nil {
		t.Fatal(err)
	}
	if out != "3 files:\nAGENTS.md\nCLAUDE.md\nreal/x.md" {
		t.Fatalf("glob output: %q", out)
	}
	grepBackends(t, func(t *testing.T) {
		r := setup(t)
		if got, want := grepFiles(t, r, grepArgs{Pattern: "Foo"}), []string{"AGENTS.md", "real/x.md"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("grep files = %v, want %v", got, want)
		}
		// Named explicitly, a symlinked file is searched.
		if got, want := grepFiles(t, r, grepArgs{Pattern: "Foo", Path: "CLAUDE.md"}), []string{"CLAUDE.md"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("explicit symlink = %v, want %v", got, want)
		}
	})
}
