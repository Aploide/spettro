package provider

import (
	"regexp"
	"strings"

	"spettro/internal/budget"
)

// DefaultMaxOutputTokens is the output cap sent to Anthropic-protocol models
// whose real limit is unknown. Without an explicit value fantasy's Anthropic
// provider sends max_tokens=4096, which cuts any sizeable file write in half.
const DefaultMaxOutputTokens = 32000

// claudeOpus4Pattern matches the original Claude Opus 4 / 4.1 ids (32k output),
// but not Opus 4.5 and later (64k+).
var claudeOpus4Pattern = regexp.MustCompile(`(?:claude-opus-4|claude-4-opus)(?:[-.@][01](?:$|[^0-9])|-20\d{6}|$|[^-.0-9])`)

// knownOutputLimit returns the documented maximum output tokens of well-known
// model families, or 0 when unknown. It matches on substrings so gateway and
// cloud ids ("anthropic/claude-sonnet-4.5", "us.anthropic.claude-…-v1:0")
// resolve too. Values are conservative: a cap above the model's real limit
// is a hard 400, one below it only shortens the longest replies.
func knownOutputLimit(model string) int {
	id := strings.ToLower(model)
	switch {
	case strings.Contains(id, "claude-3-5-") || strings.Contains(id, "claude-3.5"):
		return 8192
	case strings.Contains(id, "claude-3-7") || strings.Contains(id, "claude-3.7"):
		return 64000
	case strings.Contains(id, "claude-3"):
		return 4096
	case claudeOpus4Pattern.MatchString(id):
		return 32000
	case strings.Contains(id, "claude"):
		return 64000
	case strings.Contains(id, "gpt-4o"):
		return 16384
	case strings.Contains(id, "gpt-4.1"):
		return 32768
	case strings.Contains(id, "gpt-5"):
		return 128000
	case strings.Contains(id, "deepseek-reasoner"):
		return 64000
	case strings.Contains(id, "deepseek-chat"):
		return 8192
	}
	base := id
	if i := strings.LastIndex(base, "/"); i >= 0 {
		base = base[i+1:]
	}
	for _, p := range []string{"o1", "o3", "o4-mini"} {
		if base == p || strings.HasPrefix(base, p+"-") {
			return 100000
		}
	}
	return 0
}

// MaxOutputTokens returns the model's maximum output tokens when known: the
// catalog's value first, then the built-in table of well-known families.
// 0 means unknown.
func (m *Manager) MaxOutputTokens(providerName, modelName string) int {
	for _, item := range m.Models() {
		if item.Provider == providerName && item.Name == modelName && item.MaxOutput > 0 {
			return item.MaxOutput
		}
	}
	return knownOutputLimit(modelName)
}

// resolveMaxOutput decides the output cap to send. An explicit request value
// wins but is clamped to the model's known limit (a cap above it is a hard
// 400). With no explicit value the known limit is sent; if the limit is
// unknown, Anthropic-protocol models get DefaultMaxOutputTokens (their
// implicit default is a crippling 4096), while OpenAI-style backends send
// nothing and keep the server default, which is normally the model maximum —
// guessing there risks a 400 on servers with smaller limits.
func (m *Manager) resolveMaxOutput(providerName, apiKind, modelName string, requested int) int {
	limit := m.MaxOutputTokens(providerName, modelName)
	if requested > 0 {
		if limit > 0 && requested > limit {
			return limit
		}
		return requested
	}
	if limit > 0 {
		return limit
	}
	if isAnthropicAPI(providerName, apiKind) {
		return DefaultMaxOutputTokens
	}
	return 0
}

// EstimateRequestTokens approximates the prompt tokens a request occupies:
// system prompt, every message's text, reasoning, tool calls and tool
// results, plus the tool definitions (a 40-tool schema list is several
// thousand tokens that ride on every request). chars/4 heuristic; callers
// that have provider-reported usage should prefer it (see the agent loop's
// calibration).
func EstimateRequestTokens(req Request) int {
	parts := make([]string, 0, 2+len(req.Messages)*2)
	parts = append(parts, req.System, req.Prompt)
	for _, m := range req.Messages {
		parts = append(parts, m.Content)
		for _, r := range m.Reasoning {
			parts = append(parts, r.Text)
		}
		for _, tc := range m.ToolCalls {
			parts = append(parts, tc.Name, string(tc.Args))
		}
		for _, tr := range m.ToolResults {
			parts = append(parts, tr.Output)
		}
	}
	return budget.EstimateTokens(parts...) + EstimateToolTokens(req.Tools)
}

// EstimateToolTokens approximates the prompt tokens taken by tool
// definitions (name, description and JSON schema of each).
func EstimateToolTokens(tools []ToolSpec) int {
	if len(tools) == 0 {
		return 0
	}
	parts := make([]string, 0, len(tools)*3)
	for _, t := range tools {
		parts = append(parts, t.Name, t.Description, string(t.Schema))
	}
	return budget.EstimateTokens(parts...)
}
