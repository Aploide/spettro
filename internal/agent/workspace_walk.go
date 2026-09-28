package agent

import (
	"context"
	"io/fs"
	"runtime"

	"spettro/internal/fswalk"
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

func isSkipDir(name string) bool { return skipDirs[name] }

// workspaceWalker walks a directory tree below the workspace the way the
// search tools see it: skipDirs and .gitignore files prune entries, and
// symlinked directories are not followed (see internal/fswalk).
type workspaceWalker struct {
	fswalk.Walker
}

// newWorkspaceWalker returns a walker for one tool call: its .gitignore
// cache lives as long as the call.
func (r *toolRuntime) newWorkspaceWalker() workspaceWalker {
	return workspaceWalker{fswalk.Walker{Base: r.cwd, SkipDir: isSkipDir, Ignores: fswalk.NewIgnoreSet()}}
}

// walk calls visit for every regular file below root (and, with
// SymlinkedFiles, every symlink to one) with its path relative to the
// workspace, in filepath.WalkDir order, on the calling goroutine. It stops
// early when ctx is done (the ctx error is returned) or when visit returns
// errStopWalk.
func (w workspaceWalker) walk(ctx context.Context, root string, visit func(abs, rel string, d fs.DirEntry) error) error {
	return w.Walk(ctx, root, func(e fswalk.Entry) error { return visit(e.Abs, e.Rel, e.D) })
}

// errStopWalk ends a workspace walk early without reporting an error.
var errStopWalk = fswalk.ErrStop

// compareWalkOrder orders slash paths the way a walk visits them.
func compareWalkOrder(a, b string) int { return fswalk.CompareWalkOrder(a, b) }

// fileWorkers is the parallelism of the per-file work behind a walk (grep's
// file searches). Opening files is the bottleneck, not matching them: on
// the 61k-file tree (macOS, 10 cores) a full Go grep took a median 1.75 s
// with 4 workers against 2.1 s with 2 or 6 and 2.6 s with 10, where the
// kernel spent 40+ s of CPU contending in open(2).
func fileWorkers() int { return min(4, max(2, runtime.NumCPU())) }
