// Package models fetches and caches the Spettro provider catalog
// (catalog.spettro.app), a curated build of models.dev plus
// community-submitted providers.
package models

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

const apiURL = "https://catalog.spettro.app/providers.min.json"

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

// cacheFile returns the path to the local JSON cache.
func cacheFile() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".spettro", "catalog.json"), nil
}

// Load returns the catalog from disk cache, or fetches it if unavailable.
func Load() (Catalog, error) {
	path, err := cacheFile()
	if err == nil {
		if data, err := os.ReadFile(path); err == nil {
			var cat Catalog
			if json.Unmarshal(data, &cat) == nil && len(cat.Providers) > 0 {
				return cat, nil
			}
		}
	}
	return Fetch()
}

// Fetch downloads the catalog and updates the disk cache.
func Fetch() (Catalog, error) {
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(apiURL)
	if err != nil {
		return Catalog{}, fmt.Errorf("catalog fetch: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return Catalog{}, fmt.Errorf("catalog fetch: status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return Catalog{}, fmt.Errorf("catalog read: %w", err)
	}

	var cat Catalog
	if err := json.Unmarshal(body, &cat); err != nil {
		return Catalog{}, fmt.Errorf("catalog parse: %w", err)
	}
	if len(cat.Providers) == 0 {
		return Catalog{}, fmt.Errorf("catalog parse: no providers")
	}

	if path, err := cacheFile(); err == nil {
		_ = os.MkdirAll(filepath.Dir(path), 0o755)
		_ = os.WriteFile(path, body, 0o644)
	}

	return cat, nil
}

// RefreshBackground starts a goroutine that refreshes the cache once now and
// then every hour.
func RefreshBackground(onRefresh func(Catalog)) {
	go func() {
		if cat, err := Fetch(); err == nil && onRefresh != nil {
			onRefresh(cat)
		}
		ticker := time.NewTicker(time.Hour)
		defer ticker.Stop()
		for range ticker.C {
			if cat, err := Fetch(); err == nil && onRefresh != nil {
				onRefresh(cat)
			}
		}
	}()
}
