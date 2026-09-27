package indexer

import (
	"bytes"
	"context"
	"encoding/gob"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"spettro/internal/fswalk"
	"spettro/internal/safeio"
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
	// syncTTL is how long a full re-sync with the disk stays good: a lookup
	// within it answers from memory (after re-parsing files invalidated
	// meanwhile). Spettro's own writes invalidate their file and its shell
	// commands mark the index stale, so only edits made outside Spettro can
	// go unseen, for at most this long.
	syncTTL = 5 * time.Second
)

// fileSymbols is the cached index entry for one source file. Entries are
// never modified once built (a change replaces the entry), so a snapshot of
// the map can be saved while lookups go on.
type fileSymbols struct {
	ModTime time.Time
	Size    int64
	Symbols []Symbol
	// names is "\n" + each symbol's lower-cased name + "\n"..., so one
	// strings.Contains tells whether the file can hold a match. Derived,
	// not persisted.
	names string
}

func newFileSymbols(mod time.Time, size int64, syms []Symbol) *fileSymbols {
	fs := &fileSymbols{ModTime: mod, Size: size, Symbols: syms}
	fs.index()
	return fs
}

// index fills names.
func (fs *fileSymbols) index() {
	var b strings.Builder
	for _, s := range fs.Symbols {
		b.WriteByte('\n')
		b.WriteString(strings.ToLower(s.Name))
	}
	b.WriteByte('\n')
	fs.names = b.String()
}

// symbolCache is the on-disk gob layout.
type symbolCache struct {
	Version int
	Root    string
	Files   map[string]*fileSymbols
}

// cacheVersion 2 is the gob cache (1 was JSON in symbols.json).
const cacheVersion = 2

// SymbolIndex is a lazily built, cached symbol index for one project root.
// The zero value is not usable; construct with NewSymbolIndex, or get the
// process-wide one for a root with Shared.
//
// Cache: files maps a root-relative path to the symbols of that file as of
// its (mtime, size). It is filled by a full sync with the disk (a walk that
// re-parses new and changed files and drops deleted ones), which is redone
// when the last one is older than syncTTL or MarkStale was called;
// Invalidate queues one file for re-parsing at the next lookup. It is
// persisted (gob) at cachePath after a sync that changed it, and reloaded by
// the first sync of a new process. Concurrency: mu guards everything but the
// persistence; Lookup, Warm, Invalidate and MarkStale may be called from any
// goroutine.
type SymbolIndex struct {
	root        string
	cachePath   string // "" disables persistence
	extractors  []Extractor
	maxFiles    int
	maxDuration time.Duration

	mu        sync.Mutex
	loaded    bool      // the persisted cache was read (or there was none)
	lastSync  time.Time // zero: never synced in this process
	stale     bool
	dirty     map[string]struct{} // Invalidate'd paths not re-parsed yet
	truncated bool                // the last sync stopped at maxFiles or maxDuration
	syncs     int                 // full syncs run, for tests
	files     map[string]*fileSymbols
	byExt     map[string]Extractor

	saver cacheSaver
}

