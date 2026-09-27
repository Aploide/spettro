package indexer

import (
	"context"
	"maps"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"spettro/internal/fswalk"
)

// startSyncLocked starts a full sync on a goroutine of its own, unless one
// is running. Callers hold mu.
func (x *SymbolIndex) startSyncLocked() {
	if x.syncDone != nil {
		return
	}
	done := make(chan struct{})
	x.syncDone = done
	x.stale = false // a MarkStale from now on asks for the next sync
	snapshot := maps.Clone(x.files)
	bounds := Truncation{MaxFiles: x.maxFiles, MaxDuration: x.maxDuration}
	go x.runSync(snapshot, bounds, done)
}

// runSync compares the disk with snapshot (the entries as the sync
// started), without holding mu, then applies the result. bounds holds the
// file cap and time bound as the sync started.
func (x *SymbolIndex) runSync(snapshot map[string]*fileSymbols, bounds Truncation, done chan struct{}) {
	started := time.Now()
	res := x.scan(snapshot, bounds, started.Add(bounds.MaxDuration))
	x.mu.Lock()
	defer x.mu.Unlock()
	x.applySyncLocked(snapshot, res, started)
	x.syncDone = nil
	close(done)
	if x.gen != x.savedGen {
		x.requestSaveLocked()
	}
	x.idle.Broadcast()
}

// syncResult is what a sync found on disk.
type syncResult struct {
	seen   map[string]struct{}     // source files the walk reached
	parsed map[string]*fileSymbols // new and changed files, re-parsed
	trunc  Truncation
}

// parseJob is one file a sync has to (re)parse.
type parseJob struct {
	abs, rel string
	info     fsInfo
	ext      Extractor
}

// fsInfo is the part of a file's stat a parse records.
type fsInfo interface {
	ModTime() time.Time
	Size() int64
}

// scan walks the root (see newWalker; files over maxFileSize skipped),
// re-parses in parallel the files that are new or changed against
// snapshot, bounded by bounds.MaxFiles and the deadline.
func (x *SymbolIndex) scan(snapshot map[string]*fileSymbols, bounds Truncation, deadline time.Time) syncResult {
	res := syncResult{seen: make(map[string]struct{}, len(snapshot))}
	var jobs []parseJob
	_ = x.newWalker().Walk(context.Background(), x.root, func(e fswalk.Entry) error {
		ext, ok := x.byExt[strings.ToLower(filepath.Ext(e.Rel))]
		if !ok || e.Info.Size() > maxFileSize {
			return nil
		}
		if len(res.seen) >= bounds.MaxFiles {
			res.trunc = bounds
			res.trunc.Reason = TruncatedFiles
			return fswalk.ErrStop
		}
		if time.Now().After(deadline) {
			res.trunc = bounds
			res.trunc.Reason = TruncatedTime
			return fswalk.ErrStop
		}
		res.seen[e.Rel] = struct{}{}
		if prev := snapshot[e.Rel]; prev != nil && prev.modTime == e.Info.ModTime().UnixNano() && prev.size == e.Info.Size() {
			return nil // cache hit
		}
		jobs = append(jobs, parseJob{abs: e.Abs, rel: e.Rel, info: e.Info, ext: ext})
		return nil
	})
	var unread []string
	res.parsed, unread = parseAll(jobs, deadline)
	for _, rel := range unread {
		delete(res.seen, rel) // vanished or unreadable: drop its entry
	}
	if len(res.parsed)+len(unread) < len(jobs) && res.trunc.Reason == "" {
		res.trunc = bounds
		res.trunc.Reason = TruncatedTime
	}
	return res
}

// parseAll extracts jobs on runtime.NumCPU() goroutines (extraction is CPU
// bound once the prefilter has skipped most lines). Jobs not started by the
// deadline are in neither result; unread lists the files that could not be
// read.
func parseAll(jobs []parseJob, deadline time.Time) (parsed map[string]*fileSymbols, unread []string) {
	parsed = make(map[string]*fileSymbols, len(jobs))
	if len(jobs) == 0 {
		return parsed, nil
	}
	var mu sync.Mutex
	var wg sync.WaitGroup
	next := make(chan parseJob)
	for range min(runtime.NumCPU(), len(jobs)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range next {
				if time.Now().After(deadline) {
					continue
				}
				entry, ok := parseFile(j.ext, j.abs, j.rel, j.info)
				mu.Lock()
				if ok {
					parsed[j.rel] = entry
				} else {
					unread = append(unread, j.rel)
				}
				mu.Unlock()
			}
		}()
	}
	for _, j := range jobs {
		next <- j
	}
	close(next)
	wg.Wait()
	return parsed, unread
}

// applySyncLocked merges a sync's result into the index.
//
// Ordering guarantee: the sync compared the disk with a snapshot taken when
// it started, while lookups went on re-parsing files (Invalidate, Refresh,
// hit checks). An entry that changed since the snapshot is newer than
// anything the sync read, so the sync only replaces or deletes an entry
// that is still the one it saw (compared by pointer; entries are
// immutable), and never touches files added meanwhile. Files cut off by the
// file cap lose their entries (the cap bounds the index's memory); files
// cut off by the time bound keep them (lookups re-check the files of their
// hits). Callers hold mu.
func (x *SymbolIndex) applySyncLocked(snapshot map[string]*fileSymbols, res syncResult, started time.Time) {
	for rel, entry := range res.parsed {
		cur := x.files[rel]
		if cur != snapshot[rel] || cur != nil && cur.sameAs(entry) {
			continue
		}
		x.files[rel] = entry
		x.gen++
	}
	if res.trunc.Reason != TruncatedTime {
		for rel, entry := range snapshot {
			if _, ok := res.seen[rel]; !ok && x.files[rel] == entry {
				delete(x.files, rel)
				x.gen++
			}
		}
	}
	x.trunc = res.trunc
	x.syncs++
	x.lastSync = started
	x.ready = true
}

// requestSaveLocked schedules a cache write of the current state. Callers
// hold mu.
func (x *SymbolIndex) requestSaveLocked() {
	if x.cachePath == "" {
		return
	}
	if x.saving {
		x.saveAgain = true
		return
	}
	x.saving = true
	go x.saveLoop()
}

// saveLoop writes the cache until no save is pending.
//
// Ordering guarantee: only one saveLoop runs at a time, and each write
// takes its snapshot when it starts, after the previous write finished; a
// save requested during a write makes the loop write once more with the
// then-current state. So the file on disk only ever moves forward, and a
// burst of requests costs at most one extra write. Entries are immutable,
// so the snapshot is a list of pointers taken under mu and encoded without
// it.
func (x *SymbolIndex) saveLoop() {
	x.mu.Lock()
	for {
		x.saveAgain = false
		gen := x.gen
		snap := make([]cachedFile, 0, len(x.files))
		for path, e := range x.files {
			snap = append(snap, cachedFile{path: path, entries: e})
		}
		x.mu.Unlock()
		err := writeCache(x.cachePath, x.root, snap)
		x.mu.Lock()
		if err == nil && gen > x.savedGen {
			x.savedGen = gen
		}
		if !x.saveAgain {
			break
		}
	}
	x.saving = false
	x.idle.Broadcast()
	x.mu.Unlock()
}
