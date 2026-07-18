package provider

import (
	"context"
	"encoding/json"
	"time"
)

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
// (enabled vs disabled) plus `thinking.budget_tokens`; for OpenAI and
// OpenAI-compatible backends (Groq, xAI, DeepSeek, Google's compat endpoint,
// local servers, …) it's `reasoning_effort`. Levels the underlying model does
// not support fall back to the closest supported one (e.g. Claude only has
// off and high).
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

// ReasoningEffort maps a ThinkingLevel to the OpenAI-style `reasoning_effort`
// value used by OpenAI and OpenAI-compatible backends. Returns "" for
// off/empty so callers can skip the parameter entirely: most reasoning models
// cannot fully disable reasoning, so omitting the field keeps the provider's
// default instead of sending a value the model may reject. x-high and max
// both map to "xhigh" — effort is an enum, not a token budget, and "xhigh" is
// the highest value the wire format defines.
func ReasoningEffort(level ThinkingLevel) string {
	switch level {
	case ThinkingLow:
		return "low"
	case ThinkingMedium:
		return "medium"
	case ThinkingHigh:
		return "high"
	case ThinkingXHigh, ThinkingMax:
		return "xhigh"
	}
	return ""
}

// NextLowerThinking returns the next level down the ladder, ending at "" (no
// thinking parameter sent at all). Used to degrade gracefully when a model
// rejects the requested level instead of surfacing the error to the user.
func NextLowerThinking(level ThinkingLevel) ThinkingLevel {
	switch level {
	case ThinkingMax:
		return ThinkingXHigh
	case ThinkingXHigh:
		return ThinkingHigh
	case ThinkingHigh:
		return ThinkingMedium
	case ThinkingMedium:
		return ThinkingLow
	}
	return ""
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

// ToolSpec is the definition of one tool sent via the native tool-calling API.
type ToolSpec struct {
	Name        string
	Description string
	Schema      json.RawMessage // JSON Schema object for the arguments
}

// NativeTool is a structured tool invocation returned by a capable model.
type NativeTool struct {
	ID   string // provider-assigned call ID
	Name string
	Args json.RawMessage
}

// ToolResult is the executed output of a NativeTool, fed back in the next turn.
type ToolResult struct {
	ID     string
	Name   string
	Output string
	IsErr  bool
	// Images holds file paths of images produced by the tool (screenshot,
	// view-image) that the model should SEE, not just hear about. Anthropic
	// receives them as image blocks inside the tool_result; providers whose
	// tool results are text-only receive them as an immediately following
	// user turn with image parts. Only populated when the active model
	// supports vision (the tool runtime gates attachment).
	Images []string
}

// Message is one turn in a structured conversation.
type Message struct {
	Role    Role
	Content string
	// ToolCalls is set on assistant turns that issued native tool calls.
	ToolCalls []NativeTool
	// ToolResults is set on user turns that return native tool results.
	ToolResults []ToolResult
	// Images holds file paths of images attached to this user turn. Keeping
	// them on the message (rather than only request-level) means they are
	// re-sent with every step of a tool loop and survive into carried history,
	// so the model still sees them when composing its final answer.
	Images []string
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
	// Tools, when non-empty, enables native tool calling for capable backends.
	// On the text-protocol path this field is left nil.
	Tools []ToolSpec
	// OnStream, when non-nil, requests incremental token streaming. The
	// provider invokes it (synchronously, on the calling goroutine) as text and
	// reasoning deltas arrive. Streaming is best-effort: paths that cannot
	// stream still return the full Response and simply never call OnStream.
	OnStream func(StreamEvent)
	// OnRateLimit, when non-nil, is called just before Manager.Send sleeps to
	// honour a provider-issued rate limit (currently: the Spettro Subscription
	// overflow tier's 429/Retry-After) instead of surfacing it as an error.
	OnRateLimit func(time.Duration)
}

type Response struct {
	Content         string
	EstimatedTokens int
	// Usage is the provider-reported token accounting for this request,
	// including prompt-cache reads/writes. Zero-valued when the backend did
	// not report usage (EstimatedTokens then carries a local estimate).
	Usage    Usage
	Provider string
	Model    string
	// ToolCalls is populated on the native tool-calling path.
	ToolCalls []NativeTool
}

type Adapter interface {
	Send(context.Context, string, Request) (Response, error)
}
