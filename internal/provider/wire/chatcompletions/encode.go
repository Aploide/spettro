// Package chatcompletions is Spettro's own client-side wire format for the
// OpenAI chat completions API (POST {base}/chat/completions with
// "stream": true), as spoken by OpenAI-compatible servers, local model
// servers and the Spettro Subscription proxy.
//
// It holds the low-level pieces only: JSON appenders for the request body,
// a Server-Sent Events reader, the lenient chunk decoder and the tool
// argument completion tracker. The provider package decides what to send
// (it mirrors the fantasy SDK's request shape, which golden tests pin) and
// what the decoded events mean.
//
// Everything here exists for speed: the SDK path re-encodes the whole
// conversation through reflection on every step and re-validates streamed
// tool arguments on every fragment (quadratic in the argument size). See
// the benchmarks in this package and in internal/provider.
package chatcompletions

import (
	"strconv"
	"unicode/utf8"
)

const hexDigits = "0123456789abcdef"

// AppendString appends s to b as a JSON string literal.
//
// The output decodes to the same value encoding/json would produce for s:
// invalid UTF-8 becomes U+FFFD, control characters are escaped, and U+2028
// and U+2029 are escaped for the benefit of JavaScript-based servers.
// Unlike encoding/json it does not escape <, > and &, which JSON does not
// require. It allocates only when b must grow.
func AppendString(b []byte, s string) []byte {
	b = append(b, '"')
	start := 0
	for i := 0; i < len(s); {
		c := s[i]
		if c < utf8.RuneSelf {
			if c >= 0x20 && c != '"' && c != '\\' {
				i++
				continue
			}
			b = append(b, s[start:i]...)
			b = appendEscapedASCII(b, c)
			i++
			start = i
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && size == 1:
			b = append(b, s[start:i]...)
			b = append(b, `�`...)
		case r == ' ' || r == ' ':
			b = append(b, s[start:i]...)
			b = append(b, `\u202`...)
			b = append(b, hexDigits[r&0xf])
		default:
			i += size
			continue
		}
		i += size
		start = i
	}
	b = append(b, s[start:]...)
	return append(b, '"')
}

// appendEscapedASCII appends the JSON escape of an ASCII byte that cannot
// appear raw inside a JSON string.
func appendEscapedASCII(b []byte, c byte) []byte {
	switch c {
	case '"', '\\':
		return append(b, '\\', c)
	case '\n':
		return append(b, '\\', 'n')
	case '\r':
		return append(b, '\\', 'r')
	case '\t':
		return append(b, '\\', 't')
	}
	return append(b, '\\', 'u', '0', '0', hexDigits[c>>4], hexDigits[c&0xf])
}

// AppendSystemMessage appends {"role":"system","content":text}.
func AppendSystemMessage(b []byte, text string) []byte {
	b = append(b, `{"role":"system","content":`...)
	b = AppendString(b, text)
	return append(b, '}')
}

// AppendUserText appends a plain-text user message.
func AppendUserText(b []byte, text string) []byte {
	b = append(b, `{"role":"user","content":`...)
	b = AppendString(b, text)
	return append(b, '}')
}

// AppendUserParts appends a user message whose content is an array: the
// text part first, then one image_url part per data URL.
func AppendUserParts(b []byte, text string, imageURLs []string) []byte {
	b = append(b, `{"role":"user","content":[{"type":"text","text":`...)
	b = AppendString(b, text)
	b = append(b, '}')
	for _, u := range imageURLs {
		b = append(b, `,{"type":"image_url","image_url":{"url":`...)
		b = AppendString(b, u)
		b = append(b, `}}`...)
	}
	return append(b, `]}`...)
}

// AppendToolResult appends one tool message answering the call callID.
func AppendToolResult(b []byte, callID, content string) []byte {
	b = append(b, `{"role":"tool","tool_call_id":`...)
	b = AppendString(b, callID)
	b = append(b, `,"content":`...)
	b = AppendString(b, content)
	return append(b, '}')
}