// NewSymbolIndex creates an index for root, persisting its cache at cachePath
// (pass "" for in-memory only). Nothing is scanned until the first Lookup.
func NewSymbolIndex(root, cachePath string) *SymbolIndex {
	x := &SymbolIndex{
		root:        root,
		cachePath:   cachePath,
		extractors:  DefaultExtractors(),
		maxFiles:    defaultMaxFiles,
		maxDuration: defaultMaxDuration,
		dirty:       map[string]struct{}{},
		files:       map[string]*fileSymbols{},
		byExt:       map[string]Extractor{},
	}
	for _, e := range x.extractors {
		for _, ext := range e.Extensions() {
			x.byExt[ext] = e
		}
	}
	return x
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

// Lookup returns symbols matching name, definitions ranked best-first:
// exact-name matches, then prefix matches, then substring matches
// (case-insensitive within each tier). It brings the index up to date first
// (see SymbolIndex), so results reflect the files as they are on disk.
func (x *SymbolIndex) Lookup(ctx context.Context, name string) []Symbol {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	x.ensureLocked(ctx, false)

	lower := strings.ToLower(name)
	type scored struct {
		sym  Symbol
		rank int
	}
	var hits []scored
	for _, entry := range x.files {
		if !strings.Contains(entry.names, lower) {
			continue
		}
		for _, s := range entry.Symbols {
			ln := strings.ToLower(s.Name)
			var rank int
			switch {
			case s.Name == name:
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
			hits = append(hits, scored{s, rank})
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

// Warm builds (or refreshes) the index and persists its cache without
// running a query, so hosts can pay the first-scan cost in the background at
// startup instead of on the first symbol search. It saves synchronously.
func (x *SymbolIndex) Warm(ctx context.Context) {
	x.mu.Lock()
	x.ensureLocked(ctx, true)
	x.mu.Unlock()
	x.Flush()
}

// Invalidate queues a root-relative path for re-parsing at the next Lookup,
// for callers that just wrote it (the agent's file tools). A sync would
// notice the change by its mtime and size too, but only after syncTTL.
func (x *SymbolIndex) Invalidate(relPath string) {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.dirty[filepath.ToSlash(relPath)] = struct{}{}
}

// MarkStale makes the next Lookup re-sync with the disk in full, for callers
// that may have changed any file (a shell command).
func (x *SymbolIndex) MarkStale() {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.stale = true
}

// Truncated reports whether the last sync stopped before indexing every
// source file (the file cap or the time bound), and the file cap.
func (x *SymbolIndex) Truncated() (bool, int) {
	x.mu.Lock()
	defer x.mu.Unlock()
	return x.truncated, x.maxFiles
}

// Flush waits for cache saves started so far to finish.
func (x *SymbolIndex) Flush() { x.saver.wait() }

// ensureLocked brings the index up to date: a full sync when none ran within
// syncTTL (or MarkStale asked for one), otherwise just the re-parse of
// invalidated files. saveNow persists synchronously. Callers hold x.mu.
func (x *SymbolIndex) ensureLocked(ctx context.Context, saveNow bool) {
	if !x.loaded {
		x.loadCache()
		x.loaded = true
	}
	changed := false
	if x.stale || x.lastSync.IsZero() || time.Since(x.lastSync) >= syncTTL {
		changed = x.syncLocked(ctx)
	} else if len(x.dirty) > 0 {
		changed = x.reparseDirtyLocked()
	}
	if changed || saveNow && x.saver.pendingSince(x.lastSync) {
		x.saveLocked(saveNow)
	}
}

// reparseDirtyLocked re-reads the invalidated files. Callers hold x.mu.
func (x *SymbolIndex) reparseDirtyLocked() bool {
	for rel := range x.dirty {
		delete(x.dirty, rel)
		abs := filepath.Join(x.root, filepath.FromSlash(rel))
		info, err := os.Stat(abs)
		ext, ok := x.byExt[strings.ToLower(filepath.Ext(rel))]
		if err != nil || !ok || !info.Mode().IsRegular() || info.Size() > maxFileSize {
			delete(x.files, rel)
			continue
		}
		if entry, ok := x.parse(ext, abs, rel, info); ok {
			x.files[rel] = entry
		}
	}
	return true
}

// parse reads and extracts one file.
func (x *SymbolIndex) parse(ext Extractor, abs, rel string, info os.FileInfo) (*fileSymbols, bool) {
	src, err := os.ReadFile(abs)
	if err != nil {
		return nil, false
	}
	return newFileSymbols(info.ModTime(), info.Size(), ext.Extract(rel, src)), true
}

// parseJob is one file a sync has to (re)parse.
type parseJob struct {
	abs, rel string
	info     os.FileInfo
	ext      Extractor
}

// syncLocked walks the root (skipDir names, .gitignore files layered as the
// search tools apply them, files over maxFileSize skipped), re-parses new and
// changed files in parallel and drops deleted ones. Bounded by maxFiles and
// maxDuration; what was not reached keeps no entry. It reports whether
// anything changed. Callers hold x.mu.
func (x *SymbolIndex) syncLocked(ctx context.Context) bool {
	deadline := time.Now().Add(x.maxDuration)
	seen := make(map[string]struct{}, len(x.files))
	var jobs []parseJob
	truncated := false
	walker := fswalk.Walker{Base: x.root, SkipDir: skipDir, Ignores: fswalk.NewIgnoreSet(), Stat: true}
	walkErr := walker.Walk(ctx, x.root, func(e fswalk.Entry) error {
		ext, ok := x.byExt[strings.ToLower(filepath.Ext(e.Rel))]
		if !ok || e.Info.Size() > maxFileSize {
			return nil
		}
		if len(seen) >= x.maxFiles || time.Now().After(deadline) {
			truncated = true
			return fswalk.ErrStop
		}
		seen[e.Rel] = struct{}{}
		if prev, ok := x.files[e.Rel]; ok && prev.ModTime.Equal(e.Info.ModTime()) && prev.Size == e.Info.Size() {
			if _, dirty := x.dirty[e.Rel]; !dirty {
				return nil // cache hit
			}
		}
		jobs = append(jobs, parseJob{abs: e.Abs, rel: e.Rel, info: e.Info, ext: ext})
		return nil
	})
	if walkErr != nil { // cancelled: keep what we had, retry next time
		return false
	}
	parsed := x.parseAll(ctx, jobs, deadline)
	changed := false
	for rel, entry := range parsed {
		x.files[rel] = entry
		changed = true
	}
	for _, j := range jobs {
		if _, ok := parsed[j.rel]; !ok {
			delete(seen, j.rel) // unreadable, or past the deadline
			truncated = truncated || time.Now().After(deadline)
		}
	}
	for rel := range x.files {
		if _, ok := seen[rel]; !ok {
			delete(x.files, rel)
			changed = true
		}
	}
	x.dirty = map[string]struct{}{}
	x.syncs++
	x.truncated = truncated
	x.stale = false
	x.lastSync = time.Now()
	return changed
}

// parseAll extracts jobs on runtime.NumCPU() goroutines (extraction is CPU
// bound once the prefilter has skipped most lines). Jobs not started by the
// deadline are left out of the result.
func (x *SymbolIndex) parseAll(ctx context.Context, jobs []parseJob, deadline time.Time) map[string]*fileSymbols {
	out := make(map[string]*fileSymbols, len(jobs))
	if len(jobs) == 0 {
		return out
	}
	var mu sync.Mutex
	var wg sync.WaitGroup
	next := make(chan parseJob)
	for range min(runtime.NumCPU(), len(jobs)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range next {
				if ctx.Err() != nil || time.Now().After(deadline) {
					continue
				}
				if entry, ok := x.parse(j.ext, j.abs, j.rel, j.info); ok {
					mu.Lock()
					out[j.rel] = entry
					mu.Unlock()
				}
			}
		}()
	}
	for _, j := range jobs {
		next <- j
	}
	close(next)
	wg.Wait()
	return out
}

func skipDir(name string) bool {
	switch name {
	case ".git", ".spettro", "vendor", "node_modules", "dist", "build", "__pycache__", ".venv", "venv":
		return true
	}
	return false
}

func (x *SymbolIndex) loadCache() {
	if x.cachePath == "" {
		return
	}
	raw, err := os.ReadFile(x.cachePath)
	if err != nil {
		return
	}
	var c symbolCache
	if gob.NewDecoder(bytes.NewReader(raw)).Decode(&c) != nil || c.Version != cacheVersion || c.Root != x.root || c.Files == nil {
		return
	}
	for _, entry := range c.Files {
		entry.index()
	}
	x.files = c.Files
}

// saveLocked persists a snapshot of the index: synchronously when now is
// set, otherwise on the saver's goroutine. Callers hold x.mu.
func (x *SymbolIndex) saveLocked(now bool) {
	if x.cachePath == "" {
		return
	}
	snap := symbolCache{Version: cacheVersion, Root: x.root, Files: maps.Clone(x.files)}
	x.saver.save(x.cachePath, snap, x.lastSync, now)
}

// cacheSaver writes cache snapshots off the lookup path.
//
// Ordering guarantee: saves run one at a time (mu), and a snapshot older
// than the last one written (by its sync time) is dropped, so the file on
// disk never goes back to an earlier state even when saves finish out of
// order.
type cacheSaver struct {
	mu      sync.Mutex
	wg      sync.WaitGroup
	written time.Time // sync time of the snapshot on disk

	stateMu sync.Mutex
	queued  time.Time // sync time of the newest snapshot handed over
}

// pendingSince reports whether the state as of syncedAt has not been handed
// to a save yet.
func (s *cacheSaver) pendingSince(syncedAt time.Time) bool {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	return s.queued.Before(syncedAt)
}

func (s *cacheSaver) save(path string, snap symbolCache, syncedAt time.Time, now bool) {
	s.stateMu.Lock()
	s.queued = syncedAt
	s.stateMu.Unlock()
	s.wg.Add(1)
	write := func() {
		defer s.wg.Done()
		s.mu.Lock()
		defer s.mu.Unlock()
		if syncedAt.Before(s.written) {
			return
		}
		if writeCache(path, snap) == nil {
			s.written = syncedAt
		}
	}
	if now {
		write()
		return
	}
	go write()
}

func (s *cacheSaver) wait() { s.wg.Wait() }

// writeCache encodes snap to path atomically. Best-effort persistence: a
// failed write just means a cold rebuild next session, never a failed
// search. The JSON cache of earlier versions is removed.
func writeCache(path string, snap symbolCache) error {
	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(snap); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, buf.Bytes(), 0o644); err != nil {
		return err
	}
	if err := safeio.Replace(tmp, path); err != nil {
		return err
	}
	if legacy := filepath.Join(filepath.Dir(path), "symbols.json"); legacy != path {
		_ = os.Remove(legacy)
	}
	return nil
}
