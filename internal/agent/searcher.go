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
// at <cwd>/.spettro/cache/symbols.idx, so the TUI's startup warm-up and
// every session's lookups use the same warm index.
func NewRepoSearcher(cwd string) RepoSearcher {
	return RepoSearcher{Index: indexer.Shared(cwd, filepath.Join(cwd, ".spettro", "cache", "symbols.idx"))}
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
//
// The grep runs first: every file defining the name contains it, so the
// files it matched are handed to the index (Refresh) before the lookup,
// and the definitions listed are current for all of them even when they
// changed outside Spettro since the index last synced.
func (s RepoSearcher) searchWith(ctx context.Context, r *toolRuntime, query string) (string, error) {
	if query == "" {
		return r.runGlob(ctx, "**", "")
	}
	found, err := r.grepMatches(ctx, grepArgs{Pattern: regexp.QuoteMeta(query), CaseInsensitive: true, MaxResults: maxSymbolUsages})
	if err != nil {
		return "", err
	}
	header, defs := s.symbolHeader(ctx, query, found.files())
	usages := r.reportGrep(found)
	if len(found.results) == 0 && defs > 0 {
		return header + fmt.Sprintf("no other matches for %q", query), nil
	}
	return header + usages, nil
}

// maxDefs caps the definitions listed, so a common name cannot drown the
// usages.
const maxDefs = 20

// symbolHeader returns a "definitions:" block for identifier-shaped queries,
// ranked best-first by the symbol index, after re-checking the files the
// usage grep matched (seen), and how many definitions it found. The block
// is "" when the index is disabled, the query is not an identifier, or
// there is nothing to say. When the index was cut short (file cap or time
// bound) it says so, also when no definition was found, since the
// definition may be in the part never indexed.
func (s RepoSearcher) symbolHeader(ctx context.Context, query string, seen []string) (string, int) {
	if s.Index == nil || !identifierRE.MatchString(query) {
		return "", 0
	}
	s.Index.Refresh(ctx, seen)
	syms := s.Index.Lookup(ctx, query)
	cut := s.Index.Truncated().Describe()
	var b strings.Builder
	switch {
	case len(syms) == 0 && cut == "":
		return "", 0
	case len(syms) == 0:
		fmt.Fprintf(&b, "(no definitions of %q in the symbol index, which %s: definitions in the files it did not reach are not listed, but the matches below come from every file)\n\n", query, cut)
		return b.String(), 0
	}
	fmt.Fprintf(&b, "%d definitions:\n", len(syms))
	for _, sym := range syms[:min(len(syms), maxDefs)] {
		fmt.Fprintf(&b, "%s:%d  %s %s  %s\n", sym.Path, sym.Line, sym.Kind, sym.Name, sym.Signature)
	}
	if len(syms) > maxDefs {
		fmt.Fprintf(&b, "... %d more definitions omitted\n", len(syms)-maxDefs)
	}
	if cut != "" {
		fmt.Fprintf(&b, "(the symbol index %s: definitions in the rest are not listed, but the matches below come from every file)\n", cut)
	}
	b.WriteString("\n")
	return b.String(), len(syms)
}
