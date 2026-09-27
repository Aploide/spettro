// Package models fetches and caches the Spettro provider catalog
// (catalog.spettro.app), a curated build of models.dev plus
// community-submitted providers.
package models

import (
	"bytes"
	"compress/gzip"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"spettro/internal/homedir"
)

// catalogURL is where the catalog is served; a variable so tests can point
// it at a local server.
var catalogURL = "https://catalog.spettro.app/providers.min.json"

// APIKind is the wire protocol used to talk to a provider. Spettro's backend
// supports exactly two: OpenAI-compatible and Anthropic-compatible.
const (
	APIOpenAI    = "openai"
	APIAnthropic = "anthropic"
)

// CatalogModel is a single chat model entry. Boolean capability flags and
// status are omitted from the JSON when false/empty.
type CatalogModel struct {
	Name      string `json:"name"`
	Reasoning bool   `json:"reasoning,omitempty"`
	ToolCall  bool   `json:"tool_call,omitempty"`
	Vision    bool   `json:"vision,omitempty"`
	Context   int    `json:"context,omitempty"`
	Status    string `json:"status,omitempty"` // "alpha" | "beta"
	// Output is the model's maximum output tokens (models.dev limit.output),
	// when the catalog carries it; 0 = unknown.
	Output int `json:"output,omitempty"`
}

// CatalogProvider is one provider entry from the Spettro catalog.
type CatalogProvider struct {
	Name    string                  `json:"name"`
	API     string                  `json:"api"` // APIOpenAI or APIAnthropic
	BaseURL string                  `json:"base_url"`
	Env     string                  `json:"env"`
	Models  map[string]CatalogModel `json:"models"`
}

// Catalog is the full document served by catalog.spettro.app.
type Catalog struct {
	Version   int                        `json:"version"`
	Updated   string                     `json:"updated"`
	Providers map[string]CatalogProvider `json:"providers"`
}

// cacheTTL is how old the disk cache may be before the background refresh
// asks the server again. The server answers a conditional request with 304
// when nothing changed, so an expired cache usually costs one round trip and
// no download.
const cacheTTL = 6 * time.Hour

// refreshInterval is how often a long-running process re-checks the cache
// age; a request is made only once the cache is older than cacheTTL.
const refreshInterval = time.Hour

// snapshotGz is the catalog as it was when this binary was built, gzip
// compressed (about 9 KB). Load serves it when there is no usable disk cache
// (first run, offline, a broken proxy), so startup never waits for the
// network. Regenerate it with "go generate ./internal/models"; the release
// workflow does so before every build.
//
//go:generate go run ./internal/snapshotgen -o catalog_snapshot.json.gz
//go:embed catalog_snapshot.json.gz
var snapshotGz []byte

// cacheFile returns the path to the local JSON cache.
func cacheFile() (string, error) {
	home, err := homedir.Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".spettro", "catalog.json"), nil
}

// Load returns the catalog from the disk cache, or the snapshot embedded in
// the binary when there is no usable cache. It never touches the network;
// RefreshBackground brings the cache up to date.
func Load() (Catalog, error) {
	if cat, _, err := readCache(); err == nil {
		return cat, nil
	}
	return Snapshot()
}

// Snapshot returns the catalog embedded in the binary at build time.
func Snapshot() (Catalog, error) {
	zr, err := gzip.NewReader(bytes.NewReader(snapshotGz))
	if err != nil {
		return Catalog{}, fmt.Errorf("catalog snapshot: %w", err)
	}
	data, err := io.ReadAll(zr)
	if err != nil {
		return Catalog{}, fmt.Errorf("catalog snapshot: %w", err)
	}
	return parseCatalog(data)
}

// readCache returns the disk cache and its modification time, or an error
// when it is missing or unusable.
func readCache() (Catalog, time.Time, error) {
	path, err := cacheFile()
	if err != nil {
		return Catalog{}, time.Time{}, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return Catalog{}, time.Time{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Catalog{}, time.Time{}, err
	}
	cat, err := parseCatalog(data)
	if err != nil {
		return Catalog{}, time.Time{}, err
	}
	return cat, info.ModTime(), nil
}

// parseCatalog decodes a catalog document and rejects one without providers.
func parseCatalog(data []byte) (Catalog, error) {
	var cat Catalog
	if err := json.Unmarshal(data, &cat); err != nil {
		return Catalog{}, fmt.Errorf("catalog parse: %w", err)
	}
	if len(cat.Providers) == 0 {
		return Catalog{}, fmt.Errorf("catalog parse: no providers")
	}
	return cat, nil
}

// Fetch downloads the catalog unconditionally and updates the disk cache.
func Fetch() (Catalog, error) {
	cat, _, err := fetch(time.Time{})
	return cat, err
}

// fetch downloads the catalog. With a non-zero cachedAt it sends
// If-Modified-Since, and changed reports false (with an empty catalog) when
// the server answers 304 Not Modified; the cache's modification time then
// moves to now so its TTL restarts. A downloaded catalog is written to the
// disk cache.
func fetch(cachedAt time.Time) (cat Catalog, changed bool, err error) {
	req, err := http.NewRequest(http.MethodGet, catalogURL, nil)
	if err != nil {
		return Catalog{}, false, fmt.Errorf("catalog fetch: %w", err)
	}
	if !cachedAt.IsZero() {
		req.Header.Set("If-Modified-Since", cachedAt.UTC().Format(http.TimeFormat))
	}
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return Catalog{}, false, fmt.Errorf("catalog fetch: %w", err)
	}
	defer resp.Body.Close()

	path, pathErr := cacheFile()
	if resp.StatusCode == http.StatusNotModified && !cachedAt.IsZero() {
		if pathErr == nil {
			now := time.Now()
			_ = os.Chtimes(path, now, now)
		}
		return Catalog{}, false, nil
	}
	if resp.StatusCode != http.StatusOK {
		return Catalog{}, false, fmt.Errorf("catalog fetch: status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return Catalog{}, false, fmt.Errorf("catalog read: %w", err)
	}
	cat, err = parseCatalog(body)
	if err != nil {
		return Catalog{}, false, err
	}
	if pathErr == nil {
		_ = os.MkdirAll(filepath.Dir(path), 0o755)
		_ = os.WriteFile(path, body, 0o644)
	}
	return cat, true, nil
}

// refreshIfStale fetches the catalog when the disk cache is missing,
// unusable or older than cacheTTL, and hands a changed catalog to
// onRefresh. A fresh cache costs one stat and parse and no request.
func refreshIfStale(onRefresh func(Catalog)) {
	_, cachedAt, err := readCache()
	if err == nil && time.Since(cachedAt) < cacheTTL {
		return
	}
	if err != nil {
		cachedAt = time.Time{} // no usable cache: download unconditionally
	}
	cat, changed, err := fetch(cachedAt)
	if err == nil && changed && onRefresh != nil {
		onRefresh(cat)
	}
}

// RefreshBackground starts a goroutine that brings the disk cache up to date
// now and then re-checks it every refreshInterval for the life of the
// process. A check makes a request only when the cache is older than
// cacheTTL (the cache is shared by every spettro process, so a TUI and an
// ACP server started together fetch once), and the request is conditional,
// so an unchanged catalog is not downloaded again. onRefresh runs on that
// goroutine with each catalog that changed.
func RefreshBackground(onRefresh func(Catalog)) {
	go func() {
		refreshIfStale(onRefresh)
		ticker := time.NewTicker(refreshInterval)
		defer ticker.Stop()
		for range ticker.C {
			refreshIfStale(onRefresh)
		}
	}()
}
