package agent

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"

	"spettro/internal/ignore"
)

// skipDirs are directories to skip when walking the workspace. They are only
// skipped below the search root: a search pointed explicitly at one of them
// still runs.
var skipDirs = map[string]bool{
	".git":         true,
	".spettro":     true,
	"vendor":       true,
	"node_modules": true,
	"dist":         true,
	"build":        true,
}

// walkReadDirWorkers is how many directories a walk lists at once. Measured
// on a 61k-file tree (perf harness TestPerfWalkInterleaved): 4 workers took
// 171 ms against 311 ms sequential, and 16 were slower again (215 ms, twice
// the CPU), as APFS serialises much of the directory reading.
const walkReadDirWorkers = 4

// workspaceWalker walks a directory tree below the workspace the way the
// search tools see it: skipDirs and .gitignore files prune entries, and
// symlinked directories are not followed.
type workspaceWalker struct {
	cwd     string
	ignores *gitignoreSet
	// symlinkedFiles makes the walk visit symlinks to regular files (glob:
	// a CLAUDE.md -> AGENTS.md link is a file the model should see listed).
	// grep leaves it off, matching ripgrep, which skips every symlink it
	// meets while traversing; both grep backends then agree.
	symlinkedFiles bool
}

func (r *toolRuntime) newWorkspaceWalker() workspaceWalker {
	return workspaceWalker{cwd: r.cwd, ignores: newGitignoreSet()}
}

// gitignoreLevel is one .gitignore file: the directory holding it, in
// slash form (see slashPath), and its rules.
type gitignoreLevel struct {
	dir     string
	matcher *ignore.Matcher
}

// relTo returns full (a slash path below the level's directory) relative to
// that directory, by slicing: no allocation.
func (l gitignoreLevel) relTo(full string) string {
	if strings.HasSuffix(l.dir, "/") { // "/" or a Windows drive root "C:/"
		return full[len(l.dir):]
	}
	return full[len(l.dir)+1:]
}

// ignoredByChain applies a directory's .gitignore chain (outermost first) the
// way git and ripgrep (run with --no-require-git) layer the files: the
// deepest file with a matching rule decides. full is the entry's slash path.
func ignoredByChain(chain []gitignoreLevel, full string, isDir bool) bool {
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

// gitignoreSet caches the .gitignore chains of directories: the file in
// every directory from the filesystem root down to the directory, outermost
// first. Walks use it for the directories above their root (their own
// listing supplies the rest); ignoredBelow uses it for arbitrary paths.
//
// Cache: key is the directory's absolute path; the value is its chain. It
// is never invalidated: a set lives for one tool call (newWorkspaceWalker),
// so a .gitignore edited during a search applies from the next call. Any
// goroutine may use it; mu guards the map.
type gitignoreSet struct {
	mu     sync.Mutex
	chains map[string][]gitignoreLevel
}

func newGitignoreSet() *gitignoreSet {
	return &gitignoreSet{chains: map[string][]gitignoreLevel{}}
}

func (s *gitignoreSet) chain(dir string) []gitignoreLevel {
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
		c = append(c, gitignoreLevel{dir: slashPath(dir), matcher: m})
	}
	s.mu.Lock()
	s.chains[dir] = c
	s.mu.Unlock()
	return c
}

// ignored reports whether the entry at abs is excluded by the .gitignore
// files of its ancestors. It does not look at whether a parent directory is
// itself ignored; walks prune those, and ignoredBelow checks them.
func (s *gitignoreSet) ignored(abs string, isDir bool) bool {
	return ignoredByChain(s.chain(filepath.Dir(abs)), slashPath(abs), isDir)
}

// ignoredBelow reports whether rel (a file, relative to the workspace) is
// excluded by .gitignore rules, checking the file and each directory strictly
// below rootRel — the same entries the walk would have pruned.
func (w workspaceWalker) ignoredBelow(rootRel, rel string) bool {
	abs := func(p string) string { return filepath.Join(w.cwd, filepath.FromSlash(p)) }
	if w.ignores.ignored(abs(rel), false) {
		return true
	}
	for dir := parentDir(rel); dir != "." && dir != "/" && dir != rootRel; dir = parentDir(dir) {
		if rootRel != "." && !strings.HasPrefix(dir, rootRel+"/") {
			break
		}
		if w.ignores.ignored(abs(dir), true) {
			return true
		}
	}
	return false
}

// parentDir is path.Dir for the slash paths the walker produces.
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

// errStopWalk ends a workspace walk early without reporting an error.
var errStopWalk = errors.New("stop walk")

// walkEntry is one entry of a listed directory that survived the filters.
type walkEntry struct {
	abs, rel string
	d        fs.DirEntry
	sub      *walkListing // set for a directory to descend into
}

// walkListing is a directory's filtered, name-sorted entries, filled in by
// a walk worker; done is closed once they are ready.
type walkListing struct {
	abs, rel string
	chain    []gitignoreLevel // the .gitignore files that apply inside
	done     chan struct{}
	entries  []walkEntry
}

// walkRun is the state of one walk: a LIFO queue of directories to list and
// the workers listing them.
//
// Ordering guarantee: workers list directories concurrently and in any
// order, but walk hands entries to visit from a single goroutine, in the
// order filepath.WalkDir would (lexical within a directory, depth first),
// by waiting on each listing's done channel as it reaches it. Results that
// depend on order (the first max_results matches) are therefore identical
// to a sequential walk. The queue is LIFO so the prefetch runs roughly in
// that same depth-first order and the consumer rarely waits.
type walkRun struct {
	w    *workspaceWalker
	stop chan struct{} // closed when the consumer is done; workers exit

	mu      sync.Mutex
	cond    *sync.Cond
	queue   []*walkListing
	stopped bool
}

