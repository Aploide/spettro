// Package fswalk walks a directory tree the way Spettro's search tools and
// symbol index see it: named directories pruned, .gitignore files applied as
// git layers them, symlinked directories not followed. Directories are listed
// by a few goroutines at once, while entries are still handed to the caller
// one at a time, in filepath.WalkDir order.
package fswalk

import (
	"container/heap"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"spettro/internal/ignore"
)

// DefaultWorkers is how many directories a walk lists at once. Measured on
// a 61k-file tree (macOS, APFS): 4 workers took 171 ms against 311 ms
// sequential, and 16 were slower again (215 ms, twice the CPU), as the
// filesystem serialises much of the directory reading.
const DefaultWorkers = 4

// ErrStop ends a walk early without reporting an error.
var ErrStop = errors.New("stop walk")

// Entry is one file a walk visits.
type Entry struct {
	Abs string      // absolute path
	Rel string      // slash path relative to Walker.Base
	D   fs.DirEntry // as listed
	// Info is the file's Lstat, when Walker.Stat is set (taken by the
	// listing goroutines, so the stats run in parallel too).
	Info fs.FileInfo
}

// Walker describes a walk. The zero value walks everything below the root
// with no pruning; set Ignores to apply .gitignore files.
type Walker struct {
	// Base is what Entry.Rel is relative to (the workspace root).
	Base string
	// SkipDir prunes directories by name below the walk's root; nil prunes
	// nothing.
	SkipDir func(name string) bool
	// Ignores applies .gitignore files; nil applies none.
	Ignores *IgnoreSet
	// SymlinkedFiles also visits symlinks to regular files. Symlinked
	// directories are never followed.
	SymlinkedFiles bool
	// FileName, when set, visits only the files whose name it accepts. It
	// is applied while listing, before any stat or .gitignore check, so a
	// walk that wants few of the files (the symbol index wants source
	// files) does not pay for the rest.
	FileName func(name string) bool
	// Stat fills Entry.Info.
	Stat bool
	// Workers lists that many directories at once (DefaultWorkers if 0).
	Workers int
}

// level is one .gitignore file: its directory, in slash form, and its rules.
type level struct {
	dir     string
	matcher *ignore.Matcher
}

// relTo returns full (a slash path below the level's directory) relative to
// that directory, by slicing: no allocation.
func (l level) relTo(full string) string {
	if strings.HasSuffix(l.dir, "/") { // "/" or a Windows drive root "C:/"
		return full[len(l.dir):]
	}
	return full[len(l.dir)+1:]
}

// ignoredByChain applies a directory's .gitignore chain (outermost first) the
// way git and ripgrep (with --no-require-git) layer the files: the deepest
// file with a matching rule decides. full is the entry's slash path.
func ignoredByChain(chain []level, full string, isDir bool) bool {
	for i := len(chain) - 1; i >= 0; i-- {
		if ig, ok := chain[i].matcher.Match(chain[i].relTo(full), isDir); ok {
			return ig
		}
	}
	return false
}

// slashPath is abs with forward slashes, the form .gitignore rules match.
// On Unix it is abs itself (no allocation).
func slashPath(abs string) string { return filepath.ToSlash(abs) }

// IgnoreSet caches the .gitignore chains of directories: the file in every
// directory from the set's top (the filesystem root unless the set was made
// by NewIgnoreSetFrom) down to the directory, outermost first. Walks use it
// for the directories above their root (their own listings supply the
// rest); Ignored uses it for arbitrary paths.
//
// Cache: key is the directory's absolute path; the value is its chain. It
// is never invalidated, so a set should live for one operation (one tool
// call, one index sync): a .gitignore edited meanwhile applies from the next
// set. Any goroutine may use it; mu guards the map.
type IgnoreSet struct {
	top    string // "" for the filesystem root
	mu     sync.Mutex
	chains map[string][]level
}

// NewIgnoreSet returns an empty set whose chains start at the filesystem
// root, as git and ripgrep (with --no-require-git) layer .gitignore files
// for the search tools.
func NewIgnoreSet() *IgnoreSet {
	return &IgnoreSet{chains: map[string][]level{}}
}

