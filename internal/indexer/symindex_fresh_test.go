package indexer

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func syncs(x *SymbolIndex) int {
	x.mu.Lock()
	defer x.mu.Unlock()
	return x.syncs
}

// Work-count guard: a lookup within syncTTL answers from memory (no walk),
// an Invalidate re-parses only that file, and MarkStale forces a full sync.
func TestLookupWithinTTLDoesNotWalk(t *testing.T) {
	root := fixtureRepo(t)
	x := NewSymbolIndex(root, "")
	x.Lookup(context.Background(), "Server")
	if n := syncs(x); n != 1 {
		t.Fatalf("first lookup ran %d syncs, want 1", n)
	}
	x.Lookup(context.Background(), "Server")
	writeFile(t, root, "pkg/server.go", "package pkg\n\nfunc Renamed() {}\n")
	x.Invalidate("pkg/server.go")
	if syms := x.Lookup(context.Background(), "Renamed"); len(syms) != 1 {
		t.Fatalf("invalidated file not re-parsed: %+v", syms)
	}
	if n := syncs(x); n != 1 {
		t.Fatalf("lookups within the TTL ran %d syncs, want 1", n)
	}
	writeFile(t, root, "web/new.ts", "export function freshly() {}\n")
	x.MarkStale()
	if syms := x.Lookup(context.Background(), "freshly"); len(syms) != 1 {
		t.Fatalf("stale index missed a new file: %+v", syms)
	}
	if n := syncs(x); n != 2 {
		t.Fatalf("MarkStale did not force a sync (%d)", n)
	}
}

func TestSharedIndexPerRoot(t *testing.T) {
	root := t.TempDir()
	a := Shared(root, "")
	if b := Shared(root+string(filepath.Separator), ""); a != b {
		t.Fatal("same root gave two indexes")
	}
	if c := Shared(t.TempDir(), ""); c == a {
		t.Fatal("different roots share an index")
	}
}

// The index applies nested .gitignore files, as the search tools do.
func TestNestedGitignoreRespected(t *testing.T) {
	root := fixtureRepo(t)
	writeFile(t, root, "pkg/gen/.gitignore", "*.go\n")
	writeFile(t, root, "pkg/gen/out.go", "package gen\n\nfunc GeneratedThing() {}\n")
	x := NewSymbolIndex(root, "")
	if syms := x.Lookup(context.Background(), "GeneratedThing"); len(syms) != 0 {
		t.Fatalf("file ignored by a nested .gitignore was indexed: %+v", syms)
	}
}

func TestTruncatedIsReported(t *testing.T) {
	x := NewSymbolIndex(fixtureRepo(t), "")
	x.maxFiles = 1
	x.Lookup(context.Background(), "Server")
	if trunc, limit := x.Truncated(); !trunc || limit != 1 {
		t.Fatalf("Truncated() = %v, %d", trunc, limit)
	}
	y := NewSymbolIndex(fixtureRepo(t), "")
	y.Lookup(context.Background(), "Server")
	if trunc, _ := y.Truncated(); trunc {
		t.Fatal("a complete index reports truncation")
	}
}

// The cache is gob now; a JSON cache of an earlier version next to it is
// removed, and a cache from another root or version is ignored.
func TestGobCacheReplacesJSON(t *testing.T) {
	root := fixtureRepo(t)
	dir := filepath.Join(root, ".spettro", "cache")
	writeFile(t, root, ".spettro/cache/symbols.json", `{"version":1}`)
	x := NewSymbolIndex(root, filepath.Join(dir, "symbols.gob"))
	x.Warm(context.Background())
	if _, err := os.Stat(filepath.Join(dir, "symbols.json")); !os.IsNotExist(err) {
		t.Fatalf("legacy JSON cache kept: %v", err)
	}
	y := NewSymbolIndex(root, filepath.Join(dir, "symbols.gob"))
	y.mu.Lock()
	y.loadCache()
	n := len(y.files)
	y.mu.Unlock()
	if n == 0 {
		t.Fatal("gob cache not loaded")
	}
	other := NewSymbolIndex(t.TempDir(), filepath.Join(dir, "symbols.gob"))
	other.loadCache()
	if len(other.files) != 0 {
		t.Fatal("a cache written for another root was used")
	}
}

