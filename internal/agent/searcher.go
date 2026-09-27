package agent

import (
	"context"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"spettro/internal/indexer"
)

// SearchAgent searches the repository for files or content.
type SearchAgent interface {
	Search(ctx context.Context, cwd, query string) (string, error)
}

// RepoSearcher answers grep's symbol form: ranked definitions of an
// identifier from the symbol index (when Index is set and the query looks
// like one), then its usages from the grep engine.
type RepoSearcher struct {
	Index *indexer.SymbolIndex
}

// NewRepoSearcher returns a searcher backed by the project's shared symbol
// index (one per workspace root per process, see indexer.Shared), persisted
// at <cwd>/.spettro/cache/symbols.gob, so the TUI's startup warm-up and
// every session's lookups use the same warm index.
func NewRepoSearcher(cwd string) RepoSearcher {
	return RepoSearcher{Index: indexer.Shared(cwd, filepath.Join(cwd, ".spettro", "cache", "symbols.gob"))}
}

// identifierRE gates symbol lookups: only bare identifier-shaped queries hit
// the index; phrases, regexes and paths go straight to the grep path.
var identifierRE = regexp.MustCompile(`^[A-Za-z_$][A-Za-z0-9_$]*$`)

// maxSymbolUsages caps the usages listed after the definitions: a common
// name ("Context") used to list every occurrence in the tree (43 MB on a
// 61k-file repo).
const maxSymbolUsages = 200

// Search runs the symbol search for cwd with a runtime of its own (no
// sandbox, nothing recorded as read); the grep tool uses searchWith.
func (s RepoSearcher) Search(ctx context.Context, cwd, query string) (string, error) {
	r := &toolRuntime{cwd: cwd, readSet: map[string]struct{}{}, requiredReads: map[string]struct{}{}}
	return s.searchWith(ctx, r, query)
}

// searchWith answers a symbol query through r's search tools: the usages
// are a case-insensitive literal grep (so .gitignore, the binary and size
// filters and max_results apply, and rg is used when available), and an
// empty query lists the workspace's files as glob "**" does (capped).
func (s RepoSearcher) searchWith(ctx context.Context, r *toolRuntime, query string) (string, error) {
	if query == "" {
		return r.runGlob(ctx, "**", "")
	}
	header := s.symbolHeader(ctx, query)
	usages, err := r.runGrep(ctx, grepArgs{Pattern: regexp.QuoteMeta(query), CaseInsensitive: true, MaxResults: maxSymbolUsages})
	if err != nil {
		return "", err
	}
	if strings.HasPrefix(usages, "no matches") && header != "" {
		return header + fmt.Sprintf("no other matches for %q", query), nil
	}
	return header + usages, nil
}

// symbolHeader returns a "definitions:" block for identifier-shaped queries,
// ranked best-first by the symbol index, or "" when the index is disabled or
// has nothing. Capped so a common name can't drown the content matches.
func (s RepoSearcher) symbolHeader(ctx context.Context, query string) string {
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
	if truncated, limit := s.Index.Truncated(); truncated {
		fmt.Fprintf(&b, "(the symbol index stopped at %d source files: definitions in the rest are not listed, but the matches below come from every file)\n", limit)
	}
	b.WriteString("\n")
	return b.String()
}
