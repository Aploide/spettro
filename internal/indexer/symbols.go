package indexer

import (
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
// exactly one capture group: the symbol name.
func regexExtract(relPath string, src []byte, rules []regexRule) []Symbol {
	var out []Symbol
	for i, line := range strings.Split(string(src), "\n") {
		trimmed := strings.TrimRight(line, " \t\r")
		for _, r := range rules {
			m := r.re.FindStringSubmatch(trimmed)
			if m == nil {
				continue
			}
			out = append(out, Symbol{
				Path:      filepath.ToSlash(relPath),
				Line:      i + 1,
				Kind:      r.kind,
				Name:      m[1],
				Signature: strings.TrimSpace(trimmed),
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

// --- Go ---

type goExtractor struct{}

var goRules = []regexRule{
	{"method", regexp.MustCompile(`^func\s+\([^)]+\)\s+([A-Za-z_][A-Za-z0-9_]*)\s*\(`)},
	{"func", regexp.MustCompile(`^func\s+([A-Za-z_][A-Za-z0-9_]*)\s*[([]`)},
	{"type", regexp.MustCompile(`^type\s+([A-Za-z_][A-Za-z0-9_]*)\s`)},
	{"const", regexp.MustCompile(`^const\s+([A-Za-z_][A-Za-z0-9_]*)\s`)},
	{"var", regexp.MustCompile(`^var\s+([A-Za-z_][A-Za-z0-9_]*)\s`)},
}

func (goExtractor) Extensions() []string { return []string{".go"} }
func (goExtractor) Extract(relPath string, src []byte) []Symbol {
	return regexExtract(relPath, src, goRules)
}

// --- Python ---

type pyExtractor struct{}

var pyRules = []regexRule{
	{"func", regexp.MustCompile(`^\s*(?:async\s+)?def\s+([A-Za-z_][A-Za-z0-9_]*)\s*\(`)},
	{"class", regexp.MustCompile(`^\s*class\s+([A-Za-z_][A-Za-z0-9_]*)\s*[(:]`)},
	{"const", regexp.MustCompile(`^([A-Z_][A-Z0-9_]*)\s*=`)},
}

func (pyExtractor) Extensions() []string { return []string{".py"} }
func (pyExtractor) Extract(relPath string, src []byte) []Symbol {
	return regexExtract(relPath, src, pyRules)
}

// --- JavaScript / TypeScript ---

type jsExtractor struct{}

var jsRules = []regexRule{
	{"func", regexp.MustCompile(`^\s*(?:export\s+)?(?:default\s+)?(?:async\s+)?function\s*\*?\s*([A-Za-z_$][A-Za-z0-9_$]*)\s*\(`)},
	{"class", regexp.MustCompile(`^\s*(?:export\s+)?(?:default\s+)?(?:abstract\s+)?class\s+([A-Za-z_$][A-Za-z0-9_$]*)`)},
	{"type", regexp.MustCompile(`^\s*(?:export\s+)?(?:interface|type|enum)\s+([A-Za-z_$][A-Za-z0-9_$]*)`)},
	{"const", regexp.MustCompile(`^\s*(?:export\s+)?(?:const|let|var)\s+([A-Za-z_$][A-Za-z0-9_$]*)\s*=\s*(?:async\s*)?(?:\(|function|[A-Za-z_$(<]|\d|['"{[])`)},
}

func (jsExtractor) Extensions() []string {
	return []string{".js", ".jsx", ".ts", ".tsx", ".mjs", ".cjs"}
}
func (jsExtractor) Extract(relPath string, src []byte) []Symbol {
	return regexExtract(relPath, src, jsRules)
}
