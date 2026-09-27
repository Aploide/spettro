package indexer

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"spettro/internal/fswalk"
)

// Build/scan bounds. The index must never make a symbol search slower than plain
// grep in the fallback case, so an oversized or slow repo simply stops
// indexing where the cap hits and later queries answer from what was indexed
// (Truncated reports it).
const (
	// defaultMaxFiles caps the source files indexed (product decision D10:
	// raised from 20k now that the scan reads and parses in parallel; a
	// 61k-file tree's 42k Go files fit).
	defaultMaxFiles    = 100000
	defaultMaxDuration = 10 * time.Second
	maxFileSize        = 1 << 20 // skip files >1MiB, generated/vendored blobs
	// syncTTL is how old the last full sync with the disk may get before a
	// lookup starts another one in the background. Lookups never wait for
	// it (see SymbolIndex), so it trades the CPU of a re-walk (1.3 s over
	// the 60k-file perf corpus) against how soon the index notices changes
	// no other path reports.
	syncTTL = 30 * time.Second
	// verifyHits is how many of a lookup's best hits have their files
	// re-checked on disk before the lookup answers.
	verifyHits = 50
)

// SymbolIndex is a lazily built, cached symbol index for one project root.
// The zero value is not usable; construct with NewSymbolIndex, or get the
// process-wide one for a root with Shared.
//
// Cache: files maps a root-relative path to the symbols of that file as of
// its (mtime, size). It is persisted at cachePath and reloaded by the first
// use in a new process. It is brought up to date by:
//
//   - a full sync, a walk that re-parses new and changed files and drops
//     deleted ones. The first lookup of a process with no usable disk cache
//     waits for one; afterwards they run in the background, started by a
//     lookup when the last one is older than syncTTL or MarkStale was
//     called (a shell command, a checkpoint rewind). A lookup never waits
//     for a background sync: it answers from memory at once.
//   - Invalidate, for files Spettro itself wrote: re-parsed before the next
//     lookup answers.
//   - Refresh, for the files a caller has just seen on disk (the symbol
//     search passes the files its grep matched): each is re-checked by its
//     mtime and size and re-parsed or added when it changed.
//   - the lookup itself: the files of its best verifyHits hits are
//     re-checked the same way before it answers, so a listed definition
//     never points into a file that changed or went away.
//
// So a definition can be missed only in a file changed outside Spettro
// that neither the caller's grep nor the lookup's hits covered, until the
// background sync that lookup started has finished.
//
// The cache on disk is rewritten (in the background, see saveLoop) only
// after a sync that changed the index, and by Warm when the state it
// leaves differs from the disk; re-parses alone mark it unsaved for the
// next sync to write.
//
// Concurrency: mu guards all state. Lookup, Refresh, Warm, Invalidate,
// MarkStale, Truncated and Flush may be called from any goroutine. At most
// one sync and one save run at a time, each on a goroutine of its own; a
// sync walks and parses without holding mu and applies its result under
// it (see applySyncLocked for how it yields to newer entries).
type SymbolIndex struct {
	root        string
	cachePath   string // "" disables persistence
	extractors  []Extractor
	byExt       map[string]Extractor
	maxFiles    int
	maxDuration time.Duration

	mu       sync.Mutex
	idle     *sync.Cond // broadcast when a sync or a save ends
	files    map[string]*fileSymbols
	loaded   bool      // the disk cache was read (or found missing)
	ready    bool      // files come from a finished sync or a usable cache
	lastSync time.Time // start of the last finished sync; zero: none yet
	stale    bool      // MarkStale since the running or last sync started
	dirty    map[string]struct{}
	trunc    Truncation
	syncDone chan struct{} // non-nil while a sync runs; closed when it ends
	syncs    int           // full syncs run, for tests

	gen       uint64 // bumped on every change to files
	savedGen  uint64 // gen of the state last written to (or read from) disk
	saving    bool   // saveLoop is running
	saveAgain bool   // a save was requested while saveLoop was writing
}

