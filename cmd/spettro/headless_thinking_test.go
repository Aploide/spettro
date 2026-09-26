package main

import (
	"testing"

	"spettro/internal/config"
	"spettro/internal/models"
	"spettro/internal/provider"
)

// Headless and goal runs honor the user's thinking_level exactly like the
// ACP bridge: sent for reasoning-capable models, omitted otherwise.
func TestConfiguredThinking(t *testing.T) {
	pm := provider.NewManager()
	pm.SetCatalog(models.Catalog{Providers: map[string]models.CatalogProvider{
		"p": {API: models.APIOpenAI, BaseURL: "https://x", Models: map[string]models.CatalogModel{
			"thinker": {Reasoning: true},
			"plain":   {},
		}},
	}})
	cfg := config.UserConfig{ActiveProvider: "p", ActiveModel: "thinker", ThinkingLevel: "high"}
	if got := configuredThinking(pm, cfg); got != provider.ThinkingHigh {
		t.Fatalf("reasoning model: got %q, want high", got)
	}
	cfg.ActiveModel = "plain"
	if got := configuredThinking(pm, cfg); got != "" {
		t.Fatalf("non-reasoning model: got %q, want none", got)
	}
}