// NewIgnoreSetFrom returns an empty set whose chains start at top: the
// .gitignore files of top's ancestors do not apply. The symbol index uses
// it so that, for example, a dotfiles ~/.gitignore holding "*" does not
// empty the index of every project below the home directory.
func NewIgnoreSetFrom(top string) *IgnoreSet {
	return &IgnoreSet{top: filepath.Clean(top), chains: map[string][]level{}}
}

func (s *IgnoreSet) chain(dir string) []level {
	if s.top != "" && !inDir(s.top, dir) {
		return nil // above the top: no file applies
	}
	s.mu.Lock()
	c, ok := s.chains[dir]
	s.mu.Unlock()
	if ok {
		return c
	}
	if parent := filepath.Dir(dir); parent != dir && dir != s.top {
		c = slices.Clip(s.chain(parent))
	}
	if m := ignore.Load(filepath.Join(dir, ".gitignore")); m != nil {
		c = append(c, level{dir: slashPath(dir), matcher: m})
	}
	s.mu.Lock()
	s.chains[dir] = c
	s.mu.Unlock()
	return c
}

// inDir reports whether path is dir or below it (both clean, absolute).
func inDir(dir, path string) bool {
	if !strings.HasPrefix(path, dir) {
		return false
	}
	return len(path) == len(dir) || os.IsPathSeparator(path[len(dir)]) || os.IsPathSeparator(dir[len(dir)-1])
}

// Ignored reports whether the entry at abs is excluded by the .gitignore
// files of its ancestors. It does not look at whether a parent directory is
// itself ignored; walks prune those, and Walker.IgnoredBelow checks them.
func (s *IgnoreSet) Ignored(abs string, isDir bool) bool {
	if s == nil {
		return false
	}
	return ignoredByChain(s.chain(filepath.Dir(abs)), slashPath(abs), isDir)
}

// IgnoredBelow reports whether rel (a file, relative to Base) is excluded by
// .gitignore rules, checking the file and each directory strictly below
// rootRel: the same entries a walk of rootRel would have pruned.
func (w Walker) IgnoredBelow(rootRel, rel string) bool {
	abs := func(p string) string { return filepath.Join(w.Base, filepath.FromSlash(p)) }
	if w.Ignores.Ignored(abs(rel), false) {
		return true
	}
	for dir := parentDir(rel); dir != "." && dir != "/" && dir != rootRel; dir = parentDir(dir) {
		if rootRel != "." && !strings.HasPrefix(dir, rootRel+"/") {
			break
		}
		if w.Ignores.Ignored(abs(dir), true) {
			return true
		}
	}
	return false
}

// parentDir is path.Dir for the slash paths a walk produces.
func parentDir(rel string) string {
	i := strings.LastIndexByte(rel, '/')
	switch {
	case i < 0:
		return "."
	case i == 0:
		return "/"
	}
	return rel[:i]
}

// Reachable reports whether a walk of root would reach start: every
// directory from below root down to start exists under exactly that name,
// is a real directory (not a symlink) and is neither pruned by SkipDir nor
// ignored. root itself is never filtered, as in a walk.
//
// The exact-name check matters on case-insensitive filesystems (the macOS
// and Windows defaults): Lstat("sub/SECRET") succeeds for a directory
// listed as "sub/secret", but a walk from there would report its files as
// "sub/SECRET/..." and case-sensitive .gitignore rules or SkipDir names
// ("secret/", "node_modules") would no longer see them.
func (w Walker) Reachable(root, start string) bool {
	if start == root {
		return true
	}
	rel, err := filepath.Rel(root, start)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	dir := root
	for _, name := range strings.Split(rel, string(filepath.Separator)) {
		parent := dir
		dir = filepath.Join(dir, name)
		info, err := os.Lstat(dir)
		if err != nil || !info.IsDir() || w.skip(name) || !hasExactName(parent, name) || w.Ignores.Ignored(dir, true) {
			return false
		}
	}
	return true
}