// NewSymbolIndex creates an index for root, persisting its cache at cachePath
// (pass "" for in-memory only). Nothing is scanned until the first Lookup.
func NewSymbolIndex(root, cachePath string) *SymbolIndex {
	x := &SymbolIndex{
		root:        filepath.Clean(root),
		cachePath:   cachePath,
		extractors:  DefaultExtractors(),
		maxFiles:    defaultMaxFiles,
		maxDuration: defaultMaxDuration,
		dirty:       map[string]struct{}{},
		files:       map[string]*fileSymbols{},
		byExt:       map[string]Extractor{},
	}
	x.idle = sync.NewCond(&x.mu)
	for _, e := range x.extractors {
		for _, ext := range e.Extensions() {
			x.byExt[ext] = e
		}
	}
	return x
}

// SetLimits changes the file cap and time bound of the syncs started from
// now on (the defaults are 100000 files and 10 s).
func (x *SymbolIndex) SetLimits(maxFiles int, maxDuration time.Duration) {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.maxFiles, x.maxDuration = maxFiles, maxDuration
}

var (
	sharedMu sync.Mutex
	shared   = map[string]*SymbolIndex{}
)

// Shared returns the process-wide index for root, creating it (persisted at
// cachePath) on first use, so every session, the TUI's startup warm-up and
// the agent's lookups share one index and its warm state. The first
// caller's cachePath wins for a root.
func Shared(root, cachePath string) *SymbolIndex {
	root = filepath.Clean(root)
	sharedMu.Lock()
	defer sharedMu.Unlock()
	if x, ok := shared[root]; ok {
		return x
	}
	x := NewSymbolIndex(root, cachePath)
	shared[root] = x
	return x
}

// Truncation says whether, and why, the last sync stopped before indexing
// every source file.
type Truncation struct {
	// Reason is "" for a complete index, TruncatedFiles or TruncatedTime.
	Reason      string
	MaxFiles    int
	MaxDuration time.Duration
}

// The reasons a sync stops early.
const (
	TruncatedFiles = "files" // the source file cap
	TruncatedTime  = "time"  // the time bound
)

// Describe says what cut the index short, for a notice ("stopped at 100000
// source files"); "" for a complete index.
func (t Truncation) Describe() string {
	switch t.Reason {
	case TruncatedFiles:
		return fmt.Sprintf("stopped at %d source files", t.MaxFiles)
	case TruncatedTime:
		return fmt.Sprintf("stopped after %s", t.MaxDuration)
	}
	return ""
}

// Lookup returns symbols matching name, definitions ranked best-first:
// exact-name matches, then prefix matches, then substring matches
// (case-insensitive within each tier). See SymbolIndex for how current the
// answer is; it never waits for a background sync.
func (x *SymbolIndex) Lookup(ctx context.Context, name string) []Symbol {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	x.prepareLocked(ctx)
	hits := x.rankLocked(name)
	if x.verifyLocked(hits) {
		hits = x.rankLocked(name)
	}
	return hits
}

// Refresh re-checks the given root-relative files against the disk before
// a lookup: a file whose mtime or size changed is re-parsed, a new one
// added (when the index would include it: a known extension, not below a
// skipped directory, not ignored), a missing one dropped. It is cheap (one
// stat per file) and does nothing before the index has its first contents,
// which the next lookup's sync supplies anyway.
func (x *SymbolIndex) Refresh(ctx context.Context, relPaths []string) {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.loadLocked()
	if !x.ready {
		return
	}
	f := x.newFilter()
	seen := make(map[string]struct{}, len(relPaths))
	for _, rel := range relPaths {
		if ctx.Err() != nil {
			return
		}
		rel = filepath.ToSlash(filepath.Clean(rel))
		if _, dup := seen[rel]; dup {
			continue
		}
		seen[rel] = struct{}{}
		x.refreshFileLocked(rel, f, false)
	}
}

// Warm builds (or refreshes) the index and persists its cache without
// running a query, so hosts can pay the first-scan cost in the background at
// startup instead of on the first symbol search. It waits for the sync and
// for the cache write, if the state differs from the disk.
func (x *SymbolIndex) Warm(ctx context.Context) {
	x.mu.Lock()
	x.loadLocked()
	x.reparseDirtyLocked()
	if !x.ready || x.syncDueLocked() {
		x.startSyncLocked()
	}
	x.waitSyncLocked(ctx)
	if x.gen != x.savedGen {
		x.requestSaveLocked()
	}
	x.mu.Unlock()
	x.Flush()
}

// Invalidate queues a root-relative path for re-parsing before the next
// lookup answers, for callers that just wrote it (the agent's file tools).
// A sync would notice the change by its mtime and size too, but later.
func (x *SymbolIndex) Invalidate(relPath string) {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.dirty[filepath.ToSlash(filepath.Clean(relPath))] = struct{}{}
}

