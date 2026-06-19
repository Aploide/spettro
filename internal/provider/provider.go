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
	ThinkingMax    ThinkingLevel = "max"
)

// IsValidThinkingLevel reports whether s is one of the recognised ThinkingLevel
// values. The empty string is treated as ThinkingOff and is also valid.
func IsValidThinkingLevel(s string) bool {
	switch ThinkingLevel(s) {
	case "", ThinkingOff, ThinkingLow, ThinkingMedium, ThinkingHigh, ThinkingXHigh, ThinkingMax:
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
	case ThinkingMax:
		return 100000
	}
	return 0
}

type Model struct {
	Provider      string
	ProviderName  string
	Name          string
	DisplayName   string
	Vision        bool
	Reasoning     bool
	ToolCall      bool
	PromptCaching bool
	Context       int
	Status        string
	EnvKey        string
	Local         bool
}

type ProviderInfo struct {
	ID   string
	Name string
	Env  string
}

// StreamEventKind classifies an incremental streaming chunk.
type StreamEventKind string

const (
	// StreamText is a delta of the model's visible answer text.
	StreamText StreamEventKind = "text"
	// StreamReasoning is a delta of the model's extended-thinking / reasoning.
	StreamReasoning StreamEventKind = "reasoning"
)

// StreamEvent is one incremental delta emitted while a response is generated.
type StreamEvent struct {
	Kind  StreamEventKind
	Delta string
}

// Role is the speaker role in a conversation turn.
type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

// Message is one turn in a structured conversation.
type Message struct {
	Role    Role
	Content string
}

type Request struct {
	// System is sent in the provider's dedicated system/developer field. Empty
	// means no system prompt.
	System string
	// Messages holds the ordered conversation turns. When non-empty the adapter
	// uses the provider's native multi-turn format and Prompt is ignored.
	Messages []Message
	// Prompt is the legacy single-blob fallback used when Messages is empty.
	Prompt      string
	Images      []string
	RequireFast bool
	MaxTokens   int
	// Thinking selects extended-thinking compute. Empty == ThinkingOff.
	Thinking ThinkingLevel
	// OnStream, when non-nil, requests incremental token streaming. The
	// provider invokes it (synchronously, on the calling goroutine) as text and
	// reasoning deltas arrive. Streaming is best-effort: paths that cannot
	// stream still return the full Response and simply never call OnStream.
	OnStream func(StreamEvent)
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
