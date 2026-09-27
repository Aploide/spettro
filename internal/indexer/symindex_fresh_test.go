package indexer

import (
	"bufio"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"sync"
	"testing"
	"time"
	"unsafe"
)

func syncs(x *SymbolIndex) int {
	x.mu.Lock()
	defer x.mu.Unlock()
	return x.syncs
}

func gen(x *SymbolIndex) uint64 {
	x.mu.Lock()
	defer x.mu.Unlock()
	return x.gen
}

func paths(syms []Symbol) []string {
	var out []string
	for _, s := range syms {
		out = append(out, s.Path+":"+s.Name)
	}
	return out
}

// Work-count guard: the first lookup waits for one sync; later lookups
// answer from memory, an Invalidate re-parses only that file, and MarkStale
// starts a sync in the background that the lookup does not wait for.
func TestLookupSyncsOnceThenAnswersFromMemory(t *testing.T) {
	root := fixtureRepo(t)
	x := NewSymbolIndex(root, "")
	ctx := context.Background()
	x.Lookup(ctx, "Server")
	if n := syncs(x); n != 1 {
		t.Fatalf("first lookup ran %d syncs, want 1", n)
	}
	x.Lookup(ctx, "Server")
	writeFile(t, root, "pkg/server.go", "package pkg\n\nfunc Renamed() {}\n")
	x.Invalidate("pkg/server.go")
	if syms := x.Lookup(ctx, "Renamed"); len(syms) != 1 {
		t.Fatalf("invalidated file not re-parsed: %+v", syms)
	}
	if n := syncs(x); n != 1 {
		t.Fatalf("lookups within the TTL ran %d syncs, want 1", n)
	}
	// A new file nothing reports: the lookup that starts the sync answers
	// before it (it holds the index while ranking, so the sync cannot have
	// applied yet); once the sync is done the file is found.
	writeFile(t, root, "web/new.ts", "export function freshly() {}\n")
	x.MarkStale()
	if syms := x.Lookup(ctx, "freshly"); len(syms) != 0 {
		t.Fatalf("lookup waited for the background sync: %+v", syms)
	}
	x.Flush()
	if syms := x.Lookup(ctx, "freshly"); len(syms) != 1 {
		t.Fatalf("background sync missed a new file: %+v", syms)
	}
	if n := syncs(x); n != 2 {
		t.Fatalf("MarkStale ran %d syncs in all, want 2", n)
	}
}

// A lookup re-checks the files of its hits: a definition changed or removed
// outside Spettro, within the TTL, is not listed from memory.
func TestLookupVerifiesTheFilesOfItsHits(t *testing.T) {
	root := fixtureRepo(t)
	x := NewSymbolIndex(root, "")
	ctx := context.Background()
	if syms := x.Lookup(ctx, "NewServer"); len(syms) != 1 {
		t.Fatalf("NewServer: %+v", syms)
	}
	writeFile(t, root, "pkg/server.go", "package pkg\n\n// moved\n\nfunc NewServerV2() {}\n")
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(filepath.Join(root, "pkg", "server.go"), future, future); err != nil {
		t.Fatal(err)
	}
	syms := x.Lookup(ctx, "NewServer")
	if got := paths(syms); !reflect.DeepEqual(got, []string{"pkg/server.go:NewServerV2"}) || syms[0].Line != 5 {
		t.Fatalf("after an outside edit: %+v", syms)
	}
	if err := os.Remove(filepath.Join(root, "pkg", "server.go")); err != nil {
		t.Fatal(err)
	}
	if syms := x.Lookup(ctx, "NewServer"); len(syms) != 0 {
		t.Fatalf("a deleted file's definitions are listed: %+v", syms)
	}
	if n := syncs(x); n != 1 {
		t.Fatalf("verification ran %d syncs, want 1", n)
	}
}

