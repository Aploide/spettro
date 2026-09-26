package spettro

import "spettro/internal/provider"

// ProviderModels converts the plan's model list into provider models under
// the Spettro Subscription provider. Every host (TUI, headless, ACP)
// registers plan models through it, so they carry the same flags
// everywhere. A model the list marks reasoning:false gets NoReasoning, so no
// thinking parameter is sent to it; one the list does not flag either way
// keeps the default (the proxy forwards reasoning_effort, and a rejection
// steps down the downgrade ladder).
func ProviderModels(infos []ModelInfo) []provider.Model {
	out := make([]provider.Model, 0, len(infos))
	for _, mi := range infos {
		out = append(out, provider.Model{
			Provider:     ProviderID,
			ProviderName: ProviderName,
			Name:         mi.ID,
			DisplayName:  mi.ID,
			ToolCall:     true,
			Vision:       mi.Vision,
			Reasoning:    mi.Reasoning != nil && *mi.Reasoning,
			NoReasoning:  mi.Reasoning != nil && !*mi.Reasoning,
			Context:      mi.ContextWindow,
		})
	}
	return out
}
