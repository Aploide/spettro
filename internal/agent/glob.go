package agent

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
)

// runGlob implements the glob tool: a workspace walk with ** support. The
// pattern is matched against paths relative to the workspace and, when a
// path argument narrows the search, relative to that directory as well, so
// both {"pattern":"internal/**/*.go"} and {"path":"internal","pattern":"**/*.go"}
// work. Brace alternatives ("*.{ts,tsx}") are expanded.
//
// The walk starts at the patterns' longest literal directory prefix
// ("go126/src/net/http/**/*.go" walks only go126/src/net/http), which is
// what makes a prefixed glob cost milliseconds on a large tree. Listed
// matches are the first maxGlobResults in walk order, as with one full walk.
func (r *toolRuntime) runGlob(ctx context.Context, pattern, subPath string) (string, error) {
	if strings.TrimSpace(pattern) == "" {
		return "", fmt.Errorf("glob: pattern is required")
	}
	root := r.cwd
	if strings.TrimSpace(subPath) != "" {
		abs, _, err := r.resolvePath(subPath)
		if err != nil {
			return "", fmt.Errorf("glob path: %w", err)
		}
		root = abs
	}
	pat := strings.TrimPrefix(strings.TrimSpace(pattern), "./")
	if strings.HasPrefix(pat, "/") || filepath.IsAbs(pat) {
		rel, ok := r.workspaceRelativePattern(pat)
		if !ok {
			return "", fmt.Errorf("glob: pattern %q is outside the working directory; patterns match paths relative to it (e.g. internal/**/*.go)", pattern)
		}
		pat = rel
	}
	patterns := compileGlobs(expandBraces(pat))
	rootRel := "."
	if rel, err := filepath.Rel(r.cwd, root); err == nil {
		rootRel = filepath.ToSlash(rel)
	}

	var matches []string
	walker := r.newWorkspaceWalker()
	walker.symlinkedFiles = true
	visit := func(_, rel string, _ fs.DirEntry) error {
		if globMatchesAny(patterns, rel, rootRel) {
			matches = append(matches, rel)
		}
		return nil
	}
	starts := globWalkStarts(root, rootRel, r.cwd, patterns)
	for _, start := range starts {
		if !walker.reachable(root, start) {
			continue
		}
		if err := walker.walk(ctx, start, visit); err != nil {
			return "", fmt.Errorf("glob walk: %w", err)
		}
	}
	if len(matches) == 0 {
		return fmt.Sprintf("no files match %q", pattern), nil
	}
	total := len(matches)
	if total > maxGlobResults {
		if len(starts) > 1 {
			// Keep the first files in the order one walk of the whole
			// root would have listed them.
			slices.SortFunc(matches, compareWalkOrder)
		}
		matches = matches[:maxGlobResults]
	}
	sort.Strings(matches)
	out := fmt.Sprintf("%d files:\n%s", total, strings.Join(matches, "\n"))
	if total > len(matches) {
		out += fmt.Sprintf("\n(showing the first %d of %d files; narrow the pattern or path to see the rest)", len(matches), total)
	}
	return out, nil
}

// globMatchesAny reports whether rel (relative to the workspace) matches one
// of the patterns, directly or relative to the search root rootRel.
func globMatchesAny(patterns []globPattern, rel, rootRel string) bool {
	relRoot := rel
	if rootRel != "." {
		relRoot = strings.TrimPrefix(rel, rootRel+"/")
	}
	for _, p := range patterns {
		if p.match(rel) || (relRoot != rel && p.match(relRoot)) {
			return true
		}
	}
	return false
}

// workspaceRelativePattern turns an absolute glob pattern that starts with the
// workspace path (as given, or with symlinks resolved) into the equivalent
// relative pattern.
func (r *toolRuntime) workspaceRelativePattern(pat string) (string, bool) {
	pat = filepath.ToSlash(pat)
	bases := []string{r.cwd}
	if resolved, err := filepath.EvalSymlinks(r.cwd); err == nil && resolved != r.cwd {
		bases = append(bases, resolved)
	}
	for _, base := range bases {
		b := strings.TrimSuffix(filepath.ToSlash(base), "/")
		if pat == b {
			return "*", true
		}
		if rest, ok := strings.CutPrefix(pat, b+"/"); ok {
			return rest, true
		}
	}
	return "", false
}

// globPattern is one brace-expanded glob pattern, split into its path
// segments once rather than on every file.
type globPattern struct {
	segs []string
}

