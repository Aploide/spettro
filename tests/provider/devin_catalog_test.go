package provider_test

import (
	"testing"

	"spettro/internal/models"
	"spettro/internal/provider"
)

// TestDevinSurvivesCatalogRefresh is a regression test for the /connect
// picker not showing Devin once models.dev took over the catalog. Devin
// is not an upstream-known chat provider, so a naive SetCatalog
// replacement wipes it; alwaysIncludedModels re-merges it back into both
// the raw Models() list and the AllProviderInfos() derivation used by
// /connect.
func TestDevinSurvivesCatalogRefresh(t *testing.T) {
	pm := provider.NewManager()

	// Sanity: in the fallback path Devin shows up in the provider list.
	preFallback := containsProvider(pm.AllProviderInfos(), "devin")
	if !preFallback {
		t.Fatalf("devin should be visible pre-catalog (fallback path)")
	}

	// Simulate the real-world catalog: anthropic + openai, no devin.
	pm.SetCatalog(models.Catalog{
		"anthropic": models.DevProvider{
			ID: "anthropic", Name: "Anthropic", Env: []string{"ANTHROPIC_API_KEY"},
			Models: map[string]models.DevModel{
				"claude-opus-4-5": {ID: "claude-opus-4-5", Name: "Claude Opus 4.5"},
			},
		},
		"openai": models.DevProvider{
			ID: "openai", Name: "OpenAI", Env: []string{"OPENAI_API_KEY"},
			Models: map[string]models.DevModel{
				"gpt-5": {ID: "gpt-5", Name: "GPT-5"},
			},
		},
	})

	if !containsProvider(pm.AllProviderInfos(), "devin") {
		t.Fatalf("devin missing from /connect picker after a catalog refresh; suggestedProviderIDs / alwaysIncludedModels merge regressed")
	}
	if !pm.HasModel("devin", "session") {
		t.Fatalf("devin:session model missing after catalog refresh")
	}
	// The catalog entries still flow through.
	if !pm.HasModel("anthropic", "claude-opus-4-5") {
		t.Fatalf("anthropic:claude-opus-4-5 missing after catalog refresh")
	}
}

func containsProvider(infos []provider.ProviderInfo, id string) bool {
	for _, pi := range infos {
		if pi.ID == id {
			return true
		}
	}
	return false
}