// oracleRegexExtract is the extraction before the keyword prefilter
// (commit 0f95899): every line through every rule.
func oracleRegexExtract(relPath string, src []byte, rules []regexRule) []Symbol {
	var out []Symbol
	for i, line := range strings.Split(string(src), "\n") {
		trimmed := strings.TrimRight(line, " \t\r")
		for _, r := range rules {
			m := r.re.FindStringSubmatch(trimmed)
			if m == nil {
				continue
			}
			out = append(out, Symbol{Path: filepath.ToSlash(relPath), Line: i + 1, Kind: r.kind, Name: m[1], Signature: strings.TrimSpace(trimmed)})
			break
		}
	}
	return out
}

// FuzzExtractMatchesOracle checks the keyword prefilter never drops a line
// one of the rules matches.
func FuzzExtractMatchesOracle(f *testing.F) {
	for _, s := range []string{"func (s *S) M() {}", "  async def f():", "export const x = 1", "type T struct{}", "\tclass K:", "A_B = 1", "var x int"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, src string) {
		for _, c := range []struct {
			ext   Extractor
			rules []regexRule
		}{{goExtractor{}, goRules}, {pyExtractor{}, pyRules}, {jsExtractor{}, jsRules}} {
			got := c.ext.Extract("f", []byte(src))
			want := oracleRegexExtract("f", []byte(src), c.rules)
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("%T on %q:\n got %+v\nwant %+v", c.ext, src, got, want)
			}
		}
	})
}

// The prefiltered extraction finds exactly what the plain one does: on the
// Go standard library, and on Python and JS/TS lines built to exercise every
// rule and keyword.
func TestExtractMatchesOracle(t *testing.T) {
	src := filepath.Join(runtime.GOROOT(), "src", "net", "http")
	n := 0
	_ = filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".go") {
			return nil
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return nil
		}
		n++
		if got, want := (goExtractor{}).Extract("x.go", data), oracleRegexExtract("x.go", data, goRules); !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: prefiltered extraction differs", p)
		}
		return nil
	})
	if n == 0 {
		t.Skip("no Go sources under GOROOT")
	}
	var py, js strings.Builder
	for _, indent := range []string{"", "  ", "\t", " \f"} {
		for _, l := range []string{"def f(x):", "async def g():", "class C(Base):", "class D:", "CONST_1 = 3", "_PRIV = 1", "lower = 2", "x = def", "   CONST = 1"} {
			fmt.Fprintf(&py, "%s%s\n", indent, l)
		}
		for _, l := range []string{"export default async function* gen() {", "function f(a) {", "export abstract class A {", "class B extends C {", "export interface I {", "type T = string", "enum E {", "export const x = () => 1", "let y = 2", "var z = 'a'", "const w = {", "default function d() {}", "async function a() {}", "notakeyword x = 1", "exported = 1"} {
			fmt.Fprintf(&js, "%s%s\r\n", indent, l)
		}
	}
	if got, want := (pyExtractor{}).Extract("a.py", []byte(py.String())), oracleRegexExtract("a.py", []byte(py.String()), pyRules); !reflect.DeepEqual(got, want) {
		t.Fatalf("python extraction differs:\n%+v\n%+v", got, want)
	}
	if got, want := (jsExtractor{}).Extract("a.ts", []byte(js.String())), oracleRegexExtract("a.ts", []byte(js.String()), jsRules); !reflect.DeepEqual(got, want) {
		t.Fatalf("js extraction differs:\n%+v\n%+v", got, want)
	}
}
