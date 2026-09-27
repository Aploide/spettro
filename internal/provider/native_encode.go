package provider

import (
	"bytes"
	"slices"
	"strconv"
	"strings"
	"sync"

	wire "spettro/internal/provider/wire/chatcompletions"
)

// toolImageNote is the text of the user turn that carries images produced
// by tools to providers whose tool results are text-only.
const toolImageNote = "[image attached from the tool result above]"

// defaultToolSchema replaces a tool schema that is not a JSON object.
const defaultToolSchema = `{"additionalProperties":true,"type":"object"}`

// Encoder cache bounds: conversations encoded at once (the main agent plus
// concurrent sub-agents) and distinct tool surfaces kept.
const (
	maxEncoderLanes = 4
	maxToolSurfaces = 4
)

// chatEncoder builds chat-completions request bodies for the native wire
// client. The body carries the same JSON as the fantasy SDK sends for the
// same Request (TestNativeBodyMatchesFantasy pins this), but a step of a
// long agent run re-encodes only what changed.
//
// It caches, per conversation ("lane"), the encoding of each message by
// its position, together with a snapshot of the message it was built
// from. A cached encoding is reused when the message at that position
// still equals its snapshot. The comparison is cheap because a carried
// history shares its strings from step to step: Go compares two strings
// that share their bytes without reading them. Anything else (compaction
// rewriting the history, a switch of model, an edited message) fails the
// comparison and that message is encoded again.
//
// Key: lane = the conversation's first message plus the provider and model
// (reasoning replay depends on them); entry = position within the lane.
// Invalidation: by comparison on every use, as above; lanes are recycled
// least recently used first. Messages with images are never cached: their
// bytes come from the media cache, which checks the files on every use.
// Owner: shared by the Manager's goroutines; mu guards all of it. Cached
// encodings are never modified once stored, so request bodies keep
// pointing at them after mu is released.
type chatEncoder struct {
	mu    sync.Mutex
	lanes []*encoderLane // most recently used first
	tools []*toolSurface // most recently used first
}

// encoderLane is the cache of one conversation.
type encoderLane struct {
	provider, model string
	// first is a snapshot of the conversation's first message, which
	// identifies the lane.
	first     Message
	system    string
	systemEnc []byte
	entries   []encodedMessage
}

// encodedMessage is one message's cached encoding: zero or more JSON
// message objects, each preceded by a comma. msg is a snapshot of the
// source message (see snapshotMessage). An entry that is not cached (the
// message has images) has cached unset.
type encodedMessage struct {
	msg    Message
	enc    []byte
	cached bool
}

// toolSurface is one cached "tools" array body (the objects, comma
// separated, without the brackets).
type toolSurface struct {
	specs []ToolSpec
	enc   []byte
}

// encode returns the request body for req. providerName and modelName
// select the reasoning blocks that are replayed.
func (e *chatEncoder) encode(providerName, modelName string, req Request) *wire.Body {
	body := &wire.Body{}
	// Head, system prompt, one chunk per message, the tool array's three.
	body.Grow(len(req.Messages) + 6)
	body.Append(wire.AppendRequestHead(nil, chatOptions(modelName, req)))
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(req.Messages) == 0 {
		msg := Message{Role: RoleUser, Content: req.Prompt}
		body.Append(appendChatMessage(nil, providerName, modelName, msg, req.Images)[1:])
	} else {
		e.appendMessages(body, providerName, modelName, req)
	}
	if len(req.Tools) == 0 {
		body.AppendString(wire.MessagesEnd)
		return body
	}
	body.AppendString(wire.ToolsStart)
	body.Append(e.toolsEncoding(req.Tools))
	body.AppendString(wire.ToolsEnd)
	return body
}

// chatOptions returns the request fields outside the arrays, as fantasy's
// OpenAI-compatible provider sets them.
func chatOptions(modelName string, req Request) wire.Options {
	o := wire.Options{Model: modelName, ReasoningEffort: ReasoningEffort(req.Thinking)}
	if req.MaxTokens > 0 {
		o.MaxTokens = int64(req.MaxTokens)
		o.MaxTokensField = "max_tokens"
		if isOpenAIReasoningModel(modelName) {
			o.MaxTokensField = "max_completion_tokens"
		}
	}
	return o
}

// isOpenAIReasoningModel mirrors the fantasy SDK's test for the OpenAI
// reasoning model families, which take max_completion_tokens instead of
// max_tokens.
func isOpenAIReasoningModel(modelID string) bool {
	return strings.HasPrefix(modelID, "o1") || strings.Contains(modelID, "-o1") ||
		strings.HasPrefix(modelID, "o3") || strings.Contains(modelID, "-o3") ||
		strings.HasPrefix(modelID, "o4") || strings.Contains(modelID, "-o4") ||
		strings.HasPrefix(modelID, "oss") || strings.Contains(modelID, "-oss") ||
		strings.Contains(modelID, "gpt-5")
}