// AppendAssistantText appends an assistant message that is plain text.
func AppendAssistantText(b []byte, text string) []byte {
	b = append(b, `{"role":"assistant","content":`...)
	b = AppendString(b, text)
	return append(b, '}')
}

// ToolCall is one function call replayed on an assistant message.
type ToolCall struct {
	ID        string
	Name      string
	Arguments string
}

// Assistant is an assistant message with structured parts: optional text,
// optional reasoning_content (the replayed reasoning of OpenAI-compatible
// reasoning models) and function calls.
type Assistant struct {
	// Content is sent when HasContent is set.
	Content    string
	HasContent bool
	// ReasoningContent is sent when non-empty.
	ReasoningContent string
	ToolCalls        []ToolCall
}

// AppendAssistant appends a structured assistant message.
func AppendAssistant(b []byte, m Assistant) []byte {
	b = append(b, `{"role":"assistant"`...)
	if m.HasContent {
		b = append(b, `,"content":`...)
		b = AppendString(b, m.Content)
	}
	if len(m.ToolCalls) > 0 {
		b = append(b, `,"tool_calls":[`...)
		for i, tc := range m.ToolCalls {
			if i > 0 {
				b = append(b, ',')
			}
			b = append(b, `{"id":`...)
			b = AppendString(b, tc.ID)
			b = append(b, `,"type":"function","function":{"name":`...)
			b = AppendString(b, tc.Name)
			b = append(b, `,"arguments":`...)
			b = AppendString(b, tc.Arguments)
			b = append(b, `}}`...)
		}
		b = append(b, ']')
	}
	if m.ReasoningContent != "" {
		b = append(b, `,"reasoning_content":`...)
		b = AppendString(b, m.ReasoningContent)
	}
	return append(b, '}')
}

// AppendFunctionTool appends one entry of the "tools" array. parameters
// must be a JSON object; it is copied verbatim.
func AppendFunctionTool(b []byte, name, description string, parameters []byte) []byte {
	b = append(b, `{"type":"function","function":{"name":`...)
	b = AppendString(b, name)
	b = append(b, `,"description":`...)
	b = AppendString(b, description)
	b = append(b, `,"parameters":`...)
	b = append(b, parameters...)
	return append(b, `,"strict":false}}`...)
}

// Options are the request fields outside the message and tool arrays.
type Options struct {
	Model string
	// MaxTokens is the output cap; 0 sends none. MaxTokensField names the
	// field it travels in: "max_tokens", or "max_completion_tokens" for the
	// OpenAI reasoning model families that reject max_tokens.
	MaxTokens      int64
	MaxTokensField string
	// ReasoningEffort is sent as reasoning_effort when non-empty.
	ReasoningEffort string
}

// AppendRequestHead appends the start of a streaming request body, up to
// and including the opening bracket of the "messages" array.
func AppendRequestHead(b []byte, o Options) []byte {
	b = append(b, `{"model":`...)
	b = AppendString(b, o.Model)
	b = append(b, `,"stream":true,"stream_options":{"include_usage":true}`...)
	if o.MaxTokens > 0 {
		b = append(b, ',')
		b = AppendString(b, o.MaxTokensField)
		b = append(b, ':')
		b = strconv.AppendInt(b, o.MaxTokens, 10)
	}
	if o.ReasoningEffort != "" {
		b = append(b, `,"reasoning_effort":`...)
		b = AppendString(b, o.ReasoningEffort)
	}
	return append(b, `,"messages":[`...)
}

// MessagesEnd closes the "messages" array of a request without tools and
// the request object itself.
const MessagesEnd = "]}"

// ToolsStart closes the "messages" array and opens the "tools" array.
const ToolsStart = `],"tools":[`

// ToolsEnd closes the "tools" array, selects automatic tool choice and
// closes the request object.
const ToolsEnd = `],"tool_choice":"auto"}`
