package agent

import (
	"context"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// count and files_with_matches answer "how many" and "which files": the cap
// limits how many files are listed, never the per-file count, and one busy
// file must not hide the others.
func TestGrepCountAndFilesModesAreNotCutByMatchCap(t *testing.T) {
	grepBackends(t, func(t *testing.T) {
		r := newShellTestRuntime(t)
		writeTree(t, r.cwd, map[string]string{
			"a.txt": strings.Repeat("Foo\n", 300),
			"b.txt": "Foo\n",
		})
		out, err := r.runGrep(context.Background(), grepArgs{Pattern: "Foo", OutputMode: "count"})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out, "a.txt: 300") || !strings.Contains(out, "b.txt: 1") || strings.Contains(out, "truncated") {
			t.Fatalf("count mode: %s", out)
		}
		if got := grepFiles(t, r, grepArgs{Pattern: "Foo"}); !reflect.DeepEqual(got, []string{"a.txt", "b.txt"}) {
			t.Fatalf("files_with_matches = %v", got)
		}
		// max_results caps the number of files listed in these modes.
		out, err = r.runGrep(context.Background(), grepArgs{Pattern: "Foo", OutputMode: "count", MaxResults: 1})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out, "a.txt: 300") || strings.Contains(out, "b.txt") || !strings.Contains(out, "truncated at 1 files") {
			t.Fatalf("count mode with a file cap: %s", out)
		}
	})
}

// An unknown type must be an error naming the supported ones, not a silent
// search of every file; common long names map to their short form.
func TestGrepTypeValidation(t *testing.T) {
	r := newSearchWorkspace(t)
	_, err := r.runGrep(context.Background(), grepArgs{Pattern: "Foo", Type: "java"})
	if err == nil || !strings.Contains(err.Error(), "go, ts, js") {
		t.Fatalf("unknown type: err=%v", err)
	}
	if got := grepFiles(t, r, grepArgs{Pattern: "Foo", Type: "typescript"}); !reflect.DeepEqual(got, []string{"src/deep/x.ts", "src/deep/y.tsx"}) {
		t.Fatalf("typescript alias = %v", got)
	}
}

// rg/grep flag spellings: -c is count (not -C context), -A/-B are context.
func TestGrepDashFlagsAreCaseSensitive(t *testing.T) {
	var a grepArgs
	if err := decodeJSONStrict([]byte(`{"pattern":"main","-c":true}`), &a); err != nil {
		t.Fatalf("-c:true: %v", err)
	}
	r := newSearchWorkspace(t)
	q, err := r.newGrepQuery(a)
	if err != nil {
		t.Fatal(err)
	}
	if q.mode != "count" || q.context != 0 {
		t.Fatalf("-c decoded as mode=%q context=%d", q.mode, q.context)
	}
	a = grepArgs{}
	if err := decodeJSONStrict([]byte(`{"pattern":"main","-A":3,"-B":1,"-C":2,"-i":true}`), &a); err != nil {
		t.Fatal(err)
	}
	q, err = r.newGrepQuery(a)
	if err != nil {
		t.Fatal(err)
	}
	if q.context != 3 || !q.ignoreCase {
		t.Fatalf("context=%d ignoreCase=%v", q.context, q.ignoreCase)
	}
}

// An absolute pattern inside the workspace is matched like its relative form.
func TestGlobAcceptsAbsolutePatternInsideWorkspace(t *testing.T) {
	r := newSearchWorkspace(t)
	for _, p := range []string{filepath.ToSlash(r.cwd) + "/sub/*.go", filepath.ToSlash(r.cwd) + "/**/b.go"} {
		out, err := r.runGlob(context.Background(), p, "")
		if err != nil {
			t.Fatal(err)
		}
		if out != "1 files:\nsub/b.go" {
			t.Fatalf("glob %q = %q", p, out)
		}
	}
	if _, err := r.runGlob(context.Background(), "/elsewhere/**/*.go", ""); err == nil {
		t.Fatal("absolute pattern outside the workspace must be an error, not 'no files match'")
	}
}

// A file named explicitly as path is searched even if it looks binary or is
// larger than the walk's size cap: "no matches" must mean there are none.
func TestGrepExplicitFileIsNeverSilentlySkipped(t *testing.T) {
	r := newShellTestRuntime(t)
	pad := strings.Repeat("padding line\n", maxSearchFileBytes/13+1)
	writeTree(t, r.cwd, map[string]string{
		"nul.log": "a\x00b\nERROR here\n",
		"big.log": "ERROR first\n" + pad + "ERROR last\n",
	})
	orig := lookRipgrep
	t.Cleanup(func() { lookRipgrep = orig })
	lookRipgrep = func() (string, bool) { return "", false }
	out, err := r.runGrep(context.Background(), grepArgs{Pattern: "ERROR", Path: "nul.log"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "nul.log:2: ERROR here") {
		t.Fatalf("explicit binary-looking file: %q", out)
	}
	out, err = r.runGrep(context.Background(), grepArgs{Pattern: "ERROR", Path: "big.log", OutputMode: "count"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "big.log: 2") {
		t.Fatalf("explicit oversized file: %q", out)
	}
}

// Nested .gitignore files (and those of parent directories) apply to both
// grep backends and to glob, as they do for git and ripgrep.
func TestSearchHonoursNestedAndParentGitignore(t *testing.T) {
	grepBackends(t, func(t *testing.T) {
		r := newShellTestRuntime(t)
		writeTree(t, r.cwd, map[string]string{
			".gitignore":            "*.gen.go\n",
			"sub/.gitignore":        "secret.txt\n/anchored.txt\n",
			"sub/ok.txt":            "Foo\n",
			"sub/secret.txt":        "Foo\n",
			"sub/anchored.txt":      "Foo\n",
			"sub/deep/anchored.txt": "Foo\n",
			"sub/x.gen.go":          "Foo\n",
		})
		want := []string{"sub/deep/anchored.txt", "sub/ok.txt"}
		if got := grepFiles(t, r, grepArgs{Pattern: "Foo"}); !reflect.DeepEqual(got, want) {
			t.Fatalf("grep from root = %v, want %v", got, want)
		}
		out, err := r.runGlob(context.Background(), "**/*.txt", "")
		if err != nil {
			t.Fatal(err)
		}
		if out != "2 files:\nsub/deep/anchored.txt\nsub/ok.txt" {
			t.Fatalf("glob from root = %q", out)
		}
		sub := &toolRuntime{cwd: filepath.Join(r.cwd, "sub"), readSet: map[string]struct{}{}}
		if got := grepFiles(t, sub, grepArgs{Pattern: "Foo"}); !reflect.DeepEqual(got, []string{"deep/anchored.txt", "ok.txt"}) {
			t.Fatalf("grep from sub = %v", got)
		}
		out, err = sub.runGlob(context.Background(), "*.go", "")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(out, "no files match") {
			t.Fatalf("glob from sub must honour the parent .gitignore: %q", out)
		}
	})
}
