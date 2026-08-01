package agent

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"spettro/internal/indexer"
)

// SearchAgent searches the repository for files or content.
type SearchAgent interface {
	Search(ctx context.Context, cwd, query string) (string, error)
}

// RepoSearcher walks the repo tree and optionally greps file contents. When
// Index is set and the query looks like a symbol name, ranked definitions
// from the symbol index are listed before the plain content matches.
type RepoSearcher struct {
	Index *indexer.SymbolIndex
}

// NewRepoSearcher returns a searcher backed by the project's symbol index,
// persisted at <cwd>/.spettro/cache/symbols.json.
func NewRepoSearcher(cwd string) RepoSearcher {
	return RepoSearcher{Index: indexer.NewSymbolIndex(cwd, filepath.Join(cwd, ".spettro", "cache", "symbols.json"))}
}

// identifierRE gates symbol lookups: only bare identifier-shaped queries hit
// the index; phrases, regexes and paths go straight to the grep path.
var identifierRE = regexp.MustCompile(`^[A-Za-z_$][A-Za-z0-9_$]*$`)

func (s RepoSearcher) Search(ctx context.Context, cwd, query string) (string, error) {
	var files []string
	err := filepath.WalkDir(cwd, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable entries
		}
		rel, _ := filepath.Rel(cwd, path)
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", ".spettro", "vendor", "node_modules", "dist", "build":
				return filepath.SkipDir
			}
			return nil
		}
		files = append(files, rel)
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("walk: %w", err)
	}

	if query == "" {
		return fmt.Sprintf("%d files:\n%s", len(files), strings.Join(files, "\n")), nil
	}

	q := strings.ToLower(query)
	var results []string
	for _, rel := range files {
		abs := filepath.Join(cwd, rel)
		data, err := os.ReadFile(abs)
		if err != nil {
			continue
		}
		for i, line := range strings.Split(string(data), "\n") {
			if strings.Contains(strings.ToLower(line), q) {
				results = append(results, fmt.Sprintf("%s:%d: %s", rel, i+1, strings.TrimSpace(line)))
			}
		}
	}

	header := s.symbolHeader(ctx, cwd, query)
	if len(results) == 0 {
		if header != "" {
			return header + fmt.Sprintf("no other matches for %q in %d files", query, len(files)), nil
		}
		return fmt.Sprintf("no matches for %q in %d files", query, len(files)), nil
	}
	return header + fmt.Sprintf("%d matches:\n%s", len(results), strings.Join(results, "\n")), nil
}

// symbolHeader returns a "definitions:" block for identifier-shaped queries,
// ranked best-first by the symbol index, or "" when the index is disabled or
// has nothing. Capped so a common name can't drown the content matches.
func (s RepoSearcher) symbolHeader(ctx context.Context, cwd, query string) string {
	if s.Index == nil || !identifierRE.MatchString(query) {
		return ""
	}
	syms := s.Index.Lookup(ctx, query)
	if len(syms) == 0 {
		return ""
	}
	const maxDefs = 20
	total := len(syms)
	if len(syms) > maxDefs {
		syms = syms[:maxDefs]
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d definitions:\n", total)
	for _, sym := range syms {
		fmt.Fprintf(&b, "%s:%d  %s %s  %s\n", sym.Path, sym.Line, sym.Kind, sym.Name, sym.Signature)
	}
	if total > maxDefs {
		fmt.Fprintf(&b, "... %d more definitions omitted\n", total-maxDefs)
	}
	b.WriteString("\n")
	return b.String()
}
