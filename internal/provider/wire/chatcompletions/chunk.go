package chatcompletions

import (
	"bytes"
	"encoding/json"
	"errors"
)

// Chunk is one chat.completion.chunk event, reduced to the fields Spettro
// reads.
type Chunk struct {
	ID      string   `json:"id"`
	Choices []Choice `json:"choices"`
	Usage   Usage    `json:"usage"`
	// Error is set (to the raw JSON value, "null" included) when the event
	// carries an "error" member: the server reports a failure mid-stream.
	Error json.RawMessage `json:"error"`
}

// Choice is one entry of a chunk's "choices" array.
type Choice struct {
	Index        int64  `json:"index"`
	Delta        Delta  `json:"delta"`
	FinishReason string `json:"finish_reason"`
}

// Delta is the incremental content of a choice.
type Delta struct {
	Content string `json:"content"`
	// ReasoningContent is the reasoning of DeepSeek-style reasoning models.
	ReasoningContent string          `json:"reasoning_content"`
	ToolCalls        []ToolCallDelta `json:"tool_calls"`
}

// ToolCallDelta is a fragment of a streamed function call. The first
// fragment of a call carries its id and name; later ones append to its
// arguments.
type ToolCallDelta struct {
	Index    int64         `json:"index"`
	ID       string        `json:"id"`
	Type     string        `json:"type"`
	Function FunctionDelta `json:"function"`
}

// FunctionDelta is the function part of a ToolCallDelta.
type FunctionDelta struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// Usage is the token accounting a chunk may carry (normally the last one,
// requested through stream_options.include_usage).
type Usage struct {
	PromptTokens        int64 `json:"prompt_tokens"`
	CompletionTokens    int64 `json:"completion_tokens"`
	TotalTokens         int64 `json:"total_tokens"`
	PromptTokensDetails struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	CompletionTokensDetails struct {
		ReasoningTokens int64 `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
}

// DecodeChunk decodes one event's data into c, replacing its previous
// contents (the Choices slice's storage is reused).
//
// Like the OpenAI SDK's decoder it is lenient: a field whose JSON type does
// not match (a numeric "index" sent as a string, say) is left zero instead
// of failing the stream. Malformed JSON is still an error.
//
// Well-formed chunks of the usual shape take a hand-written fast path;
// everything else goes through encoding/json.
func DecodeChunk(data []byte, c *Chunk) error {
	if decodeChunkFast(data, c) {
		return nil
	}
	return decodeChunkStd(data, c)
}

// decodeChunkStd is DecodeChunk on encoding/json. It is the reference the
// fast path is checked against (FuzzDecodeChunkFast).
func decodeChunkStd(data []byte, c *Chunk) error {
	choices := c.Choices[:0]
	clear(choices[:cap(choices)])
	*c = Chunk{Choices: choices}
	err := json.Unmarshal(data, c)
	if _, mismatch := errors.AsType[*json.UnmarshalTypeError](err); mismatch {
		return nil
	}
	return err
}

// ErrorMessage renders the value of a mid-stream "error" member the way the
// OpenAI SDK words it after "received error while streaming: ": a string
// value unquoted, null as nothing, anything else as its raw JSON.
func ErrorMessage(raw json.RawMessage) string {
	v := bytes.TrimSpace(raw)
	if len(v) > 0 && v[0] == '"' {
		var s string
		if json.Unmarshal(v, &s) == nil {
			return s
		}
	}
	if string(v) == "null" {
		return ""
	}
	return string(v)
}