// MarkStale makes the next lookup start a full background sync, for
// callers that may have changed any file (a shell command, a rewind).
func (x *SymbolIndex) MarkStale() {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.stale = true
}

// Truncated reports whether, and why, the last sync stopped before
// indexing every source file.
func (x *SymbolIndex) Truncated() Truncation {
	x.mu.Lock()
	defer x.mu.Unlock()
	return x.trunc
}

// Flush waits until the running sync, if any, and the cache writes started
// so far have finished. Tests and benchmarks use it to observe a settled
// index; Warm uses it to persist before returning.
func (x *SymbolIndex) Flush() {
	x.mu.Lock()
	defer x.mu.Unlock()
	for x.syncDone != nil || x.saving {
		x.idle.Wait()
	}
}

// prepareLocked brings the index to the state a lookup answers from: the
// disk cache loaded, the first sync done when there was no usable cache,
// invalidated files re-parsed, and a background sync started when one is
// due. Callers hold mu.
func (x *SymbolIndex) prepareLocked(ctx context.Context) {
	x.loadLocked()
	if !x.ready {
		x.startSyncLocked()
		x.waitSyncLocked(ctx)
	}
	x.reparseDirtyLocked()
	if x.syncDueLocked() {
		x.startSyncLocked()
	}
}

// syncDueLocked reports whether a background sync should start.
func (x *SymbolIndex) syncDueLocked() bool {
	return x.stale || x.lastSync.IsZero() || time.Since(x.lastSync) >= syncTTL
}

// waitSyncLocked waits for the running sync, if any, to finish or for ctx
// to end, whichever is first. The sync carries on regardless. Callers hold
// mu; it is released while waiting.
func (x *SymbolIndex) waitSyncLocked(ctx context.Context) {
	done := x.syncDone
	if done == nil {
		return
	}
	x.mu.Unlock()
	select {
	case <-done:
	case <-ctx.Done():
	}
	x.mu.Lock()
}

// loadLocked reads the disk cache once per index. Callers hold mu.
func (x *SymbolIndex) loadLocked() {
	if x.loaded {
		return
	}
	x.loaded = true
	if x.cachePath == "" {
		return
	}
	files, err := readCache(x.cachePath, x.root)
	if err != nil {
		return
	}
	x.files = files
	x.ready = true
	x.gen++
	x.savedGen = x.gen // the disk holds exactly this state
}

// rankLocked finds and orders the matches of name. Callers hold mu.
func (x *SymbolIndex) rankLocked(name string) []Symbol {
	lower := strings.ToLower(name)
	type scored struct {
		sym  Symbol
		rank int
	}
	var hits []scored
	for path, entry := range x.files {
		if !strings.Contains(entry.names, lower) {
			continue
		}
		for i := range entry.syms {
			n := entry.name(i)
			ln := strings.ToLower(n)
			var rank int
			switch {
			case n == name:
				rank = 0
			case ln == lower:
				rank = 1
			case strings.HasPrefix(ln, lower):
				rank = 2
			case strings.Contains(ln, lower):
				rank = 3
			default:
				continue
			}
			hits = append(hits, scored{entry.symbol(path, i), rank})
		}
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].rank != hits[j].rank {
			return hits[i].rank < hits[j].rank
		}
		if hits[i].sym.Path != hits[j].sym.Path {
			return hits[i].sym.Path < hits[j].sym.Path
		}
		return hits[i].sym.Line < hits[j].sym.Line
	})
	out := make([]Symbol, len(hits))
	for i, h := range hits {
		out[i] = h.sym
	}
	return out
}

// verifyLocked re-checks the files of the best verifyHits hits on disk and
// reports whether any entry changed. Callers hold mu.
func (x *SymbolIndex) verifyLocked(hits []Symbol) bool {
	changed := false
	checked := map[string]struct{}{}
	for _, h := range hits[:min(len(hits), verifyHits)] {
		if _, done := checked[h.Path]; done {
			continue
		}
		checked[h.Path] = struct{}{}
		if x.refreshFileLocked(h.Path, nil, false) {
			changed = true
		}
	}
	return changed
}