// appendMessages adds the system prompt and every message of req to body,
// from the lane cache where possible. The caller holds e.mu.
func (e *chatEncoder) appendMessages(body *wire.Body, providerName, modelName string, req Request) {
	lane := e.lane(providerName, modelName, req.Messages[0])
	first := true
	add := func(enc []byte) {
		if len(enc) == 0 {
			return
		}
		if first {
			enc = enc[1:] // drop the leading comma
			first = false
		}
		body.Append(enc)
	}
	if strings.TrimSpace(req.System) != "" {
		if lane.systemEnc == nil || lane.system != req.System {
			lane.system = req.System
			lane.systemEnc = wire.AppendSystemMessage([]byte{','}, req.System)
		}
		add(lane.systemEnc)
	}
	if n := len(req.Messages); len(lane.entries) != n {
		if len(lane.entries) > n {
			clear(lane.entries[n:])
		}
		lane.entries = slices.Grow(lane.entries[:min(len(lane.entries), n)], n)[:n]
	}
	imageIdx := lastUserIndex(req.Messages)
	for i, msg := range req.Messages {
		entry := &lane.entries[i]
		var extra []string
		if i == imageIdx {
			extra = req.Images
		}
		if len(extra) > 0 || hasImages(msg) {
			*entry = encodedMessage{}
			add(appendChatMessage(nil, providerName, modelName, msg, extra))
			continue
		}
		if !entry.cached || !sameWireMessage(entry.msg, msg) {
			enc := appendChatMessage(nil, providerName, modelName, msg, nil)
			*entry = encodedMessage{msg: snapshotMessage(msg), enc: enc, cached: true}
		}
		add(entry.enc)
	}
}

// lane returns the cache lane of the conversation that starts with first,
// moving it to the front, or recycles the least recently used lane for it.
// The caller holds e.mu.
func (e *chatEncoder) lane(providerName, modelName string, first Message) *encoderLane {
	for i, l := range e.lanes {
		if l.provider != providerName || l.model != modelName || !sameWireMessage(l.first, first) {
			continue
		}
		copy(e.lanes[1:i+1], e.lanes[:i])
		e.lanes[0] = l
		return l
	}
	l := &encoderLane{provider: providerName, model: modelName, first: snapshotMessage(first)}
	if len(e.lanes) < maxEncoderLanes {
		e.lanes = append(e.lanes, nil)
	}
	copy(e.lanes[1:], e.lanes[:len(e.lanes)-1])
	e.lanes[0] = l
	return l
}

// toolsEncoding returns the encoded tool definitions. The caller holds
// e.mu.
func (e *chatEncoder) toolsEncoding(specs []ToolSpec) []byte {
	for i, s := range e.tools {
		if sameToolSpecs(s.specs, specs) {
			copy(e.tools[1:i+1], e.tools[:i])
			e.tools[0] = s
			return s.enc
		}
	}
	var enc []byte
	for i, t := range specs {
		if i > 0 {
			enc = append(enc, ',')
		}
		schema := []byte(t.Schema)
		if toolSchemas.parse(t.Schema) == nil {
			schema = []byte(defaultToolSchema)
		}
		enc = wire.AppendFunctionTool(enc, t.Name, t.Description, schema)
	}
	surface := &toolSurface{specs: cloneToolSpecs(specs), enc: enc}
	if len(e.tools) < maxToolSurfaces {
		e.tools = append(e.tools, nil)
	}
	copy(e.tools[1:], e.tools[:len(e.tools)-1])
	e.tools[0] = surface
	return enc
}

// appendChatMessage appends msg as chat-completions message objects, each
// preceded by a comma, the way fantasy's OpenAI-compatible provider
// converts the equivalent fantasy prompt (see buildFantasyCall):
//
//   - a tool-results turn becomes one "tool" message per result, then a user
//     message with the turn's text if any, then a user message carrying the
//     results' images;
//   - a user turn is plain text, or a text part plus image parts when images
//     load (extraImages are the request-level images of the current turn);
//   - an assistant turn is plain text when that is all it has; otherwise it
//     carries its text, its tool calls and the reasoning replayed for this
//     model as reasoning_content (the last block's text), and is dropped
//     when it has neither text nor tool calls.
func appendChatMessage(b []byte, providerName, modelName string, msg Message, extraImages []string) []byte {
	switch msg.Role {
	case RoleUser:
		if len(msg.ToolResults) > 0 {
			var spill []string
			for _, tr := range msg.ToolResults {
				b = append(b, ',')
				b = wire.AppendToolResult(b, tr.ID, tr.Output)
				spill = append(spill, tr.Images...)
			}
			if msg.Content != "" {
				b = append(b, ',')
				b = wire.AppendUserText(b, msg.Content)
			}
			if urls := imageDataURLs(spill); len(urls) > 0 {
				b = append(b, ',')
				b = wire.AppendUserParts(b, toolImageNote, urls)
			}
			return b
		}
		images := msg.Images
		if len(extraImages) > 0 {
			images = append(images[:len(images):len(images)], extraImages...)
		}
		b = append(b, ',')
		if urls := imageDataURLs(images); len(urls) > 0 {
			return wire.AppendUserParts(b, msg.Content, urls)
		}
		return wire.AppendUserText(b, msg.Content)
	case RoleAssistant:
		return appendAssistantMessage(b, providerName, modelName, msg)
	}
	return b
}

