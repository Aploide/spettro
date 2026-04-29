package provider

import "context"

// ThinkingLevel selects how much "extended thinking" / reasoning compute the
// model should spend before answering. Levels are normalized across providers:
//
//   - off       no extended thinking (default for chat-style use)
//   - low       short reasoning budget (e.g. ~2k thinking tokens)
//   - medium    medium reasoning budget (e.g. ~5k thinking tokens)
//   - high      long reasoning budget (e.g. ~16k thinking tokens)
//   - x-high    maximum reasoning budget (e.g. ~32k thinking tokens)
//
// Not every provider supports every level. Adapters map ThinkingLevel to their
// native parameter — for Anthropic Claude Opus/Sonnet that's `thinking.type`
// (enabled vs disabled) plus `thinking.budget_tokens`. Levels the underlying
// model does not support fall back to the closest supported one (e.g. Claude
// only has off and high).
type ThinkingLevel string

const (
	ThinkingOff    ThinkingLevel = "off"
	ThinkingLow    ThinkingLevel = "low"
	ThinkingMedium ThinkingLevel = "medium"
	ThinkingHigh   ThinkingLevel = "high"
	ThinkingXHigh  ThinkingLevel = "x-high"
)

// IsValidThinkingLevel reports whether s is one of the recognised ThinkingLevel
// values. The empty string is treated as ThinkingOff and is also valid.
func IsValidThinkingLevel(s string) bool {
	switch ThinkingLevel(s) {
	case "", ThinkingOff, ThinkingLow, ThinkingMedium, ThinkingHigh, ThinkingXHigh:
		return true
	}
	return false
}

// ThinkingBudgetTokens maps a ThinkingLevel to the budget_tokens value passed
// to providers that take an integer budget (Anthropic). Returns 0 for
// "off"/empty so callers can detect and skip the param entirely.
func ThinkingBudgetTokens(level ThinkingLevel) int {
	switch level {
	case ThinkingLow:
		return 2048
	case ThinkingMedium:
		return 5120
	case ThinkingHigh:
		return 16384
	case ThinkingXHigh:
		return 32768
	}
	return 0
}

type Model struct {
	Provider     string
	ProviderName string
	Name         string
	DisplayName  string
	Vision       bool
	Reasoning    bool
	ToolCall     bool
	Context      int
	Status       string
	EnvKey       string
	Local        bool
}

type ProviderInfo struct {
	ID   string
	Name string
	Env  string
}

type Request struct {
	Prompt      string
	Images      []string
	RequireFast bool
	MaxTokens   int
	// Thinking selects extended-thinking compute. Empty == ThinkingOff.
	Thinking ThinkingLevel
}

type Response struct {
	Content         string
	EstimatedTokens int
	Provider        string
	Model           string
}

type Adapter interface {
	Send(context.Context, string, Request) (Response, error)
}
