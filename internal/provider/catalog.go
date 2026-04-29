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

// fallbackModels backs the /models picker and provider list before the
// models.dev catalog has been loaded or when it fails to fetch. These
// entries are REPLACED by the models.dev catalog once it becomes
// available (so the data stays up to date). Providers that need to
// survive catalog refreshes live in alwaysIncludedModels instead.
var fallbackModels = []Model{
	{Provider: "anthropic", ProviderName: "Anthropic", Name: "claude-opus-4", DisplayName: "Claude Opus 4", Vision: true, Reasoning: true, ToolCall: true, EnvKey: "ANTHROPIC_API_KEY"},
	{Provider: "anthropic", ProviderName: "Anthropic", Name: "claude-sonnet-4-5", DisplayName: "Claude Sonnet 4.5", Vision: true, Reasoning: true, ToolCall: true, EnvKey: "ANTHROPIC_API_KEY"},
	{Provider: "openai", ProviderName: "OpenAI", Name: "gpt-4.1", DisplayName: "GPT-4.1", Vision: true, ToolCall: true, EnvKey: "OPENAI_API_KEY"},
	{Provider: "openai", ProviderName: "OpenAI", Name: "o3", DisplayName: "o3", Vision: true, Reasoning: true, ToolCall: true, EnvKey: "OPENAI_API_KEY"},
	{Provider: "google", ProviderName: "Google", Name: "gemini-2.5-pro", DisplayName: "Gemini 2.5 Pro", Vision: true, Reasoning: true, ToolCall: true, EnvKey: "GOOGLE_API_KEY"},
	{Provider: "x-ai", ProviderName: "xAI", Name: "grok-3", DisplayName: "Grok 3", Vision: true, ToolCall: true, EnvKey: "XAI_API_KEY"},
	{Provider: "groq", ProviderName: "Groq", Name: "llama-3.3-70b-versatile", DisplayName: "Llama 3.3 70B", ToolCall: true, EnvKey: "GROQ_API_KEY"},
}

// alwaysIncludedModels list providers/models that are NOT part of the
// models.dev catalog but are implemented natively by Spettro. They are
// appended to both the fallback list and the catalog-built list so they
// remain visible in /connect and /models across catalog refreshes.
//
// Devin (Cognition) lives here because Devin isn't a chat-completion
// provider in the models.dev sense — it's an agent runner with its own
// session lifecycle, wired through DevinAdapter / devin-session tool.
var alwaysIncludedModels = []Model{
	{Provider: "devin", ProviderName: "Devin (Cognition)", Name: "session", DisplayName: "Devin Session", Reasoning: true, ToolCall: true, EnvKey: "DEVIN_API_KEY"},
}

// mergeAlwaysIncluded appends entries from alwaysIncludedModels to the
// given list, skipping any whose provider already exists. This is the
// hook that keeps the Devin entry visible whether Spettro is running on
// the fallback list or on the latest models.dev catalog.
func mergeAlwaysIncluded(in []Model) []Model {
	if len(alwaysIncludedModels) == 0 {
		return in
	}
	existing := make(map[string]struct{}, len(in))
	for _, m := range in {
		existing[m.Provider] = struct{}{}
	}
	out := append([]Model(nil), in...)
	for _, m := range alwaysIncludedModels {
		if _, ok := existing[m.Provider]; ok {
			continue
		}
		out = append(out, m)
	}
	return out
}

func buildModels(cat models.Catalog) []Model {
	providerIDs := make([]string, 0, len(cat))
	for id := range cat {
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
		prov := cat[pid]
		modelIDs := make([]string, 0, len(prov.Models))
		for id, mod := range prov.Models {
			if mod.Status != "deprecated" {
				modelIDs = append(modelIDs, id)
			}
		}
		sort.Strings(modelIDs)

		envKey := ""
		if len(prov.Env) > 0 {
			envKey = prov.Env[0]
		}

		for _, mid := range modelIDs {
			mod := prov.Models[mid]
			ctx := 0
			if mod.Limit != nil {
				ctx = mod.Limit.Context
			}
			out = append(out, Model{
				Provider:     pid,
				ProviderName: prov.Name,
				Name:         mid,
				DisplayName:  mod.Name,
				Vision:       mod.SupportsImage(),
				Reasoning:    mod.Reasoning,
				ToolCall:     mod.ToolCall,
				Context:      ctx,
				Status:       mod.Status,
				EnvKey:       envKey,
			})
		}
	}
	return out
}

var knownBaseURLs = map[string]string{
	"groq":         "https://api.groq.com/openai/v1",
	"mistral":      "https://api.mistral.ai/v1",
	"xai":          "https://api.x.ai/v1",
	"x-ai":         "https://api.x.ai/v1",
	"together":     "https://api.together.xyz/v1",
	"togetherai":   "https://api.together.xyz/v1",
	"fireworks":    "https://api.fireworks.ai/inference/v1",
	"fireworks-ai": "https://api.fireworks.ai/inference/v1",
	"openrouter":   "https://openrouter.ai/api/v1",
	"google":       "https://generativelanguage.googleapis.com/v1beta/openai",
	"cohere":       "https://api.cohere.com/compatibility/v1",
	"deepseek":     "https://api.deepseek.com/v1",
	"perplexity":   "https://api.perplexity.ai",
	"zai":          "https://api.zai.ai/v1",
}