// reparseDirtyLocked re-reads the invalidated files, applying the same
// filters a sync does (an ignored or skipped file written by the agent is
// not indexed). Callers hold mu.
func (x *SymbolIndex) reparseDirtyLocked() {
	if len(x.dirty) == 0 {
		return
	}
	f := x.newFilter()
	for rel := range x.dirty {
		delete(x.dirty, rel)
		x.refreshFileLocked(rel, f, true)
	}
}

// refreshFileLocked brings one file's entry up to date with the disk:
// re-parsed when forced or when its mtime or size changed, dropped when it
// is gone, no longer a small regular file, or (with a filter) excluded. It
// reports whether the entry changed. Callers hold mu.
func (x *SymbolIndex) refreshFileLocked(rel string, f *indexFilter, force bool) bool {
	prev := x.files[rel]
	ext, ok := x.byExt[strings.ToLower(filepath.Ext(rel))]
	if ok && f != nil {
		ok = f.includes(rel)
	}
	abs := filepath.Join(x.root, filepath.FromSlash(rel))
	info, err := os.Stat(abs)
	if !ok || err != nil || !info.Mode().IsRegular() || info.Size() > maxFileSize {
		return x.dropLocked(rel)
	}
	if !force && prev != nil && prev.modTime == info.ModTime().UnixNano() && prev.size == info.Size() {
		return false
	}
	entry, ok := parseFile(ext, abs, rel, info)
	if !ok {
		return x.dropLocked(rel)
	}
	if prev != nil && prev.sameAs(entry) {
		return false
	}
	x.files[rel] = entry
	x.gen++
	return true
}

// dropLocked removes rel's entry, reporting whether there was one.
func (x *SymbolIndex) dropLocked(rel string) bool {
	if _, had := x.files[rel]; !had {
		return false
	}
	delete(x.files, rel)
	x.gen++
	return true
}

// parseFile reads and extracts one file; info is its stat.
func parseFile(ext Extractor, abs, rel string, info fsInfo) (*fileSymbols, bool) {
	src, err := os.ReadFile(abs)
	if err != nil {
		return nil, false
	}
	return newFileSymbols(info.ModTime().UnixNano(), info.Size(), ext.Extract(rel, src)), true
}

// skipDir names the directories the index never descends into.
func skipDir(name string) bool {
	switch name {
	case ".git", ".spettro", "vendor", "node_modules", "dist", "build", "__pycache__", ".venv", "venv":
		return true
	}
	return false
}

// syncWorkers is how many directories a sync lists (and stats) at once. A
// sync runs in the background, so CPU matters more than its wall time: a
// re-sync of the 60k-file perf corpus took 0.33 s and 0.75 s of CPU with 2
// workers, 0.24 s and 1.03 s with 4 (the search tools' setting).
const syncWorkers = 2

// newWalker returns the walker a sync uses: only files with an indexed
// extension, skipDir names pruned, the .gitignore files from the root down
// applied (not those above it: a dotfiles ~/.gitignore holding "*" must
// not empty the index of every project below the home directory),
// symlinked files included.
func (x *SymbolIndex) newWalker() fswalk.Walker {
	return fswalk.Walker{
		Base:           x.root,
		SkipDir:        skipDir,
		Ignores:        fswalk.NewIgnoreSetFrom(x.root),
		FileName:       x.indexedName,
		SymlinkedFiles: true,
		Stat:           true,
		Workers:        syncWorkers,
	}
}

// indexedName reports whether a file name has an extension the index
// parses.
func (x *SymbolIndex) indexedName(name string) bool {
	_, ok := x.byExt[strings.ToLower(filepath.Ext(name))]
	return ok
}

// indexFilter applies a sync's directory and .gitignore rules to single
// files (Invalidate, Refresh). Its .gitignore cache lives as long as it.
type indexFilter struct {
	w fswalk.Walker
}

func (x *SymbolIndex) newFilter() *indexFilter { return &indexFilter{w: x.newWalker()} }

// includes reports whether a sync would reach and keep rel: no directory
// on its path is skipped or ignored, and the file itself is not ignored.
func (f *indexFilter) includes(rel string) bool {
	if rel == "." || strings.HasPrefix(rel, "../") || filepath.IsAbs(filepath.FromSlash(rel)) {
		return false
	}
	dirs := strings.Split(rel, "/")
	for _, d := range dirs[:len(dirs)-1] {
		if skipDir(d) {
			return false
		}
	}
	return !f.w.IgnoredBelow(".", rel)
}
