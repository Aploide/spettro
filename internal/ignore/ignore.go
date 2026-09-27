// Package ignore loads and applies .gitignore rules. It is shared by the
// search tools' workspace walker, the TUI file pickers and the repo symbol
// indexer.
//
// Matching follows git (gitignore(5)): a rule without a slash (other than a
// trailing one) matches a name at any depth, a rule with one is anchored to
// the directory holding the .gitignore file, a trailing slash limits the rule
// to directories, "!" re-includes, and the last matching rule wins. Wildcards
// use git's wildmatch with WM_PATHNAME: "*" and "?" never cross a "/", and
// "**" between slashes spans any number of directories. Matching is
// case-sensitive, as git's is with core.ignorecase=false.
//
// Each file is compiled once. The common rule shapes (a literal name, "*.ext",
// a literal anchored path) are answered by map lookups, so Match makes no
// allocations; see ignore_bench_test.go.
package ignore

import (
	"bufio"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// ruleKind selects how a rule is tested; the kinds are ordered from the
// cheapest check to the most general one.
type ruleKind uint8

const (
	// kindBaseLiteral: the name equals the pattern ("node_modules").
	kindBaseLiteral ruleKind = iota
	// kindBaseExt: the name ends in an extension ("*.log" holds ".log").
	kindBaseExt
	// kindBaseSuffix: the name ends in a literal ("*~", "*.tar.gz").
	kindBaseSuffix
	// kindBaseGlob: the name matches a wildcard pattern (".env.*").
	kindBaseGlob
	// kindPathLiteral: the anchored path equals the pattern ("docs/out").
	kindPathLiteral
	// kindPathGlob: the anchored path matches a wildcard pattern
	// ("docs/**/*.tmp"); prefix is the literal text before the first
	// wildcard, checked first.
	kindPathGlob
)

// rule is one compiled .gitignore line.
type rule struct {
	pattern string // what is matched: leading "!" and "/" and trailing "/" removed
	prefix  string // kindPathGlob: literal prefix of pattern
	kind    ruleKind
	negate  bool // "!pattern": a match re-includes
	dirOnly bool // "pattern/": matches directories only
}

// Matcher holds the rules of one .gitignore file. Paths passed to it are
// slash-separated and relative to the directory holding the file. A Matcher
// is immutable once built and safe for concurrent use.
type Matcher struct {
	rules []rule
	// Indexes into rules, ascending, by lookup key: the whole name for
	// kindBaseLiteral, the extension (".log") for kindBaseExt, the whole
	// path for kindPathLiteral. other lists every remaining rule.
	baseLiteral map[string][]int
	baseExt     map[string][]int
	pathLiteral map[string][]int
	other       []int
}

// NewMatcher loads the .gitignore file in root. The result is never nil; with
// no file it ignores nothing.
func NewMatcher(root string) *Matcher {
	if m := Load(filepath.Join(root, ".gitignore")); m != nil {
		return m
	}
	return &Matcher{}
}

// Load reads the rules of one .gitignore file. It returns nil when the file
// does not exist or holds no rules. Paths passed to the returned matcher are
// relative to the directory holding the file.
func Load(path string) *Matcher {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	return Parse(f)
}

// Parse compiles .gitignore rules read from rd. It returns nil when rd holds
// no rules. A read error keeps the rules read before it (best effort, as a
// half-readable file is better applied than dropped).
func Parse(rd io.Reader) *Matcher {
	m := &Matcher{}
	sc := bufio.NewScanner(rd)
	first := true
	for sc.Scan() {
		line := sc.Text()
		if first {
			line = strings.TrimPrefix(line, "\xef\xbb\xbf") // git skips a UTF-8 BOM
			first = false
		}
		if r, ok := parseRule(line); ok {
			m.add(r)
		}
	}
	if len(m.rules) == 0 {
		return nil
	}
	return m
}

// add appends r and indexes it by kind.
func (m *Matcher) add(r rule) {
	idx := len(m.rules)
	m.rules = append(m.rules, r)
	index := func(mp *map[string][]int, key string) {
		if *mp == nil {
			*mp = map[string][]int{}
		}
		(*mp)[key] = append((*mp)[key], idx)
	}
	switch r.kind {
	case kindBaseLiteral:
		index(&m.baseLiteral, r.pattern)
	case kindBaseExt:
		index(&m.baseExt, r.pattern[1:]) // "*.log" -> ".log"
	case kindPathLiteral:
		index(&m.pathLiteral, r.pattern)
	default:
		m.other = append(m.other, idx)
	}
}

// parseRule compiles one .gitignore line; ok is false for blank lines and
// comments.
func parseRule(line string) (rule, bool) {
	line = trimTrailingSpaces(strings.TrimSuffix(line, "\r"))
	if line == "" || line[0] == '#' {
		return rule{}, false
	}
	var r rule
	if line[0] == '!' {
		r.negate = true
		line = line[1:]
	}
	if strings.HasSuffix(line, "/") && !strings.HasSuffix(line, `\/`) {
		r.dirOnly = true
		line = strings.TrimRight(line, "/")
	}
	if line == "" {
		return rule{}, false
	}
	// A slash anywhere but at the end anchors the rule to the .gitignore's
	// directory; without one it matches a name at any depth.
	anchored := strings.Contains(line, "/")
	line = strings.TrimPrefix(line, "/")
	if line == "" {
		return rule{}, false
	}
	r.pattern = line
	literal := !hasWildcard(line)
	switch {
	case anchored && literal:
		r.kind = kindPathLiteral
	case anchored:
		r.kind = kindPathGlob
		// The prefix ends at a slash, so what Wildmatch sees still starts
		// a path segment (a "**" there keeps its any-directories meaning).
		lit := line[:strings.IndexAny(line, `*?[\`)]
		r.prefix = lit[:strings.LastIndexByte(lit, '/')+1]
	case literal:
		r.kind = kindBaseLiteral
	case line[0] == '*' && !hasWildcard(line[1:]):
		r.kind = kindBaseSuffix
		if strings.LastIndexByte(line, '.') == 1 {
			r.kind = kindBaseExt // "*.ext": one dot, right after the star
		}
	default:
		r.kind = kindBaseGlob
	}
	return r, true
}

// trimTrailingSpaces drops trailing spaces unless the last one is escaped
// with a backslash, as git does.
func trimTrailingSpaces(s string) string {
	end := len(s)
	for end > 0 && s[end-1] == ' ' {
		if end >= 2 && s[end-2] == '\\' {
			break
		}
		end--
	}
	return s[:end]
}

// hasWildcard reports whether s uses any wildmatch syntax (a backslash
// escape counts: it has to be interpreted).
func hasWildcard(s string) bool { return strings.ContainsAny(s, `*?[\`) }

// Ignored reports whether the given relative path (using forward slashes)
// should be ignored. isDir should be true when the path refers to a directory.
func (m *Matcher) Ignored(relPath string, isDir bool) bool {
	ignored, _ := m.Match(relPath, isDir)
	return ignored
}

// Match is Ignored that also reports whether any rule matched at all, so a
// caller layering several .gitignore files (a nested one overriding its
// parent's) can tell "not ignored" from "no opinion". relPath must be
// slash-separated and relative to the .gitignore's directory. It does not
// allocate.
func (m *Matcher) Match(relPath string, isDir bool) (ignored, matched bool) {
	if m == nil || len(m.rules) == 0 {
		return false, false
	}
	base := relPath[strings.LastIndexByte(relPath, '/')+1:]
	best := -1
	best = m.lastIndexed(m.baseLiteral, base, isDir, best)
	if dot := strings.LastIndexByte(base, '.'); dot >= 0 {
		best = m.lastIndexed(m.baseExt, base[dot:], isDir, best)
	}
	best = m.lastIndexed(m.pathLiteral, relPath, isDir, best)
	// The remaining rules are checked last to first, and only those that
	// come after the best indexed match can change the outcome.
	for i := len(m.other) - 1; i >= 0; i-- {
		idx := m.other[i]
		if idx <= best {
			break
		}
		r := &m.rules[idx]
		if r.dirOnly && !isDir {
			continue
		}
		if r.matches(relPath, base) {
			best = idx
			break
		}
	}
	if best < 0 {
		return false, false
	}
	return !m.rules[best].negate, true
}

// lastIndexed returns the larger of best and the index of the last rule
// under key in index that applies to the entry (a directory-only rule does
// not apply to a file).
func (m *Matcher) lastIndexed(index map[string][]int, key string, isDir bool, best int) int {
	idxs, ok := index[key]
	if !ok {
		return best
	}
	for i := len(idxs) - 1; i >= 0 && idxs[i] > best; i-- {
		if isDir || !m.rules[idxs[i]].dirOnly {
			return idxs[i]
		}
	}
	return best
}

// matches tests a rule that is not answered by an index.
func (r *rule) matches(relPath, base string) bool {
	switch r.kind {
	case kindBaseSuffix:
		return strings.HasSuffix(base, r.pattern[1:])
	case kindBaseGlob:
		return Wildmatch(r.pattern, base)
	case kindPathGlob:
		return strings.HasPrefix(relPath, r.prefix) && Wildmatch(r.pattern[len(r.prefix):], relPath[len(r.prefix):])
	case kindBaseLiteral:
		return base == r.pattern
	case kindBaseExt:
		return strings.HasSuffix(base, r.pattern[1:])
	default: // kindPathLiteral
		return relPath == r.pattern
	}
}

// Rules returns the number of rules in m, for callers that report on it.
func (m *Matcher) Rules() int {
	if m == nil {
		return 0
	}
	return len(m.rules)
}

// String renders the rules back in .gitignore syntax, one per line, for
// debugging.
func (m *Matcher) String() string {
	var b bytes.Buffer
	for _, r := range m.rules {
		if r.negate {
			b.WriteByte('!')
		}
		if r.kind == kindPathLiteral || r.kind == kindPathGlob {
			b.WriteByte('/')
		}
		b.WriteString(r.pattern)
		if r.dirOnly {
			b.WriteByte('/')
		}
		b.WriteByte('\n')
	}
	return b.String()
}