// hasExactName reports whether dir lists an entry spelled exactly name.
// It reads names only (no per-entry stat); a prefixed glob on a 61k-file
// tree reads four directories this way.
func hasExactName(dir, name string) bool {
	f, err := os.Open(dir)
	if err != nil {
		return false
	}
	defer f.Close()
	for {
		names, err := f.Readdirnames(1024)
		if slices.Contains(names, name) {
			return true
		}
		if err != nil {
			return false
		}
	}
}

func (w Walker) skip(name string) bool { return w.SkipDir != nil && w.SkipDir(name) }

// entry is one listed entry that survived the filters.
type entry struct {
	Entry
	sub *listing // set for a directory to descend into
}

// listing is a directory's filtered, name-sorted entries, filled in by a
// walk worker or by the consumer itself; done is closed once they are
// ready. claimed (guarded by run.mu) is set by whoever lists it, so it is
// listed exactly once.
type listing struct {
	abs, rel string
	chain    []level // the .gitignore files that apply inside
	done     chan struct{}
	entries  []entry
	claimed  bool
}

// maxAhead bounds how many listed entries may wait for the consumer. The
// workers list ahead of it (that is what makes the walk parallel), but a
// consumer slower than the listing (grep reading files) used to let them
// list the whole tree ahead. Measured on the 60k-file perf corpus with Stat
// and a consumer spending 20 us per file: 40 MB live at peak unbounded,
// 3.5 MB with 4096, and the same 1.24 s either way; a walk with an idle
// consumer is unchanged too (BenchmarkWalk, ~0.3 s). A variable so tests
// can shrink it.
var maxAhead = 4096

// run is the state of one walk: the directories waiting to be listed and
// the workers listing them.
//
// Ordering guarantee: workers list directories concurrently and in any
// order, but Walk hands entries to visit from a single goroutine, in the
// order filepath.WalkDir would (lexical within a directory, depth first),
// by waiting on each listing's done channel as it reaches it. Results that
// depend on order (a search's first max_results matches) are therefore
// identical to a sequential walk's.
//
// Prefetch order: the queue is a heap ordered by walk order
// (CompareWalkOrder on the directories' paths), so the workers always list
// the directory the consumer will reach first. A LIFO queue did nearly as
// well without a budget, but with one it let a worker descend into a later
// sibling's subtree and fill the budget with entries needed much later,
// while the consumer listed the directories it needed itself (the 20 us per
// file consumer of maxAhead's measurement took 1.43 s instead of 1.24 s).
//
// Memory: a worker only starts a listing while fewer than maxAhead listed
// entries wait for the consumer, and the consumer drops each entry once it
// has handed it over, so a walk holds about maxAhead entries plus the
// listings on the current path, whatever the tree's size. The consumer
// never waits on a listing nobody is working on: when it reaches one no
// worker has claimed, it lists it itself, so the budget cannot deadlock the
// walk.
type run struct {
	w    *Walker
	stop chan struct{} // closed when the consumer is done; workers exit

	mu      sync.Mutex
	cond    *sync.Cond
	queue   listingHeap
	ahead   int // entries listed but not yet taken by the consumer
	peak    int // the most ahead ever was, for tests
	stopped bool
}

// listingHeap is a min-heap of listings by walk order (container/heap).
type listingHeap []*listing

func (h listingHeap) Len() int           { return len(h) }
func (h listingHeap) Less(i, j int) bool { return CompareWalkOrder(h[i].rel, h[j].rel) < 0 }
func (h listingHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *listingHeap) Push(x any)        { *h = append(*h, x.(*listing)) }
func (h *listingHeap) Pop() any {
	old := *h
	l := old[len(old)-1]
	old[len(old)-1] = nil
	*h = old[:len(old)-1]
	return l
}

// Walk calls visit for every regular file below root (and, with
// SymlinkedFiles, every symlink to one). It stops early when ctx is done
// (the ctx error is returned) or when visit returns ErrStop. visit runs on
// the calling goroutine, in filepath.WalkDir order. A root that is a file is
// the walk's one entry; an inaccessible root is an empty walk.
func (w Walker) Walk(ctx context.Context, root string, visit func(Entry) error) error {
	_, err := w.walk(ctx, root, visit)
	return err
}