// walk calls visit for every regular file below root (and, with
// symlinkedFiles, every symlink to one) with its path relative to the
// workspace (slash-separated). It stops early when ctx is done (the ctx
// error is returned) or when visit returns errStopWalk. visit runs on the
// calling goroutine, in filepath.WalkDir order; the directory listing
// behind it is done by walkReadDirWorkers goroutines.
func (w workspaceWalker) walk(ctx context.Context, root string, visit func(abs, rel string, d fs.DirEntry) error) error {
	info, err := os.Lstat(root)
	if err != nil {
		return nil // an inaccessible root is an empty walk
	}
	rootRel := "."
	if rel, err := filepath.Rel(w.cwd, root); err == nil {
		rootRel = filepath.ToSlash(rel)
	}
	if !info.IsDir() {
		return w.visitRootFile(root, rootRel, info, visit)
	}
	run := &walkRun{w: &w, stop: make(chan struct{})}
	run.cond = sync.NewCond(&run.mu)
	top := &walkListing{abs: root, rel: rootRel, chain: w.ignores.chain(filepath.Dir(root)), done: make(chan struct{})}
	run.push(top)
	var workers sync.WaitGroup
	for range walkReadDirWorkers {
		workers.Add(1)
		go func() {
			defer workers.Done()
			run.work()
		}()
	}
	err = run.consume(ctx, top, visit)
	run.shutdown()
	workers.Wait()
	if errors.Is(err, errStopWalk) {
		return nil
	}
	return err
}

// visitRootFile handles a walk whose root is a file, as filepath.WalkDir
// does: the root itself is the one entry.
func (w workspaceWalker) visitRootFile(root, rel string, info fs.FileInfo, visit func(abs, rel string, d fs.DirEntry) error) error {
	d := fs.FileInfoToDirEntry(info)
	if !w.keepFile(root, d) || w.ignores.ignored(root, false) {
		return nil
	}
	if err := visit(root, rel, d); err != nil && !errors.Is(err, errStopWalk) {
		return err
	}
	return nil
}

// keepFile applies the file-type rules: regular files, and symlinks to
// regular files when symlinkedFiles is set.
func (w workspaceWalker) keepFile(abs string, d fs.DirEntry) bool {
	if d.Type()&fs.ModeSymlink != 0 {
		if !w.symlinkedFiles {
			return false
		}
		info, err := os.Stat(abs)
		return err == nil && info.Mode().IsRegular() // not dangling, not a directory
	}
	return d.Type().IsRegular()
}

func (run *walkRun) push(l *walkListing) {
	run.mu.Lock()
	run.queue = append(run.queue, l)
	run.mu.Unlock()
	run.cond.Signal()
}

// shutdown stops the workers; listings nobody will read are abandoned.
func (run *walkRun) shutdown() {
	run.mu.Lock()
	run.stopped = true
	run.mu.Unlock()
	close(run.stop)
	run.cond.Broadcast()
}

// work lists queued directories until the walk shuts down.
func (run *walkRun) work() {
	for {
		run.mu.Lock()
		for len(run.queue) == 0 && !run.stopped {
			run.cond.Wait()
		}
		if run.stopped {
			run.mu.Unlock()
			return
		}
		l := run.queue[len(run.queue)-1]
		run.queue = run.queue[:len(run.queue)-1]
		run.mu.Unlock()
		run.list(l)
	}
}

// list reads one directory, applies skipDirs, .gitignore and the file-type
// rules, queues the surviving subdirectories and publishes the entries.
// Subdirectories are queued in reverse so the LIFO queue lists them first to
// last. A read error keeps whatever entries were read (as WalkDir does).
func (run *walkRun) list(l *walkListing) {
	defer close(l.done)
	des, _ := os.ReadDir(l.abs)
	chain := l.chain
	for _, de := range des {
		if de.Name() == ".gitignore" {
			if m := ignore.Load(filepath.Join(l.abs, ".gitignore")); m != nil {
				chain = append(slices.Clip(chain), gitignoreLevel{dir: slashPath(l.abs), matcher: m})
			}
			break
		}
	}
	entries := make([]walkEntry, 0, len(des))
	for _, de := range des {
		select {
		case <-run.stop:
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
			if skipDirs[name] || ignoredByChain(chain, slashPath(abs), true) {
				continue
			}
			sub := &walkListing{abs: abs, rel: rel, chain: chain, done: make(chan struct{})}
			entries = append(entries, walkEntry{abs: abs, rel: rel, d: de, sub: sub})
			continue
		}
		if !run.w.keepFile(abs, de) || ignoredByChain(chain, slashPath(abs), false) {
			continue
		}
		entries = append(entries, walkEntry{abs: abs, rel: rel, d: de})
	}
	l.entries = entries
	for i := len(entries) - 1; i >= 0; i-- {
		if entries[i].sub != nil {
			run.push(entries[i].sub)
		}
	}
}

// consume visits the entries of l and, depth first, of its subdirectories,
// waiting for each listing to be ready.
func (run *walkRun) consume(ctx context.Context, l *walkListing, visit func(abs, rel string, d fs.DirEntry) error) error {
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
			if err := run.consume(ctx, e.sub, visit); err != nil {
				return err
			}
			continue
		}
		if err := visit(e.abs, e.rel, e.d); err != nil {
			return err
		}
	}
	return nil
}

// fileWorkers is the parallelism of the per-file work behind a walk (grep's
// file searches). Opening files is the bottleneck, not matching them: on
// the 61k-file tree (macOS, 10 cores) a full Go grep took a median 1.75 s
// with 4 workers against 2.1 s with 2 or 6 and 2.6 s with 10, where the
// kernel spent 40+ s of CPU contending in open(2).
func fileWorkers() int { return min(4, max(2, runtime.NumCPU())) }
