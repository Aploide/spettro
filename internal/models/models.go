// Package models fetches and caches the Spettro provider catalog
// (catalog.spettro.app), a curated build of models.dev plus
// community-submitted providers.
package models

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"spettro/internal/homedir"
	"spettro/internal/safeio"
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
// LoadAndRefresh also keeps the catalog current in the background.
func Load() (Catalog, error) {
	if cached, err := readCache(); err == nil {
		return cached.catalog, nil
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

// cachedCatalog is the content of the disk cache, catalog.json.
type cachedCatalog struct {
	catalog Catalog
	// sum is the SHA-256 of the file, which identifies this copy (see
	// cacheMeta.CacheSHA256 and refresher.applied).
	sum [sha256.Size]byte
	// modTime is when the file was last written.
	modTime time.Time
}

// readCache returns the disk cache, or an error when it is missing or
// unusable.
func readCache() (cachedCatalog, error) {
	path, err := cacheFile()
	if err != nil {
		return cachedCatalog{}, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return cachedCatalog{}, err
	}
	data, err := safeio.ReadFile(path)
	if err != nil {
		return cachedCatalog{}, err
	}
	cat, err := parseCatalog(data)
	if err != nil {
		return cachedCatalog{}, err
	}
	return cachedCatalog{catalog: cat, sum: sha256.Sum256(data), modTime: info.ModTime()}, nil
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
