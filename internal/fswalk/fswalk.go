// Package fswalk walks a directory tree the way Spettro's search tools and
// symbol index see it: named directories pruned, .gitignore files applied as
// git layers them, symlinked directories not followed. Directories are listed
// by a few goroutines at once, while entries are still handed to the caller
// one at a time, in filepath.WalkDir order.
package fswalk

import (
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
// directory from the filesystem root down to the directory, outermost first.
// Walks use it for the directories above their root (their own listings
// supply the rest); Ignored uses it for arbitrary paths.
//
// Cache: key is the directory's absolute path; the value is its chain. It
// is never invalidated, so a set should live for one operation (one tool
// call, one index sync): a .gitignore edited meanwhile applies from the next
// set. Any goroutine may use it; mu guards the map.
type IgnoreSet struct {
	mu     sync.Mutex
	chains map[string][]level
}

// NewIgnoreSet returns an empty set.
func NewIgnoreSet() *IgnoreSet {
	return &IgnoreSet{chains: map[string][]level{}}
}

func (s *IgnoreSet) chain(dir string) []level {
	s.mu.Lock()
	c, ok := s.chains[dir]
	s.mu.Unlock()
	if ok {
		return c
	}
	if parent := filepath.Dir(dir); parent != dir {
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
// directory from below root down to start exists, is a real directory (not
// a symlink) and is neither pruned by SkipDir nor ignored. root itself is
// never filtered, as in a walk.
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
		dir = filepath.Join(dir, name)
		info, err := os.Lstat(dir)
		if err != nil || !info.IsDir() || w.skip(name) || w.Ignores.Ignored(dir, true) {
			return false
		}
	}
	return true
}

func (w Walker) skip(name string) bool { return w.SkipDir != nil && w.SkipDir(name) }

// entry is one listed entry that survived the filters.
type entry struct {
	Entry
	sub *listing // set for a directory to descend into
}

// listing is a directory's filtered, name-sorted entries, filled in by a
// walk worker; done is closed once they are ready.
type listing struct {
	abs, rel string
	chain    []level // the .gitignore files that apply inside
	done     chan struct{}
	entries  []entry
}

// run is the state of one walk: a LIFO queue of directories to list and
// the workers listing them.
//
// Ordering guarantee: workers list directories concurrently and in any
// order, but Walk hands entries to visit from a single goroutine, in the
// order filepath.WalkDir would (lexical within a directory, depth first),
// by waiting on each listing's done channel as it reaches it. Results that
// depend on order (a search's first max_results matches) are therefore
// identical to a sequential walk's. The queue is LIFO so the prefetch runs
// roughly in that same depth-first order and the consumer rarely waits.
type run struct {
	w    *Walker
	stop chan struct{} // closed when the consumer is done; workers exit

	mu      sync.Mutex
	cond    *sync.Cond
	queue   []*listing
	stopped bool
}

// Walk calls visit for every regular file below root (and, with
// SymlinkedFiles, every symlink to one). It stops early when ctx is done
// (the ctx error is returned) or when visit returns ErrStop. visit runs on
// the calling goroutine, in filepath.WalkDir order. A root that is a file is
// the walk's one entry; an inaccessible root is an empty walk.
func (w Walker) Walk(ctx context.Context, root string, visit func(Entry) error) error {
	info, err := os.Lstat(root)
	if err != nil {
		return nil
	}
	rootRel := "."
	if rel, err := filepath.Rel(w.Base, root); err == nil {
		rootRel = filepath.ToSlash(rel)
	}
	if !info.IsDir() {
		return w.visitRootFile(root, rootRel, info, visit)
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
		return nil
	}
	return err
}

// visitRootFile handles a walk whose root is a file, as filepath.WalkDir
// does: the root itself is the one entry.
func (w Walker) visitRootFile(root, rel string, info fs.FileInfo, visit func(Entry) error) error {
	d := fs.FileInfoToDirEntry(info)
	if !w.keepFile(root, d) || w.Ignores.Ignored(root, false) {
		return nil
	}
	e := Entry{Abs: root, Rel: rel, D: d}
	if w.Stat {
		e.Info = info
	}
	if err := visit(e); err != nil && !errors.Is(err, ErrStop) {
		return err
	}
	return nil
}

// keepFile applies the file-type rules: regular files, and symlinks to
// regular files when SymlinkedFiles is set.
func (w Walker) keepFile(abs string, d fs.DirEntry) bool {
	if d.Type()&fs.ModeSymlink != 0 {
		if !w.SymlinkedFiles {
			return false
		}
		info, err := os.Stat(abs)
		return err == nil && info.Mode().IsRegular() // not dangling, not a directory
	}
	return d.Type().IsRegular()
}

func (r *run) push(l *listing) {
	r.mu.Lock()
	r.queue = append(r.queue, l)
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

// work lists queued directories until the walk shuts down.
func (r *run) work() {
	for {
		r.mu.Lock()
		for len(r.queue) == 0 && !r.stopped {
			r.cond.Wait()
		}
		if r.stopped {
			r.mu.Unlock()
			return
		}
		l := r.queue[len(r.queue)-1]
		r.queue = r.queue[:len(r.queue)-1]
		r.mu.Unlock()
		r.list(l)
	}
}

// list reads one directory, applies SkipDir, .gitignore and the file-type
// rules, queues the surviving subdirectories and publishes the entries.
// Subdirectories are queued in reverse so the LIFO queue lists them first to
// last. A read error keeps whatever entries were read (as WalkDir does).
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
		if !w.keepFile(abs, de) || ignoredByChain(chain, slashPath(abs), false) {
			continue
		}
		e := entry{Entry: Entry{Abs: abs, Rel: rel, D: de}}
		if w.Stat {
			info, err := de.Info()
			if err != nil {
				continue // vanished since the listing
			}
			e.Info = info
		}
		entries = append(entries, e)
	}
	l.entries = entries
	for i := len(entries) - 1; i >= 0; i-- {
		if entries[i].sub != nil {
			r.push(entries[i].sub)
		}
	}
}

// consume visits the entries of l and, depth first, of its subdirectories,
// waiting for each listing to be ready.
func (r *run) consume(ctx context.Context, l *listing, visit func(Entry) error) error {
	select {
	case <-l.done:
	case <-ctx.Done():
		return ctx.Err()
	}
	for _, e := range l.entries {
		if err := ctx.Err(); err != nil {
			return err
		}
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
