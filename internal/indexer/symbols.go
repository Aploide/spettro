package indexer

import (
	"bytes"
	"path/filepath"
	"regexp"
	"strings"
)

// Symbol is a single definition found in a source file.
type Symbol struct {
	Path      string `json:"path"` // slash-separated, relative to the index root
	Line      int    `json:"line"` // 1-based
	Kind      string `json:"kind"` // func, method, type, const, var, class
	Name      string `json:"name"`
	Signature string `json:"signature"` // trimmed source line of the definition
}

// Extractor turns source text into symbols. Backends are pluggable: the
// built-ins are regex-based per language; a tree-sitter or ctags backend can
// replace them behind the same interface.
type Extractor interface {
	// Extensions returns the file extensions (with dot) this extractor handles.
	Extensions() []string
	// Extract returns the symbols defined in src.
	Extract(relPath string, src []byte) []Symbol
}

// DefaultExtractors covers the languages the built-in regex backend supports.
func DefaultExtractors() []Extractor {
	return []Extractor{goExtractor{}, pyExtractor{}, jsExtractor{}}
}

// regexExtract runs kind-tagged patterns line by line. Each pattern must have
// exactly one capture group: the symbol name. mayDefine is a cheap necessary
// condition for any rule to match a line (a keyword the rules start with):
// lines failing it skip the regexps, which is most lines of a file (the
// per-file cost on the 61k-file corpus dropped ~6x; symindex_bench_test.go).
// The returned strings are copies, so they do not keep src alive.
func regexExtract(relPath string, src []byte, rules []regexRule, mayDefine func(line []byte) bool) []Symbol {
	var out []Symbol
	path := filepath.ToSlash(relPath)
	for num := 1; len(src) > 0; num++ {
		line := src
		if i := bytes.IndexByte(src, '\n'); i >= 0 {
			line, src = src[:i], src[i+1:]
		} else {
			src = nil
		}
		trimmed := bytes.TrimRight(line, " \t\r")
		if !mayDefine(trimmed) {
			continue
		}
		text := string(trimmed)
		for _, r := range rules {
			m := r.re.FindStringSubmatchIndex(text)
			if m == nil {
				continue
			}
			sig := strings.TrimSpace(text)
			out = append(out, Symbol{
				Path:      path,
				Line:      num,
				Kind:      r.kind,
				Name:      text[m[2]:m[3]],
				Signature: sig,
			})
			break
		}
	}
	return out
}

type regexRule struct {
	kind string
	re   *regexp.Regexp
}

// startsWithWord reports whether line, after the leading white space \s
// matches, starts with one of words.
func startsWithWord(line []byte, words []string) bool {
	line = bytes.TrimLeft(line, " \t\n\f\r")
	for _, w := range words {
		if bytes.HasPrefix(line, []byte(w)) {
			return true
		}
	}
	return false
}

// --- Go ---

type goExtractor struct{}

var goRules = []regexRule{
	{"method", regexp.MustCompile(`^func\s+\([^)]+\)\s+([A-Za-z_][A-Za-z0-9_]*)\s*\(`)},
	{"func", regexp.MustCompile(`^func\s+([A-Za-z_][A-Za-z0-9_]*)\s*[([]`)},
	{"type", regexp.MustCompile(`^type\s+([A-Za-z_][A-Za-z0-9_]*)\s`)},
	{"const", regexp.MustCompile(`^const\s+([A-Za-z_][A-Za-z0-9_]*)\s`)},
	{"var", regexp.MustCompile(`^var\s+([A-Za-z_][A-Za-z0-9_]*)\s`)},
}

// goMayDefine: every Go rule is anchored at column 0 on its keyword.
func goMayDefine(line []byte) bool {
	if len(line) == 0 {
		return false
	}
	switch line[0] {
	case 'f':
		return bytes.HasPrefix(line, []byte("func"))
	case 't':
		return bytes.HasPrefix(line, []byte("type"))
	case 'c':
		return bytes.HasPrefix(line, []byte("const"))
	case 'v':
		return bytes.HasPrefix(line, []byte("var"))
	}
	return false
}

func (goExtractor) Extensions() []string { return []string{".go"} }
func (goExtractor) Extract(relPath string, src []byte) []Symbol {
	return regexExtract(relPath, src, goRules, goMayDefine)
}

// --- Python ---

type pyExtractor struct{}

var pyRules = []regexRule{
	{"func", regexp.MustCompile(`^\s*(?:async\s+)?def\s+([A-Za-z_][A-Za-z0-9_]*)\s*\(`)},
	{"class", regexp.MustCompile(`^\s*class\s+([A-Za-z_][A-Za-z0-9_]*)\s*[(:]`)},
	{"const", regexp.MustCompile(`^([A-Z_][A-Z0-9_]*)\s*=`)},
}

var pyKeywords = []string{"async", "def", "class"}

// pyMayDefine: a def or class keyword after indentation, or a column-0
// upper-case name (a constant).
func pyMayDefine(line []byte) bool {
	if len(line) > 0 && (line[0] == '_' || 'A' <= line[0] && line[0] <= 'Z') {
		return true
	}
	return startsWithWord(line, pyKeywords)
}

func (pyExtractor) Extensions() []string { return []string{".py"} }
func (pyExtractor) Extract(relPath string, src []byte) []Symbol {
	return regexExtract(relPath, src, pyRules, pyMayDefine)
}

// --- JavaScript / TypeScript ---

type jsExtractor struct{}

var jsRules = []regexRule{
	{"func", regexp.MustCompile(`^\s*(?:export\s+)?(?:default\s+)?(?:async\s+)?function\s*\*?\s*([A-Za-z_$][A-Za-z0-9_$]*)\s*\(`)},
	{"class", regexp.MustCompile(`^\s*(?:export\s+)?(?:default\s+)?(?:abstract\s+)?class\s+([A-Za-z_$][A-Za-z0-9_$]*)`)},
	{"type", regexp.MustCompile(`^\s*(?:export\s+)?(?:interface|type|enum)\s+([A-Za-z_$][A-Za-z0-9_$]*)`)},
	{"const", regexp.MustCompile(`^\s*(?:export\s+)?(?:const|let|var)\s+([A-Za-z_$][A-Za-z0-9_$]*)\s*=\s*(?:async\s*)?(?:\(|function|[A-Za-z_$(<]|\d|['"{[])`)},
}

// jsKeywords are the words a JS/TS rule can start with.
var jsKeywords = []string{"export", "default", "async", "function", "abstract", "class", "interface", "type", "enum", "const", "let", "var"}

func jsMayDefine(line []byte) bool { return startsWithWord(line, jsKeywords) }

func (jsExtractor) Extensions() []string {
	return []string{".js", ".jsx", ".ts", ".tsx", ".mjs", ".cjs"}
}
func (jsExtractor) Extract(relPath string, src []byte) []Symbol {
	return regexExtract(relPath, src, jsRules, jsMayDefine)
}
