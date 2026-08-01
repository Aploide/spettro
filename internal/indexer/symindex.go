package indexer

import (
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"spettro/internal/ignore"
)

// Build/scan bounds. The index must never make repo-search slower than plain
// grep in the fallback case, so an oversized or slow repo simply stops
// indexing where the cap hits and later queries answer from what was indexed.
const (
	defaultMaxFiles    = 20000
	defaultMaxDuration = 5 * time.Second
	maxFileSize        = 1 << 20 // skip files >1MiB, generated/vendored blobs
)

// fileSymbols is the cached index entry for one source file.
type fileSymbols struct {
	ModTime time.Time `json:"mod_time"`
	Size    int64     `json:"size"`
	Symbols []Symbol  `json:"symbols"`
}

// symbolCache is the on-disk JSON layout.
type symbolCache struct {
	Version int                     `json:"version"`
	Root    string                  `json:"root"`
	Files   map[string]*fileSymbols `json:"files"`
}

const cacheVersion = 1

// SymbolIndex is a lazily built, cached symbol index for one project root.
// The zero value is not usable; construct with NewSymbolIndex.
type SymbolIndex struct {
	root        string
	cachePath   string // "" disables persistence
	extractors  []Extractor
	maxFiles    int
	maxDuration time.Duration

	mu    sync.Mutex
	built bool
	files map[string]*fileSymbols // rel path -> entry
	byExt map[string]Extractor
}

// NewSymbolIndex creates an index for root, persisting its cache at cachePath
// (pass "" for in-memory only). Nothing is scanned until the first Lookup.
func NewSymbolIndex(root, cachePath string) *SymbolIndex {
	x := &SymbolIndex{
		root:        root,
		cachePath:   cachePath,
		extractors:  DefaultExtractors(),
		maxFiles:    defaultMaxFiles,
		maxDuration: defaultMaxDuration,
		files:       map[string]*fileSymbols{},
		byExt:       map[string]Extractor{},
	}
	for _, e := range x.extractors {
		for _, ext := range e.Extensions() {
			x.byExt[ext] = e
		}
	}
	return x
}

// Lookup returns symbols matching name, definitions ranked best-first:
// exact-name matches, then prefix matches, then substring matches
// (case-insensitive within each tier). It builds or refreshes the index
// first, so results reflect on-disk state — files edited since the last
// query (including by the agent's own file-write/edit tools) are re-parsed
// via their mtime/size.
func (x *SymbolIndex) Lookup(ctx context.Context, name string) []Symbol {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	x.ensureLocked(ctx)

	lower := strings.ToLower(name)
	type scored struct {
		sym  Symbol
		rank int
	}
	var hits []scored
	for _, entry := range x.files {
		for _, s := range entry.Symbols {
			ln := strings.ToLower(s.Name)
			var rank int
			switch {
			case s.Name == name:
				rank = 0
			case ln == lower:
				rank = 1
			case strings.HasPrefix(ln, lower):
				rank = 2
			case strings.Contains(ln, lower):
				rank = 3
			default:
				continue
			}
			hits = append(hits, scored{s, rank})
		}
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].rank != hits[j].rank {
			return hits[i].rank < hits[j].rank
		}
		if hits[i].sym.Path != hits[j].sym.Path {
			return hits[i].sym.Path < hits[j].sym.Path
		}
		return hits[i].sym.Line < hits[j].sym.Line
	})
	out := make([]Symbol, len(hits))
	for i, h := range hits {
		out[i] = h.sym
	}
	return out
}

// Warm builds (or refreshes) the index and persists its cache without
// running a query, so hosts can pay the first-scan cost in the background at
// startup instead of on the first symbol search.
func (x *SymbolIndex) Warm(ctx context.Context) {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.ensureLocked(ctx)
}

// Invalidate drops the cached entry for a root-relative path so the next
// Lookup re-parses it. Lookup's mtime check already catches on-disk changes;
// this exists for callers that want to force it (e.g. straight after a write).
func (x *SymbolIndex) Invalidate(relPath string) {
	x.mu.Lock()
	defer x.mu.Unlock()
	delete(x.files, filepath.ToSlash(relPath))
}

// ensureLocked builds the index on first use and re-syncs it with the file
// system on every call: new/changed files are (re)parsed, deleted files are
// dropped. Bounded by maxFiles and maxDuration. Callers hold x.mu.
func (x *SymbolIndex) ensureLocked(ctx context.Context) {
	if !x.built {
		x.loadCache()
		x.built = true
	}

	deadline := time.Now().Add(x.maxDuration)
	gi := ignore.NewMatcher(x.root)
	seen := make(map[string]struct{}, len(x.files))
	count := 0
	changed := false

	_ = filepath.WalkDir(x.root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable entries
		}
		if ctx.Err() != nil || time.Now().After(deadline) || count >= x.maxFiles {
			return fs.SkipAll
		}
		rel, relErr := filepath.Rel(x.root, path)
		if relErr != nil || rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if skipDir(d.Name()) || gi.Ignored(rel, true) {
				return filepath.SkipDir
			}
			return nil
		}
		ext := strings.ToLower(filepath.Ext(rel))
		if _, ok := x.byExt[ext]; !ok {
			return nil
		}
		if gi.Ignored(rel, false) {
			return nil
		}
		info, infoErr := d.Info()
		if infoErr != nil || info.Size() > maxFileSize {
			return nil
		}
		count++
		seen[rel] = struct{}{}
		if prev, ok := x.files[rel]; ok && prev.ModTime.Equal(info.ModTime()) && prev.Size == info.Size() {
			return nil // cache hit
		}
		src, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil
		}
		x.files[rel] = &fileSymbols{
			ModTime: info.ModTime(),
			Size:    info.Size(),
			Symbols: x.byExt[ext].Extract(rel, src),
		}
		changed = true
		return nil
	})

	for rel := range x.files {
		if _, ok := seen[rel]; !ok {
			delete(x.files, rel)
			changed = true
		}
	}
	if changed {
		x.saveCache()
	}
}

func skipDir(name string) bool {
	switch name {
	case ".git", ".spettro", "vendor", "node_modules", "dist", "build", "__pycache__", ".venv", "venv":
		return true
	}
	return false
}

func (x *SymbolIndex) loadCache() {
	if x.cachePath == "" {
		return
	}
	raw, err := os.ReadFile(x.cachePath)
	if err != nil {
		return
	}
	var c symbolCache
	if json.Unmarshal(raw, &c) != nil || c.Version != cacheVersion || c.Root != x.root || c.Files == nil {
		return
	}
	x.files = c.Files
}

func (x *SymbolIndex) saveCache() {
	if x.cachePath == "" {
		return
	}
	raw, err := json.Marshal(symbolCache{Version: cacheVersion, Root: x.root, Files: x.files})
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(x.cachePath), 0o755); err != nil {
		return
	}
	// Best-effort persistence: a failed write just means a cold rebuild next
	// session, never a failed search.
	tmp := x.cachePath + ".tmp"
	if os.WriteFile(tmp, raw, 0o644) == nil {
		_ = os.Rename(tmp, x.cachePath)
	}
}
