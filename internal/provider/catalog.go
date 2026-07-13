package provider

import (
	"fmt"
	"sort"
	"strings"

	"spettro/internal/models"
)

func (m Model) Tag() string {
	var parts []string
	if m.Vision {
		parts = append(parts, "img")
	}
	if m.Reasoning {
		parts = append(parts, "think")
	}
	if m.Status != "" {
		parts = append(parts, m.Status)
	}
	if m.Context > 0 {
		switch {
		case m.Context >= 1_000_000:
			parts = append(parts, fmt.Sprintf("%dM ctx", m.Context/1_000_000))
		case m.Context >= 1_000:
			parts = append(parts, fmt.Sprintf("%dk ctx", m.Context/1_000))
		default:
			parts = append(parts, fmt.Sprintf("%d ctx", m.Context))
		}
	}
	return strings.Join(parts, "  ")
}

// fallbackModels covers first-run/offline before the catalog is available.
// Only anthropic and openai are listed because they are the only providers
// whose endpoints resolve without a catalog base_url.
var fallbackModels = []Model{
	{Provider: "anthropic", ProviderName: "Anthropic", Name: "claude-opus-4", DisplayName: "Claude Opus 4", Vision: true, Reasoning: true, ToolCall: true, PromptCaching: true, EnvKey: "ANTHROPIC_API_KEY"},
	{Provider: "anthropic", ProviderName: "Anthropic", Name: "claude-sonnet-4-5", DisplayName: "Claude Sonnet 4.5", Vision: true, Reasoning: true, ToolCall: true, PromptCaching: true, EnvKey: "ANTHROPIC_API_KEY"},
	{Provider: "openai", ProviderName: "OpenAI", Name: "gpt-4.1", DisplayName: "GPT-4.1", Vision: true, ToolCall: true, EnvKey: "OPENAI_API_KEY"},
	{Provider: "openai", ProviderName: "OpenAI", Name: "o3", DisplayName: "o3", Vision: true, Reasoning: true, ToolCall: true, EnvKey: "OPENAI_API_KEY"},
}

func buildModels(cat models.Catalog) []Model {
	providerIDs := make([]string, 0, len(cat.Providers))
	for id := range cat.Providers {
		providerIDs = append(providerIDs, id)
	}
	sort.Slice(providerIDs, func(i, j int) bool {
		if providerIDs[i] == "anthropic" {
			return true
		}
		if providerIDs[j] == "anthropic" {
			return false
		}
		return providerIDs[i] < providerIDs[j]
	})

	var out []Model
	for _, pid := range providerIDs {
		prov := cat.Providers[pid]
		modelIDs := make([]string, 0, len(prov.Models))
		for id := range prov.Models {
			modelIDs = append(modelIDs, id)
		}
		sort.Strings(modelIDs)

		for _, mid := range modelIDs {
			mod := prov.Models[mid]
			out = append(out, Model{
				Provider:      pid,
				ProviderName:  prov.Name,
				Name:          mid,
				DisplayName:   mod.Name,
				Vision:        mod.Vision,
				Reasoning:     mod.Reasoning,
				ToolCall:      mod.ToolCall,
				PromptCaching: prov.API == models.APIAnthropic,
				Context:       mod.Context,
				Status:        mod.Status,
				EnvKey:        prov.Env,
			})
		}
	}
	return out
}