// appendAssistantMessage appends an assistant turn (see appendChatMessage).
func appendAssistantMessage(b []byte, providerName, modelName string, msg Message) []byte {
	var reasoning string
	replayed := 0
	for _, r := range msg.Reasoning {
		if r.Provider == providerName && r.Model == modelName && r.Text != "" {
			reasoning = r.Text
			replayed++
		}
	}
	if replayed == 0 && len(msg.ToolCalls) == 0 {
		// Plain text, possibly empty.
		b = append(b, ',')
		return wire.AppendAssistantText(b, msg.Content)
	}
	if msg.Content == "" && len(msg.ToolCalls) == 0 {
		// Reasoning alone: fantasy drops the turn.
		return b
	}
	m := wire.Assistant{Content: msg.Content, HasContent: msg.Content != "", ReasoningContent: reasoning}
	if len(msg.ToolCalls) > 0 {
		m.ToolCalls = make([]wire.ToolCall, len(msg.ToolCalls))
		for i, tc := range msg.ToolCalls {
			args := string(tc.Args)
			if args == "" {
				args = "{}"
			}
			m.ToolCalls[i] = wire.ToolCall{ID: tc.ID, Name: tc.Name, Arguments: args}
		}
	}
	b = append(b, ',')
	return wire.AppendAssistant(b, m)
}

// imageDataURLs loads images as data URLs, skipping unreadable files.
func imageDataURLs(paths []string) []string {
	var urls []string
	for _, p := range paths {
		if url, _, ok := requestMedia.dataURL(p); ok {
			urls = append(urls, url)
		}
	}
	return urls
}

// hasImages reports whether msg carries images of its own or from tools.
func hasImages(msg Message) bool {
	if len(msg.Images) > 0 {
		return true
	}
	for _, tr := range msg.ToolResults {
		if len(tr.Images) > 0 {
			return true
		}
	}
	return false
}

// snapshotMessage copies the parts of msg its encoding depends on, so a
// later in-place change to the caller's slices cannot go unnoticed by
// sameWireMessage. Strings are immutable and are shared, not copied.
func snapshotMessage(msg Message) Message {
	snap := Message{Role: msg.Role, Content: msg.Content}
	if len(msg.Reasoning) > 0 {
		snap.Reasoning = make([]ReasoningBlock, len(msg.Reasoning))
		for i, r := range msg.Reasoning {
			snap.Reasoning[i] = ReasoningBlock{Text: r.Text, Provider: r.Provider, Model: r.Model}
		}
	}
	if len(msg.ToolCalls) > 0 {
		snap.ToolCalls = make([]NativeTool, len(msg.ToolCalls))
		for i, tc := range msg.ToolCalls {
			snap.ToolCalls[i] = NativeTool{ID: tc.ID, Name: tc.Name, Args: bytes.Clone(tc.Args)}
		}
	}
	if len(msg.ToolResults) > 0 {
		snap.ToolResults = make([]ToolResult, len(msg.ToolResults))
		for i, tr := range msg.ToolResults {
			snap.ToolResults[i] = ToolResult{ID: tr.ID, Output: tr.Output}
		}
	}
	return snap
}

// sameWireMessage reports whether msg encodes like the snapshot snap: the
// fields the chat-completions encoding reads are equal. Image paths are not
// compared: messages with images are never cached (and a lane's first
// message only identifies the lane, it does not vouch for any encoding).
func sameWireMessage(snap, msg Message) bool {
	if snap.Role != msg.Role || snap.Content != msg.Content ||
		len(snap.Reasoning) != len(msg.Reasoning) ||
		len(snap.ToolCalls) != len(msg.ToolCalls) ||
		len(snap.ToolResults) != len(msg.ToolResults) {
		return false
	}
	for i, r := range msg.Reasoning {
		s := snap.Reasoning[i]
		if s.Text != r.Text || s.Provider != r.Provider || s.Model != r.Model {
			return false
		}
	}
	for i, tc := range msg.ToolCalls {
		s := snap.ToolCalls[i]
		if s.ID != tc.ID || s.Name != tc.Name || !bytes.Equal(s.Args, tc.Args) {
			return false
		}
	}
	for i, tr := range msg.ToolResults {
		s := snap.ToolResults[i]
		if s.ID != tr.ID || s.Output != tr.Output {
			return false
		}
	}
	return true
}

func sameToolSpecs(a, b []ToolSpec) bool {
	return slices.EqualFunc(a, b, func(x, y ToolSpec) bool {
		return x.Name == y.Name && x.Description == y.Description && bytes.Equal(x.Schema, y.Schema)
	})
}

func cloneToolSpecs(specs []ToolSpec) []ToolSpec {
	out := make([]ToolSpec, len(specs))
	for i, t := range specs {
		out[i] = ToolSpec{Name: t.Name, Description: t.Description, Schema: bytes.Clone(t.Schema)}
	}
	return out
}

// toolCallIDForIndex is the ID fantasy gives a streamed tool call that
// arrives without one.
func toolCallIDForIndex(index int64) string {
	return "tool-call-" + strconv.FormatInt(index, 10)
}
