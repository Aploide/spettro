package main

import (
	"testing"

	"spettro/internal/config"
	"spettro/internal/models"
	"spettro/internal/provider"
	"spettro/internal/spettro"
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

	// Spettro Subscription models, registered the way headless and ACP
	// startup do, get the level unless the plan's model list marks them
	// reasoning:false; a list that does not flag reasoning either way still
	// gets it (it becomes reasoning_effort on the proxy).
	yes, no := true, false
	pm.SetSpettro("https://inference.example/v1", spettro.ProviderModels([]spettro.ModelInfo{
		{ID: "flash"}, {ID: "thinker", Reasoning: &yes}, {ID: "chat", Reasoning: &no},
	}))
	for _, m := range pm.Models() {
		if m.Provider == spettro.ProviderID && m.Name == "thinker" && !m.Reasoning {
			t.Error("spettro.ProviderModels dropped the reasoning flag")
		}
	}
	for _, model := range []string{"flash", "thinker"} {
		cfg := config.UserConfig{ActiveProvider: spettro.ProviderID, ActiveModel: model, ThinkingLevel: "low"}
		if got := configuredThinking(pm, cfg); got != provider.ThinkingLow {
			t.Errorf("spettro %s: got %q, want low", model, got)
		}
	}
	cfg = config.UserConfig{ActiveProvider: spettro.ProviderID, ActiveModel: "chat", ThinkingLevel: "high"}
	if got := configuredThinking(pm, cfg); got != "" {
		t.Errorf("spettro model listed reasoning:false: got %q, want none", got)
	}
}