// walk is Walk; it also returns the walk's state (nil for a root that is
// not a directory), for tests.
func (w Walker) walk(ctx context.Context, root string, visit func(Entry) error) (*run, error) {
	info, err := os.Lstat(root)
	if err != nil {
		return nil, nil
	}
	rootRel := "."
	if rel, err := filepath.Rel(w.Base, root); err == nil {
		rootRel = filepath.ToSlash(rel)
	}
	if !info.IsDir() {
		return nil, w.visitRootFile(root, rootRel, info, visit)
	}
	r := &run{w: &w, stop: make(chan struct{})}
	r.cond = sync.NewCond(&r.mu)
	var chain []level
	if w.Ignores != nil {
		chain = w.Ignores.chain(filepath.Dir(root))
	}
	top := &listing{abs: root, rel: rootRel, chain: chain, done: make(chan struct{})}
	r.push(top)
	workers := w.Workers
	if workers <= 0 {
		workers = DefaultWorkers
	}
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.work()
		}()
	}
	err = r.consume(ctx, top, visit)
	r.shutdown()
	wg.Wait()
	if errors.Is(err, ErrStop) {
		return r, nil
	}
	return r, err
}

// visitRootFile handles a walk whose root is a file, as filepath.WalkDir
// does: the root itself is the one entry.
func (w Walker) visitRootFile(root, rel string, info fs.FileInfo, visit func(Entry) error) error {
	d := fs.FileInfoToDirEntry(info)
	if w.FileName != nil && !w.FileName(d.Name()) {
		return nil
	}
	target, ok := w.keepFile(root, d)
	if !ok || w.Ignores.Ignored(root, false) {
		return nil
	}
	e := Entry{Abs: root, Rel: rel, D: d}
	if w.Stat {
		e.Info = info
		if target != nil {
			e.Info = target
		}
	}
	if err := visit(e); err != nil && !errors.Is(err, ErrStop) {
		return err
	}
	return nil
}

// keepFile applies the file-type rules: regular files, and symlinks to
// regular files when SymlinkedFiles is set. For a kept symlink it returns
// the target's info (nil for a regular file).
func (w Walker) keepFile(abs string, d fs.DirEntry) (target fs.FileInfo, ok bool) {
	if d.Type()&fs.ModeSymlink != 0 {
		if !w.SymlinkedFiles {
			return nil, false
		}
		info, err := os.Stat(abs)
		if err != nil || !info.Mode().IsRegular() { // dangling, or a directory
			return nil, false
		}
		return info, true
	}
	return nil, d.Type().IsRegular()
}

func (r *run) push(l *listing) {
	r.mu.Lock()
	heap.Push(&r.queue, l)
	r.mu.Unlock()
	r.cond.Signal()
}

// shutdown stops the workers; listings nobody will read are abandoned.
func (r *run) shutdown() {
	r.mu.Lock()
	r.stopped = true
	r.mu.Unlock()
	close(r.stop)
	r.cond.Broadcast()
}

// work lists queued directories until the walk shuts down, pausing while
// maxAhead entries wait for the consumer.
func (r *run) work() {
	for {
		r.mu.Lock()
		for (len(r.queue) == 0 || r.ahead >= maxAhead) && !r.stopped {
			r.cond.Wait()
		}
		if r.stopped {
			r.mu.Unlock()
			return
		}
		l := heap.Pop(&r.queue).(*listing)
		mine := !l.claimed
		l.claimed = true
		r.mu.Unlock()
		if mine { // else the consumer got to it first
			r.list(l)
		}
	}
}

