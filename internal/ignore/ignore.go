// Package ignore loads and applies .gitignore rules from a repository root.
// It is shared between the TUI file pickers and the repo symbol indexer.
package ignore

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
)

// pattern holds a compiled .gitignore rule.
type pattern struct {
	raw     string
	negate  bool
	dirOnly bool   // trailing slash in pattern → only matches directories
	rooted  bool   // pattern has a slash before the last segment → root-anchored
	glob    string // cleaned pattern used for matching
}

// Matcher loads and applies .gitignore rules from a repository root.
type Matcher struct {
	patterns []pattern
}

// NewMatcher loads the root .gitignore of the given directory.
func NewMatcher(root string) *Matcher {
	m := &Matcher{}
	m.loadFile(filepath.Join(root, ".gitignore"))
	return m
}

func (m *Matcher) loadFile(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if p, ok := parsePattern(sc.Text()); ok {
			m.patterns = append(m.patterns, p)
		}
	}
	_ = sc.Err() // best-effort load; ignore I/O errors mid-scan
}

// parsePattern parses a single .gitignore line into a pattern struct.
func parsePattern(line string) (pattern, bool) {
	line = strings.TrimRight(line, " \t\r")
	if line == "" || strings.HasPrefix(line, "#") {
		return pattern{}, false
	}

	p := pattern{raw: line}

	if strings.HasPrefix(line, "!") {
		p.negate = true
		line = line[1:]
	} else if strings.HasPrefix(line, `\#`) {
		line = line[1:] // escaped hash
	}

	if strings.HasSuffix(line, "/") {
		p.dirOnly = true
		line = strings.TrimSuffix(line, "/")
	}

	// A pattern is rooted if it contains a slash anywhere except at the end
	// (we already stripped the trailing slash above).
	if strings.Contains(line, "/") {
		p.rooted = true
		line = strings.TrimPrefix(line, "/")
	}

	p.glob = line
	return p, true
}

// Ignored reports whether the given relative path (using forward slashes)
// should be ignored. isDir should be true when the path refers to a directory.
func (m *Matcher) Ignored(relPath string, isDir bool) bool {
	ignored := false
	for _, p := range m.patterns {
		if p.dirOnly && !isDir {
			continue
		}
		if m.matchPattern(p, relPath) {
			ignored = !p.negate
		}
	}
	return ignored
}

func (m *Matcher) matchPattern(p pattern, relPath string) bool {
	relPath = filepath.ToSlash(relPath)

	if p.rooted {
		// Match against the full path from root
		return matchGlob(p.glob, relPath)
	}

	// Non-rooted: match against any path component suffix
	// e.g. "*.log" should match "foo/bar.log"
	if matchGlob(p.glob, relPath) {
		return true
	}
	// Also try matching just the base name
	if matchGlob(p.glob, filepath.Base(relPath)) {
		return true
	}
	// Also check every path prefix when pattern contains ** or /
	if strings.Contains(p.glob, "/") || strings.Contains(p.glob, "**") {
		parts := strings.Split(relPath, "/")
		for i := range parts {
			sub := strings.Join(parts[i:], "/")
			if matchGlob(p.glob, sub) {
				return true
			}
		}
	}
	return false
}

// matchGlob matches a gitignore-style glob pattern against a path.
// Supports *, **, ?, and character classes.
// ** matches any number of path segments including none.
func matchGlob(pattern, path string) bool {
	// Fast path: no special chars
	if !strings.ContainsAny(pattern, "*?[\\") {
		return pattern == path || strings.HasSuffix(path, "/"+pattern)
	}

	// Expand ** into a recursive match by trying all possible splits
	if strings.Contains(pattern, "**") {
		return matchDoublestar(pattern, path)
	}

	// Single-level glob via filepath.Match
	matched, err := filepath.Match(pattern, path)
	if err != nil {
		return false
	}
	return matched
}

// matchDoublestar handles ** in patterns by recursively trying all splits.
func matchDoublestar(pattern, path string) bool {
	parts := strings.SplitN(pattern, "**", 2)
	prefix, suffix := parts[0], parts[1]
	suffix = strings.TrimPrefix(suffix, "/")

	// The prefix must match the beginning of the path
	if prefix != "" {
		if !strings.HasPrefix(path, prefix) {
			return false
		}
		path = path[len(prefix):]
		path = strings.TrimPrefix(path, "/")
	}

	if suffix == "" {
		// ** at end matches everything
		return true
	}

	// Try matching the suffix against every suffix of path
	segments := strings.Split(path, "/")
	for i := range segments {
		candidate := strings.Join(segments[i:], "/")
		if ok, _ := filepath.Match(suffix, candidate); ok {
			return true
		}
		if matchDoublestar(suffix, candidate) {
			return true
		}
	}
	return false
}