// Refresh brings the files a caller saw (the symbol search's grep matches)
// up to date, new ones included, without a sync; excluded files stay out.
func TestRefreshPicksUpTheGivenFiles(t *testing.T) {
	root := fixtureRepo(t)
	x := NewSymbolIndex(root, "")
	ctx := context.Background()
	x.Lookup(ctx, "Server")
	writeFile(t, root, "pkg/extra.go", "package pkg\n\nfunc ServerExtra() {}\n")
	writeFile(t, root, "ignored/more.go", "package gen\n\nfunc ServerIgnored() {}\n")
	x.Refresh(ctx, []string{"pkg/extra.go", "ignored/more.go", "pkg/extra.go", "missing.go"})
	if got := paths(x.Lookup(ctx, "ServerExtra")); !reflect.DeepEqual(got, []string{"pkg/extra.go:ServerExtra"}) {
		t.Fatalf("refreshed file: %v", got)
	}
	if syms := x.Lookup(ctx, "ServerIgnored"); len(syms) != 0 {
		t.Fatalf("an ignored file was added: %+v", syms)
	}
	if n := syncs(x); n != 1 {
		t.Fatalf("Refresh ran %d syncs, want 1", n)
	}
}

// Invalidate applies the sync's filters: a written file below an ignored
// or skipped directory is not indexed.
func TestInvalidateRespectsIgnoreAndSkipDirs(t *testing.T) {
	root := fixtureRepo(t)
	writeFile(t, root, ".gitignore", "ignored/\ngen/\n")
	x := NewSymbolIndex(root, "")
	ctx := context.Background()
	x.Lookup(ctx, "Server")
	writeFile(t, root, "gen/g.go", "package g\n\nfunc ProbeGenerated() {}\n")
	writeFile(t, root, "vendor/v.go", "package v\n\nfunc ProbeVendored() {}\n")
	writeFile(t, root, "pkg/sub/.gitignore", "*.go\n")
	writeFile(t, root, "pkg/sub/s.go", "package sub\n\nfunc ProbeNested() {}\n")
	for _, rel := range []string{"gen/g.go", "vendor/v.go", "pkg/sub/s.go"} {
		x.Invalidate(rel)
	}
	for _, name := range []string{"ProbeGenerated", "ProbeVendored", "ProbeNested"} {
		if syms := x.Lookup(ctx, name); len(syms) != 0 {
			t.Errorf("%s indexed after Invalidate: %+v", name, syms)
		}
	}
}

// A no-op re-parse leaves the index (and so the cache) unchanged: a file
// that is not source, or a source file whose symbols did not change.
func TestReparseWithoutChangeKeepsTheIndex(t *testing.T) {
	root := fixtureRepo(t)
	writeFile(t, root, "README.md", "# readme\n")
	x := NewSymbolIndex(root, "")
	x.Lookup(context.Background(), "Server")
	before := gen(x)
	x.Invalidate("README.md")
	x.Invalidate("pkg/server.go")
	x.Lookup(context.Background(), "Server")
	if after := gen(x); after != before {
		t.Fatalf("a no-op re-parse changed the index (gen %d -> %d)", before, after)
	}
}