// list reads one directory, applies SkipDir, .gitignore and the file-type
// rules, queues the surviving subdirectories and publishes the entries. A
// read error keeps whatever entries were read (as WalkDir does).
func (r *run) list(l *listing) {
	defer close(l.done)
	w := r.w
	des, _ := os.ReadDir(l.abs)
	chain := l.chain
	if w.Ignores != nil {
		for _, de := range des {
			if de.Name() == ".gitignore" {
				if m := ignore.Load(filepath.Join(l.abs, ".gitignore")); m != nil {
					chain = append(slices.Clip(chain), level{dir: slashPath(l.abs), matcher: m})
				}
				break
			}
		}
	}
	entries := make([]entry, 0, len(des))
	for _, de := range des {
		select {
		case <-r.stop:
			return
		default:
		}
		name := de.Name()
		abs := filepath.Join(l.abs, name)
		rel := name
		if l.rel != "." {
			rel = l.rel + "/" + name
		}
		if de.IsDir() {
			if w.skip(name) || ignoredByChain(chain, slashPath(abs), true) {
				continue
			}
			sub := &listing{abs: abs, rel: rel, chain: chain, done: make(chan struct{})}
			entries = append(entries, entry{Entry: Entry{Abs: abs, Rel: rel, D: de}, sub: sub})
			continue
		}
		if w.FileName != nil && !w.FileName(name) {
			continue
		}
		target, ok := w.keepFile(abs, de)
		if !ok || ignoredByChain(chain, slashPath(abs), false) {
			continue
		}
		e := entry{Entry: Entry{Abs: abs, Rel: rel, D: de}}
		if w.Stat {
			e.Info = target // a symlink's target
			if e.Info == nil {
				info, err := de.Info()
				if err != nil {
					continue // vanished since the listing
				}
				e.Info = info
			}
		}
		entries = append(entries, e)
	}
	r.mu.Lock()
	l.entries = entries
	r.ahead += len(entries)
	r.peak = max(r.peak, r.ahead)
	for _, e := range entries {
		if e.sub != nil {
			heap.Push(&r.queue, e.sub)
		}
	}
	r.mu.Unlock()
	r.cond.Broadcast()
}

// take waits until l is listed, listing it on the calling goroutine when no
// worker has claimed it, and moves its entries out of the read-ahead
// budget. ok is false when ctx ended first.
func (r *run) take(ctx context.Context, l *listing) (ok bool) {
	r.mu.Lock()
	mine := !l.claimed
	l.claimed = true
	r.mu.Unlock()
	if mine {
		r.list(l)
	} else {
		select {
		case <-l.done:
		case <-ctx.Done():
			return false
		}
	}
	r.mu.Lock()
	wasFull := r.ahead >= maxAhead
	r.ahead -= len(l.entries)
	r.mu.Unlock()
	if wasFull {
		r.cond.Broadcast()
	}
	return true
}

// consume visits the entries of l and, depth first, of its subdirectories.
// Each entry is cleared once handed over, and the listing's slice dropped at
// the end, so nothing the caller has finished with stays reachable.
func (r *run) consume(ctx context.Context, l *listing, visit func(Entry) error) error {
	if !r.take(ctx, l) {
		return ctx.Err()
	}
	defer func() { l.entries = nil }()
	for i := range l.entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		e := l.entries[i]
		l.entries[i] = entry{}
		if e.sub != nil {
			if err := r.consume(ctx, e.sub, visit); err != nil {
				return err
			}
			continue
		}
		if err := visit(e.Entry); err != nil {
			return err
		}
	}
	return nil
}

// CompareWalkOrder orders slash paths the way a walk visits them: by
// segment, each compared as a name ("a/b.go" before "a.go", since the
// directory "a" sorts before the file "a.go"). It does not allocate.
func CompareWalkOrder(a, b string) int {
	for {
		sa, sb := a, b
		ia, ib := strings.IndexByte(a, '/'), strings.IndexByte(b, '/')
		if ia >= 0 {
			sa = a[:ia]
		}
		if ib >= 0 {
			sb = b[:ib]
		}
		if c := strings.Compare(sa, sb); c != 0 {
			return c
		}
		switch {
		case ia < 0 && ib < 0:
			return 0
		case ia < 0:
			return -1
		case ib < 0:
			return 1
		}
		a, b = a[ia+1:], b[ib+1:]
	}
}
