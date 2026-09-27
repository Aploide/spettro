package ignore

// The matcher this package shipped before the compiled one (commit 0f95899),
// kept as the oracle for FuzzMatchAgainstOracle. It is not git-exact (a
// literal-prefix check for "**", suffix matching for plain names), so the
// fuzz test compares only rule shapes both implementations handle the way
// git does; TestMatchAgainstGit covers the rest against git itself.

import (
	"bufio"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

type oraclePattern struct {
	negate  bool
	dirOnly bool
	rooted  bool
	glob    string
}

type oracleMatcher struct{ patterns []oraclePattern }

func oracleParse(src string) *oracleMatcher {
	m := &oracleMatcher{}
	sc := bufio.NewScanner(strings.NewReader(src))
	for sc.Scan() {
		if p, ok := oracleParsePattern(sc.Text()); ok {
			m.patterns = append(m.patterns, p)
		}
	}
	return m
}

func oracleParsePattern(line string) (oraclePattern, bool) {
	line = strings.TrimRight(line, " \t\r")
	if line == "" || strings.HasPrefix(line, "#") {
		return oraclePattern{}, false
	}
	var p oraclePattern
	if strings.HasPrefix(line, "!") {
		p.negate = true
		line = line[1:]
	} else if strings.HasPrefix(line, `\#`) {
		line = line[1:]
	}
	if strings.HasSuffix(line, "/") {
		p.dirOnly = true
		line = strings.TrimSuffix(line, "/")
	}
	if strings.Contains(line, "/") {
		p.rooted = true
		line = strings.TrimPrefix(line, "/")
	}
	p.glob = line
	return p, true
}

func (m *oracleMatcher) match(relPath string, isDir bool) (ignored, matched bool) {
	for _, p := range m.patterns {
		if p.dirOnly && !isDir {
			continue
		}
		if oracleMatchPattern(p, relPath) {
			ignored, matched = !p.negate, true
		}
	}
	return ignored, matched
}

func oracleMatchPattern(p oraclePattern, relPath string) bool {
	if p.rooted {
		if !strings.ContainsAny(p.glob, "*?[\\") {
			return p.glob == relPath
		}
		return oracleMatchGlob(p.glob, relPath)
	}
	if oracleMatchGlob(p.glob, relPath) {
		return true
	}
	if oracleMatchGlob(p.glob, filepath.Base(relPath)) {
		return true
	}
	if strings.Contains(p.glob, "/") || strings.Contains(p.glob, "**") {
		parts := strings.Split(relPath, "/")
		for i := range parts {
			if oracleMatchGlob(p.glob, strings.Join(parts[i:], "/")) {
				return true
			}
		}
	}
	return false
}

func oracleMatchGlob(pattern, path string) bool {
	if !strings.ContainsAny(pattern, "*?[\\") {
		return pattern == path || strings.HasSuffix(path, "/"+pattern)
	}
	if strings.Contains(pattern, "**") {
		return oracleMatchDoublestar(pattern, path)
	}
	matched, err := filepath.Match(pattern, path)
	return err == nil && matched
}

func oracleMatchDoublestar(pattern, path string) bool {
	parts := strings.SplitN(pattern, "**", 2)
	prefix, suffix := parts[0], strings.TrimPrefix(parts[1], "/")
	if prefix != "" {
		if !strings.HasPrefix(path, prefix) {
			return false
		}
		path = strings.TrimPrefix(path[len(prefix):], "/")
	}
	if suffix == "" {
		return true
	}
	segments := strings.Split(path, "/")
	for i := range segments {
		candidate := strings.Join(segments[i:], "/")
		if ok, _ := filepath.Match(suffix, candidate); ok {
			return true
		}
		if strings.Contains(suffix, "**") && oracleMatchDoublestar(suffix, candidate) {
			return true
		}
	}
	return false
}

// oracleComparable accepts the rule shapes where the old matcher already
// behaved like git: names and anchored paths made of plain characters, and
// name globs using only "*" and "?" (no brackets, escapes or "**").
var oracleComparable = regexp.MustCompile(`^!?(/?[A-Za-z0-9._-]+(/[A-Za-z0-9._-]+)*|[A-Za-z0-9._*?-]+)/?$`)

var pathShape = regexp.MustCompile(`^[A-Za-z0-9._-]+(/[A-Za-z0-9._-]+)*$`)

// FuzzMatchAgainstOracle checks the compiled matcher against the previous
// implementation for the rule shapes both handle as git does.
func FuzzMatchAgainstOracle(f *testing.F) {
	seeds := []struct{ rule, path string }{
		{"*.log", "a/b.log"}, {"build/", "x/build"}, {"/root.txt", "root.txt"},
		{"node_modules", "a/node_modules"}, {"docs/out", "docs/out"}, {"a?c", "abc"},
		{"!keep.log", "keep.log"}, {"*.tar.gz", "x.tar.gz"}, {".env.*", ".env.local"},
	}
	for _, s := range seeds {
		f.Add(s.rule, s.path, false)
		f.Add(s.rule, s.path, true)
	}
	f.Fuzz(func(t *testing.T, ruleLine, path string, isDir bool) {
		if !oracleComparable.MatchString(ruleLine) || strings.Contains(ruleLine, "**") || !pathShape.MatchString(path) {
			t.Skip()
		}
		body := strings.TrimSuffix(strings.TrimPrefix(ruleLine, "!"), "/")
		if strings.Trim(body, "/") == "" || strings.Contains(body, "//") {
			t.Skip()
		}
		got := Parse(strings.NewReader(ruleLine))
		want := oracleParse(ruleLine)
		gi, gm := got.Match(path, isDir)
		wi, wm := want.match(path, isDir)
		if gi != wi || gm != wm {
			t.Fatalf("rule %q path %q dir=%v: compiled (%v,%v), oracle (%v,%v)", ruleLine, path, isDir, gi, gm, wi, wm)
		}
	})
}
