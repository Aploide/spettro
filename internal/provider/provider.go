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
// value used by OpenAI and OpenAI-compatible backends. An explicit "off"
// sends "none" so models that reason by default actually stop reasoning;
// servers that reject the value trigger the downgrade ladder, which retries
// with the field omitted. Empty (never set) returns "" so callers skip the
// parameter and keep the provider's default. x-high and max both map to
// "xhigh" — effort is an enum, not a token budget, and "xhigh" is the
// highest value the wire format defines.
func ReasoningEffort(level ThinkingLevel) string {
	switch level {
	case ThinkingOff:
		return "none"
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
	Provider     string
	ProviderName string
	Name         string
	DisplayName  string
	Vision       bool
	Reasoning    bool
	// NoReasoning marks a model its source explicitly lists as
	// non-reasoning where Reasoning alone would not decide it (the Spettro
	// plan's reasoning:false; see Manager.SupportsReasoning).
	NoReasoning   bool
	ToolCall      bool
	PromptCaching bool
	Context       int
	Status        string
	EnvKey        string
	Local         bool
	// MaxOutput is the model's maximum output tokens (0 = unknown; see
	// Manager.MaxOutputTokens for the built-in fallback table).
	MaxOutput int
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
	// ArgsError is set when the model's raw arguments could not be decoded
	// (cut off at the output token limit, or malformed JSON that the repair
	// pass could not fix). Args is then "{}" so the call still replays as a
	// valid history entry; the tool runtime must not execute the call and
	// instead feeds ArgsError back to the model as the tool's error result.
	ArgsError string `json:"args_error,omitempty"`
	// RawArgs keeps the model's argument text when ArgsError is set, so a
	// caller can still tell two failed calls apart (Args is "{}" for both).
	// It is not persisted or sent back to a provider.
	RawArgs string `json:"-"`
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
	// SpoolID references the full output persisted to the session spool
	// ("spool:<n>") when the output exceeded the offload floor at execution
	// time. Compaction uses it to replace the in-context output with a short
	// stub the model can re-read via the tool-output tool. Never sent to
	// providers (adapters build their wire payloads field by field).
	SpoolID string `json:"spool_id,omitempty"`
}

// ReasoningBlock is one reasoning/thinking segment produced by a model turn.
// It is stored on the assistant message so it can be replayed on the next
// request where the provider requires (Anthropic extended thinking with tool
// use needs the signed thinking block of the in-progress turn) or benefits
// from it (OpenAI-compatible reasoning_content round-trip). Provider/Model
// record who produced it: signatures are only valid for the model that
// signed them, so adapters replay a block only to that same model.
type ReasoningBlock struct {
	Text string `json:"text,omitempty"`
	// Signature is Anthropic's opaque thinking signature.
	Signature string `json:"signature,omitempty"`
	// RedactedData is Anthropic's encrypted redacted_thinking payload.
	RedactedData string `json:"redacted_data,omitempty"`
	Provider     string `json:"provider,omitempty"`
	Model        string `json:"model,omitempty"`
}

// Message is one turn in a structured conversation.
type Message struct {
	Role    Role
	Content string
	// Reasoning is set on assistant turns whose response carried reasoning /
	// thinking content (see ReasoningBlock).
	Reasoning []ReasoningBlock `json:",omitempty"`
	// ToolCalls is set on assistant turns that issued native tool calls.
	ToolCalls []NativeTool
	// ToolResults is set on user turns that return native tool results.
	ToolResults []ToolResult
	// Images holds file paths of images attached to this user turn. Keeping
	// them on the message (rather than only request-level) means they are
	// re-sent with every step of a tool loop and survive into carried history,
	// so the model still sees them when composing its final answer.
	Images []string
	// FileStamps records, on a tool-results turn, the file content hashes the
	// agent's file tools saw during that step (the stale-read guard's state),
	// so a later run carrying this history keeps enforcing it. Never sent to
	// a provider.
	FileStamps []FileStamp `json:",omitempty"`
	// SessionContext, on a conversation's first message, is the environment
	// and project-instructions snapshot taken when the conversation started
	// (the tail of its system prompt). Carrying it with the conversation keeps
	// the system prompt byte-stable across turns without sharing it between
	// conversations. Never sent to a provider as message content.
	SessionContext string `json:",omitempty"`
	// LoadedTools, on a conversation's first message, lists the deferred
	// tools the conversation has loaded (through tool-search or a call by
	// name), so a later turn advertises the same tool list even after
	// compaction summarized away the calls that loaded them. Never sent to
	// a provider.
	LoadedTools []string `json:",omitempty"`
}

// FileStamp is one file's stale-read guard state: the SHA-256 (hex) of the
// content the agent last saw in full (Seen) and last saw through file-read,
// with line numbers (Read). Shell says Seen was set by the agent's own
// shell command rather than shown to the model, so overwriting the file
// needs a file-read first; a record with Seen set is the path's whole
// state, so a false Shell there clears an earlier mark. Path is the file's
// real absolute path.
type FileStamp struct {
	Path  string `json:"path"`
	Seen  string `json:"seen,omitempty"`
	Read  string `json:"read,omitempty"`
	Shell bool   `json:"shell,omitempty"`
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
	// MaxTokens caps the OUTPUT of this request (max_tokens /
	// max_output_tokens on the wire). 0 means "auto": the manager sends a
	// sensible per-model default (see DefaultMaxOutputTokens) so providers
	// with a tiny implicit default (Anthropic: 4096) don't truncate tool calls.
	MaxTokens int
	// PromptTokens, when positive, is the caller's own estimate of this
	// request's prompt (EstimateRequestTokens of the same request): Send
	// uses it for the input budget and the output cap instead of estimating
	// again. 0 means Send estimates. Send ignores it when it has to change
	// the request first (images stripped for a model without vision). The
	// agent run loop sets it from its incremental estimate (promptSizer),
	// which saves a walk over the whole history per step.
	PromptTokens int
	// InputBudget is the user's per-request INPUT token budget
	// (config token_budget). Requests whose estimated prompt is at or above
	// it are refused locally before any network call. 0 disables the check.
	// It is deliberately separate from MaxTokens: one number cannot be both
	// an output cap and a prompt-size limit.
	InputBudget int
	// ContextWindow is the model's context window in tokens when the caller
	// knows it better than the catalog (a configured window, or one learned
	// from an overflow error). 0 → the catalog / local probe value. It only
	// bounds the output cap: prompt + max_tokens must fit the window.
	ContextWindow int
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
	// StreamIdleTimeout bounds the silence on a streamed response (keep-alives
	// count as activity); a stream that goes quiet longer fails with
	// ErrStreamIdle (retryable) so a stalled connection cannot hang the run
	// forever. 0 → defaults (see DefaultStreamIdleTimeout: longer before the
	// first chunk, for reasoning, and for local servers).
	StreamIdleTimeout time.Duration

	// localEndpoint is set by the manager for local model servers, whose
	// first token may take minutes of prompt processing (see streamTimeouts).
	localEndpoint bool
}

// FinishReason is why the model stopped generating, normalized across
// providers. The zero value means the backend did not report one.
type FinishReason string

const (
	FinishStop          FinishReason = "stop"
	FinishLength        FinishReason = "length" // hit the output token limit
	FinishToolCalls     FinishReason = "tool-calls"
	FinishContentFilter FinishReason = "content-filter"
	FinishError         FinishReason = "error"
	FinishOther         FinishReason = "other"
)

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
	// FinishReason is why generation stopped. FinishLength means the reply
	// was cut at the output token limit: text is incomplete and any tool call
	// whose arguments were still streaming carries an ArgsError.
	FinishReason FinishReason
	// Reasoning holds the reasoning/thinking blocks of this reply (with
	// Anthropic signatures), stamped with the producing provider/model.
	// Callers store it on the assistant message so it is replayed next step.
	Reasoning []ReasoningBlock
	// Thinking is the thinking level the request finally succeeded with. It
	// differs from Request.Thinking when the manager stepped the level down
	// because the model rejected it; callers should reuse it for later
	// requests instead of paying the rejected attempt again every step.
	Thinking ThinkingLevel
	// MaxOutputTokens is the output cap actually sent (0 when none was).
	MaxOutputTokens int
	// Diagnostics describes the raw reply before normalization, for debug
	// logs only; see ResponseDiagnostics.
	Diagnostics ResponseDiagnostics
}

// ResponseDiagnostics describes a reply as the provider SDK delivered it,
// before finish-reason mapping and tool-call finalization. Nothing acts on
// it: the agent loop only logs it at debug level with every reply, so that a
// tool call lost between the provider and the loop (a reply that says it
// stopped for tool calls yet carries none) can be confirmed after the fact.
// Only the native tool-calling backends fill it in; elsewhere it is zero.
type ResponseDiagnostics struct {
	// RawFinishReason is the finish reason the SDK reported, before
	// FinishReason normalized it and the truncation checks rewrote it.
	RawFinishReason string
	// ToolCallsSeen counts the tool calls the reply introduced, finished or
	// not, before finalization dropped any.
	ToolCallsSeen int
	// UnnamedToolCalls counts introduced calls that never received a tool
	// name; finalization drops them because there is nothing to run.
	UnnamedToolCalls int
	// OrphanToolDeltas counts streamed argument fragments whose call id the
	// stream never introduced; they are ignored.
	OrphanToolDeltas int
}

// Truncated reports whether the reply was cut at the output token limit.
func (r Response) Truncated() bool { return r.FinishReason == FinishLength }

type Adapter interface {
	Send(context.Context, string, Request) (Response, error)
}