func compileGlobs(patterns []string) []globPattern {
	out := make([]globPattern, len(patterns))
	for i, p := range patterns {
		out[i] = globPattern{segs: strings.Split(p, "/")}
	}
	return out
}

// match reports whether the slash path rel matches the whole pattern.
func (g globPattern) match(rel string) bool { return globMatchSegs(g.segs, rel) }

// literalDirs returns the leading segments of the pattern that are plain
// directory names: no wildcard, and not the final (file name) segment.
func (g globPattern) literalDirs() []string {
	n := 0
	for n < len(g.segs)-1 && !strings.ContainsAny(g.segs[n], `*?[\`) && g.segs[n] != "" {
		n++
	}
	return g.segs[:n]
}

// matchGlobPattern matches a slash-separated path against a glob pattern with
// ** support.
func matchGlobPattern(pattern, rel string) bool {
	return globMatchSegs(strings.Split(pattern, "/"), rel)
}

// globMatchSegs matches pattern segments against the slash path rest,
// segment by segment: "**" spans zero or more segments, any other segment
// is a filepath.Match pattern for exactly one. rest == "" means no segments
// are left (walk paths have no empty segments). It does not allocate.
func globMatchSegs(pat []string, rest string) bool {
	if len(pat) == 0 {
		return rest == ""
	}
	if pat[0] == "**" {
		for {
			if globMatchSegs(pat[1:], rest) {
				return true
			}
			if rest == "" {
				return false
			}
			rest = afterFirstSegment(rest)
		}
	}
	if rest == "" {
		return false
	}
	seg := rest
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		seg = rest[:i]
	}
	if ok, err := filepath.Match(pat[0], seg); err != nil || !ok {
		return false
	}
	return globMatchSegs(pat[1:], afterFirstSegment(rest))
}

// afterFirstSegment drops the first segment of a slash path ("" when it was
// the last).
func afterFirstSegment(p string) string {
	if i := strings.IndexByte(p, '/'); i >= 0 {
		return p[i+1:]
	}
	return ""
}

// globWalkStarts returns the directories a glob has to walk: below root, at
// the longest directory prefix every pattern shares. A pattern may be
// relative to root or to the workspace (see runGlob), so each reading gives
// a start, and a start inside another is dropped.
func globWalkStarts(root, rootRel, cwd string, patterns []globPattern) []string {
	if info, err := os.Stat(root); err != nil || !info.IsDir() {
		return []string{root}
	}
	prefix := commonLiteralDirs(patterns)
	if len(prefix) == 0 {
		return []string{root}
	}
	lit := strings.Join(prefix, "/")
	starts := []string{filepath.Join(root, filepath.FromSlash(lit))}
	if rootRel != "." {
		switch {
		case lit == rootRel || strings.HasPrefix(lit, rootRel+"/"):
			starts = append(starts, filepath.Join(cwd, filepath.FromSlash(lit)))
		case strings.HasPrefix(rootRel, lit+"/"):
			starts = append(starts, root)
		}
	}
	return outermostDirs(starts)
}

// commonLiteralDirs is the literal directory prefix shared by all patterns.
func commonLiteralDirs(patterns []globPattern) []string {
	var common []string
	for i, p := range patterns {
		dirs := p.literalDirs()
		if i == 0 {
			common = dirs
			continue
		}
		n := 0
		for n < len(common) && n < len(dirs) && common[n] == dirs[n] {
			n++
		}
		common = common[:n]
	}
	return common
}

// outermostDirs drops duplicates and every directory inside another one.
func outermostDirs(dirs []string) []string {
	var out []string
	for i, d := range dirs {
		keep := true
		for j, o := range dirs {
			switch {
			case i == j:
			case o == d && j < i: // a duplicate: the first one stays
				keep = false
			case o != d && isWithin(o, d):
				keep = false
			}
		}
		if keep {
			out = append(out, d)
		}
	}
	return out
}

// isWithin reports whether path is dir or below it.
func isWithin(dir, path string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// reachable reports whether a walk of root would reach start: every
// directory from below root down to start exists, is a real directory (walks
// do not follow symlinked directories) and is neither a skipDirs name nor
// ignored. root itself is never filtered, as in a walk.
func (w workspaceWalker) reachable(root, start string) bool {
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
		if err != nil || !info.IsDir() || skipDirs[name] || w.ignores.ignored(dir, true) {
			return false
		}
	}
	return true
}

// compareWalkOrder orders slash paths the way a walk visits them: by
// segment, each compared as a name ("a/b.go" before "a.go", since the
// directory "a" sorts before the file "a.go").
func compareWalkOrder(a, b string) int {
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