func TestSymlinkedFilesAreIndexed(t *testing.T) {
	root := fixtureRepo(t)
	writeFile(t, root, "real/b.go", "package b\n\nfunc ProbeLinked() {}\n")
	if err := os.Symlink(filepath.Join(root, "real", "b.go"), filepath.Join(root, "link.go")); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	x := NewSymbolIndex(root, "")
	got := paths(x.Lookup(context.Background(), "ProbeLinked"))
	if want := []string{"link.go:ProbeLinked", "real/b.go:ProbeLinked"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// The .gitignore files above the root do not apply to the index.
func TestGitignoreAboveTheRootDoesNotApply(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "proj")
	writeFile(t, parent, ".gitignore", "*\n")
	writeFile(t, root, "a.go", "package a\n\nfunc ProbeAlpha() {}\n")
	x := NewSymbolIndex(root, "")
	if got := paths(x.Lookup(context.Background(), "ProbeAlpha")); !reflect.DeepEqual(got, []string{"a.go:ProbeAlpha"}) {
		t.Fatalf("got %v", got)
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
	if tr := x.Truncated(); tr.Reason != TruncatedFiles || tr.Describe() != "stopped at 1 source files" {
		t.Fatalf("Truncated() = %+v (%q)", tr, tr.Describe())
	}
	y := NewSymbolIndex(fixtureRepo(t), "")
	y.maxDuration = -time.Second // every file is past the deadline
	y.Lookup(context.Background(), "Server")
	if tr := y.Truncated(); tr.Reason != TruncatedTime || tr.Describe() == "" {
		t.Fatalf("time bound: Truncated() = %+v", tr)
	}
	z := NewSymbolIndex(fixtureRepo(t), "")
	z.Lookup(context.Background(), "Server")
	if tr := z.Truncated(); tr.Reason != "" || tr.Describe() != "" {
		t.Fatalf("a complete index reports truncation: %+v", tr)
	}
}

// A lookup whose context ends while the first sync runs returns at once;
// the sync is not tied to it and completes the index for the next lookup.
func TestCancelledFirstLookupLeavesTheSyncRunning(t *testing.T) {
	root := fixtureRepo(t)
	x := NewSymbolIndex(root, "")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	x.Lookup(ctx, "Server")
	x.Flush()
	if tr := x.Truncated(); tr.Reason != "" {
		t.Fatalf("cancelled lookup left a truncated index: %+v", tr)
	}
	if syms := x.Lookup(context.Background(), "NewServer"); len(syms) != 1 {
		t.Fatalf("index incomplete after a cancelled first lookup: %+v", syms)
	}
}

// The sync applies its result only where the entry is still the one it
// read: an entry re-parsed meanwhile, or a file added meanwhile, wins.
func TestSyncYieldsToNewerEntries(t *testing.T) {
	x := NewSymbolIndex(t.TempDir(), "")
	old := newFileSymbols(1, 1, []Symbol{{Name: "Old", Signature: "func Old()"}})
	newer := newFileSymbols(3, 3, []Symbol{{Name: "Newer", Signature: "func Newer()"}})
	fromSync := newFileSymbols(2, 2, []Symbol{{Name: "Sync", Signature: "func Sync()"}})
	added := newFileSymbols(4, 4, nil)
	x.files = map[string]*fileSymbols{"a.go": old, "gone.go": old}
	snapshot := map[string]*fileSymbols{"a.go": old, "gone.go": old}
	x.files["a.go"] = newer   // re-parsed after the snapshot
	x.files["new.go"] = added // added after the snapshot
	x.mu.Lock()
	x.applySyncLocked(snapshot, syncResult{seen: map[string]struct{}{"a.go": {}}, parsed: map[string]*fileSymbols{"a.go": fromSync}}, time.Now())
	x.mu.Unlock()
	if x.files["a.go"] != newer || x.files["new.go"] != added {
		t.Fatal("the sync overwrote an entry newer than its snapshot")
	}
	if _, ok := x.files["gone.go"]; ok {
		t.Fatal("a file the sync did not see kept its entry")
	}
}

// The cache round-trips every entry, replaces the caches of earlier
// versions, and is ignored when written for another root or damaged.
func TestCacheRoundTripAndRejection(t *testing.T) {
	root := fixtureRepo(t)
	dir := filepath.Join(root, ".spettro", "cache")
	writeFile(t, root, ".spettro/cache/symbols.json", `{"version":1}`)
	writeFile(t, root, ".spettro/cache/symbols.gob", "gob")
	path := filepath.Join(dir, "symbols.idx")
	x := NewSymbolIndex(root, path)
	x.Warm(context.Background())
	for _, legacy := range legacyCacheNames {
		if _, err := os.Stat(filepath.Join(dir, legacy)); !os.IsNotExist(err) {
			t.Fatalf("legacy cache %s kept: %v", legacy, err)
		}
	}
	loaded, err := readCache(path, x.root)
	if err != nil {
		t.Fatal(err)
	}
	x.mu.Lock()
	if len(loaded) != len(x.files) {
		t.Fatalf("loaded %d entries, index has %d", len(loaded), len(x.files))
	}
	for rel, e := range x.files {
		if l := loaded[rel]; l == nil || !l.sameAs(e) || l.names != e.names {
			t.Fatalf("entry %s did not round-trip", rel)
		}
	}
	x.mu.Unlock()
	if _, err := readCache(path, t.TempDir()); err == nil {
		t.Fatal("a cache written for another root was accepted")
	}
	raw, _ := os.ReadFile(path)
	for _, cut := range []int{len(raw) - 1, len(raw) / 2, 20} {
		if err := os.WriteFile(path, raw[:cut], 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := readCache(path, x.root); err == nil {
			t.Fatalf("a cache cut at %d of %d bytes was accepted", cut, len(raw))
		}
	}
}

// Warm on an unchanged tree does not rewrite the cache: neither in the
// process that wrote it nor in a new one that loads it (the TUI warms the
// index at every start).
func TestWarmDoesNotRewriteAnUnchangedCache(t *testing.T) {
	root := fixtureRepo(t)
	path := filepath.Join(root, ".spettro", "cache", "symbols.idx")
	x := NewSymbolIndex(root, path)
	x.Warm(context.Background())
	past := time.Now().Add(-time.Hour).Truncate(time.Second)
	if err := os.Chtimes(path, past, past); err != nil {
		t.Fatal(err)
	}
	x.MarkStale()
	x.Warm(context.Background())
	NewSymbolIndex(root, path).Warm(context.Background())
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info.ModTime().Equal(past) {
		t.Fatalf("an unchanged index rewrote its cache (mtime %v)", info.ModTime())
	}
	// A real change is written by the sync that finds it.
	writeFile(t, root, "pkg/more.go", "package pkg\n\nfunc More() {}\n")
	y := NewSymbolIndex(root, path)
	y.Warm(context.Background())
	if info, _ := os.Stat(path); info.ModTime().Equal(past) {
		t.Fatal("a changed index was not saved")
	}
}

// Lookups, invalidations, stale marks, warm-ups and flushes from many
// goroutines at once: no data race, no panic, and the cache on disk ends
// at the final state (run under -race).
func TestConcurrentUseKeepsTheCacheCurrent(t *testing.T) {
	root := fixtureRepo(t)
	path := filepath.Join(root, ".spettro", "cache", "symbols.idx")
	x := NewSymbolIndex(root, path)
	ctx := context.Background()
	var wg sync.WaitGroup
	for g := range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 20 {
				switch (g + i) % 5 {
				case 0:
					x.Lookup(ctx, "Server")
				case 1:
					src := "package pkg\n\nfunc Churn" + string(rune('A'+g)) + "() {}\n"
					if err := os.WriteFile(filepath.Join(root, "pkg", "churn.go"), []byte(src), 0o644); err != nil {
						t.Error(err)
						return
					}
					x.Invalidate("pkg/churn.go")
				case 2:
					x.MarkStale()
					x.Lookup(ctx, "Churn")
				case 3:
					x.Warm(ctx)
				case 4:
					x.Flush()
				}
			}
		}()
	}
	wg.Wait()
	x.Warm(ctx)
	loaded, err := readCache(path, x.root)
	if err != nil {
		t.Fatal(err)
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	if len(loaded) != len(x.files) {
		t.Fatalf("cache has %d entries, index %d", len(loaded), len(x.files))
	}
	for rel, e := range x.files {
		if l := loaded[rel]; l == nil || !l.sameAs(e) {
			t.Fatalf("cache entry %s is not the index's", rel)
		}
	}
}

// A packed symbol stays 24 bytes (the index's memory is dominated by
// them and the signature text: 549k symbols on the perf corpus).
func TestPackedEntriesStayCompact(t *testing.T) {
	if size := unsafe.Sizeof(packedSymbol{}); size > 24 {
		t.Fatalf("packedSymbol is %d bytes, want <= 24", size)
	}
	syms := []Symbol{
		{Path: "a.go", Line: 3, Kind: "func", Name: "Foo", Signature: "func Foo() {"},
		{Path: "a.go", Line: 9, Kind: "method", Name: "Bar", Signature: "func (f *Foo) Bar() {"},
		{Path: "a.go", Line: 12, Kind: "custom", Name: "outside", Signature: "sig without the name"},
	}
	e := newFileSymbols(1, 2, syms)
	var got []Symbol
	for i := range e.syms {
		got = append(got, e.symbol("a.go", i))
	}
	if !reflect.DeepEqual(got, syms) {
		t.Fatalf("pack/unpack:\n got %+v\nwant %+v", got, syms)
	}
	if !slices.Equal(e.syms, newFileSymbols(1, 2, syms).syms) {
		t.Fatal("packing is not deterministic")
	}
}

func TestValidRel(t *testing.T) {
	for rel, want := range map[string]bool{
		"a.go": true, "pkg/a.go": true, ".hidden/a.go": true, "a..b.go": true,
		"": false, ".": false, "..": false, "../a.go": false, "a/../../b.go": false,
		"/etc/passwd.go": false, "a//b.go": false, "./a.go": false, "a/./b.go": false, "a/": false,
	} {
		if got := validRel(rel); got != want {
			t.Errorf("validRel(%q) = %v, want %v", rel, got, want)
		}
	}
}

// The cache is untrusted input (it lives in the project): a path leaving
// the root, or counts larger than the file could hold, reject it without
// allocating for the claimed counts.
func TestCacheRejectsHostileInput(t *testing.T) {
	root := t.TempDir()
	dir := t.TempDir()
	write := func(name string, fill func(w *cacheWriter)) string {
		t.Helper()
		p := filepath.Join(dir, name)
		f, err := os.Create(p)
		if err != nil {
			t.Fatal(err)
		}
		w := cacheWriter{w: bufio.NewWriter(f)}
		w.raw(cacheMagic)
		w.uint(cacheVersion)
		w.string(filepath.Clean(root))
		fill(&w)
		if err := w.w.Flush(); err != nil || w.err != nil {
			t.Fatal(err, w.err)
		}
		f.Close()
		return p
	}
	escape := write("escape.idx", func(w *cacheWriter) {
		w.uint(0) // kinds
		w.uint(1) // files
		w.string("../outside.go")
		w.uint(0)
		w.uint(0)
		w.string("")
		w.uint(0)
		w.string(cacheEnd)
	})
	huge := write("huge.idx", func(w *cacheWriter) {
		w.uint(0)
		w.uint(1 << 24) // claims 16M files in a few bytes
	})
	hugeSyms := write("hugesyms.idx", func(w *cacheWriter) {
		w.uint(0)
		w.uint(1)
		w.string("a.go")
		w.uint(0)
		w.uint(0)
		w.string("")
		w.uint(maxCacheString) // claims 64M symbols
	})
	for _, p := range []string{escape, huge, hugeSyms} {
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		if _, err := readCache(p, filepath.Clean(root)); err == nil {
			t.Errorf("%s was accepted", filepath.Base(p))
		}
		runtime.ReadMemStats(&after)
		if grew := after.TotalAlloc - before.TotalAlloc; grew > 1<<20 {
			t.Errorf("%s: the reader allocated %d bytes for a tiny file", filepath.Base(p), grew)
		}
	}
}
