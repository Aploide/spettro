package provider

import (
	"bytes"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	wire "spettro/internal/provider/wire/chatcompletions"
)

// toolImageNote is the text of the user turn that carries images produced
// by tools to providers whose tool results are text-only.
const toolImageNote = "[image attached from the tool result above]"

// defaultToolSchema replaces a tool schema that is not a JSON object.
const defaultToolSchema = `{"additionalProperties":true,"type":"object"}`

// Encoder cache bounds.
const (
	// maxEncoderLanes bounds the conversations cached at once. The main
	// agent and all of its sub-agents share one Manager, so one encoder:
	// a workflow runs up to 16 sub-agents at a time next to the main agent
	// (and its delegations); 48 leaves headroom above that. With fewer
	// lanes than conversations taking turns, every lane is recycled before
	// its conversation's next step and every step encodes the whole history
	// again.
	maxEncoderLanes = 48
	// encoderCacheLimit bounds the cached encodings of all lanes together
	// (a lane holds about its conversation's size in JSON). Past it, lanes
	// are dropped (see chatEncoder.shrinkLocked); their conversations
	// encode in full on their next step, as every step did before this
	// cache (about 1.3 ms at 1000 messages). 8 MB holds a conversation that fills a
	// 1M-token context window (about 4 MB of JSON) next to a fan-out's
	// sub-agents at typical sizes; together with mediaCacheLimit it stays
	// under the performance plan's 15 MB memory-regression allowance.
	encoderCacheLimit = 8 << 20
	// encoderLaneIdle is how long an unused lane is kept. Without it, a
	// conversation that ended, was cleared or was compacted (its first
	// message changes, so it continues in a new lane) would keep its old
	// encodings, and the strings its snapshots share, until newer lanes
	// pushed it out, which in a single-agent session never happens.
	encoderLaneIdle = 5 * time.Minute
	// encoderLaneActive is how recently a lane must have been used to
	// count as part of the working set when the cache is over its limit:
	// a sub-agent's next step comes one model reply later, seconds to
	// tens of seconds, so a lane unused for a minute is most likely a
	// conversation that has ended.
	encoderLaneActive = time.Minute
	// maxToolSurfaces bounds the distinct tool arrays kept.
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
// Invalidation: by comparison on every use, as above. Lanes are dropped
// least recently used first past maxEncoderLanes; past encoderCacheLimit
// as shrinkLocked describes; and by a timer once unused for
// encoderLaneIdle. Messages with images are
// never cached: their data URLs come from the media cache, which checks
// the files on every use, and go into the body as chunks of their own.
// Requests of a single message (a compaction summary, a one-shot helper
// call, a sub-agent's first step) take no lane: nothing would reuse it,
// and a compaction transcript would hold its size until the lane idled
// out.
//
// Owner and ordering: shared by the Manager's goroutines, plus the idle
// timer's goroutine. mu guards the lane list, each lane's bookkeeping
// (lastUse, size, cached), the byte total, the timer and the tool arrays.
// A lane's own mu guards its encodings and is held while a request
// encodes that lane, so different conversations encode in parallel and
// only requests of the same conversation wait for each other. The two
// are never held together: encode takes mu to find the lane, releases it,
// encodes under the lane's lock, then takes mu again to charge the lane's
// new size. The media cache's lock is taken under a lane's lock, the
// schema cache's under mu; neither takes these in turn. Cached encodings
// are never modified once stored, so request bodies keep pointing at them
// after the locks are released, and after their lane is dropped.
type chatEncoder struct {
	mu    sync.Mutex
	lanes []*encoderLane // most recently used first
	total int            // sum of the cached lanes' size
	idle  *time.Timer    // pending idle sweep, nil when none
	tools []*toolSurface // most recently used first
}

// encoderLane is the cache of one conversation.
type encoderLane struct {
	// Fixed at creation. first is a snapshot of the conversation's first
	// message, which identifies the lane.
	provider, model string
	first           Message

	// Guarded by chatEncoder.mu: the last time a request used the lane,
	// the bytes it is charged for, and whether it is still in the list.
	lastUse time.Time
	size    int
	cached  bool

	// mu guards the encodings.
	mu        sync.Mutex
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
	switch len(req.Messages) {
	case 0:
		var w messageWriter
		writeChatMessage(&w, providerName, modelName, Message{Role: RoleUser, Content: req.Prompt}, req.Images)
		out := bodyMessages{body: body}
		for _, chunk := range w.finish() {
			out.add(chunk)
		}
	case 1:
		// A one-shot request: encoded through a lane of its own that is
		// never listed, so it is garbage once the body is sent.
		(&encoderLane{}).appendMessages(body, providerName, modelName, req)
	default:
		lane := e.lane(providerName, modelName, req.Messages[0])
		size := lane.appendMessages(body, providerName, modelName, req)
		e.charge(lane, size)
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

// lane returns the lane of the conversation that starts with first, moving
// it to the front, or creates one, dropping the least recently used lane
// when the list is full.
func (e *chatEncoder) lane(providerName, modelName string, first Message) *encoderLane {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := time.Now()
	for i, l := range e.lanes {
		if l.provider != providerName || l.model != modelName || !sameWireMessage(l.first, first) {
			continue
		}
		copy(e.lanes[1:i+1], e.lanes[:i])
		e.lanes[0] = l
		l.lastUse = now
		return l
	}
	if len(e.lanes) >= maxEncoderLanes {
		e.dropLaneLocked(len(e.lanes) - 1)
	}
	l := &encoderLane{provider: providerName, model: modelName, first: snapshotMessage(first), lastUse: now, cached: true}
	e.lanes = append(e.lanes, nil)
	copy(e.lanes[1:], e.lanes[:len(e.lanes)-1])
	e.lanes[0] = l
	e.armIdleLocked()
	return l
}

// charge records that l now caches size bytes and brings the cache back
// within encoderCacheLimit. A lane dropped while it was being encoded is
// not charged.
func (e *chatEncoder) charge(l *encoderLane, size int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !l.cached {
		return
	}
	e.total += size - l.size
	l.size = size
	e.shrinkLocked(l, time.Now())
}

// shrinkLocked drops lanes until the cached encodings fit
// encoderCacheLimit again, after grown was charged for new bytes. Lanes
// unused for encoderLaneActive go first, least recently used first. If
// that is not enough, grown itself is dropped: it does not fit next to
// the working set. Plain LRU would instead drop the lane whose turn comes
// next, so a fan-out larger than the limit would miss on every step (33
// conversations of 500 messages taking turns: 3668 allocations and
// 0.93 ms per encode); this way the conversations that fit keep hitting.
// The total was within the limit before grown was charged, so dropping
// grown always restores it.
func (e *chatEncoder) shrinkLocked(grown *encoderLane, now time.Time) {
	cutoff := now.Add(-encoderLaneActive)
	for e.total > encoderCacheLimit {
		last := len(e.lanes) - 1
		if victim := e.lanes[last]; victim == grown || !victim.lastUse.Before(cutoff) {
			e.dropLaneLocked(slices.Index(e.lanes, grown))
			return
		}
		e.dropLaneLocked(last)
	}
}

// dropLaneLocked removes lane i from the list. A request still encoding it
// finishes normally; the lane is garbage once that request is done.
func (e *chatEncoder) dropLaneLocked(i int) {
	l := e.lanes[i]
	e.total -= l.size
	l.size = 0
	l.cached = false
	e.lanes = slices.Delete(e.lanes, i, i+1)
}

// armIdleLocked schedules an idle sweep while lanes remain and none is
// pending.
func (e *chatEncoder) armIdleLocked() {
	if e.idle == nil && len(e.lanes) > 0 {
		e.idle = time.AfterFunc(encoderLaneIdle, e.onIdleTimer)
	}
}

// onIdleTimer runs on the timer's goroutine. A lane is dropped between
// encoderLaneIdle and twice that after its last use.
func (e *chatEncoder) onIdleTimer() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.idle = nil
	e.dropIdleLocked(time.Now())
	e.armIdleLocked()
}

// dropIdleLocked drops the lanes last used more than encoderLaneIdle
// before now. The list is in last-use order, so they are all at its end.
func (e *chatEncoder) dropIdleLocked(now time.Time) {
	cutoff := now.Add(-encoderLaneIdle)
	for n := len(e.lanes); n > 0 && e.lanes[n-1].lastUse.Before(cutoff); n = len(e.lanes) {
		e.dropLaneLocked(n - 1)
	}
}

// appendMessages adds the system prompt and every message of req to body,
// from the lane's cache where possible, and returns the bytes the lane
// caches afterwards.
func (l *encoderLane) appendMessages(body *wire.Body, providerName, modelName string, req Request) (size int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := bodyMessages{body: body}
	if strings.TrimSpace(req.System) != "" {
		if l.systemEnc == nil || l.system != req.System {
			l.system = req.System
			l.systemEnc = wire.AppendSystemMessage([]byte{','}, req.System)
		}
		out.add(l.systemEnc)
	}
	size += len(l.systemEnc)
	if n := len(req.Messages); len(l.entries) != n {
		if len(l.entries) > n {
			clear(l.entries[n:])
		}
		l.entries = slices.Grow(l.entries[:min(len(l.entries), n)], n)[:n]
	}
	imageIdx := lastUserIndex(req.Messages)
	for i, msg := range req.Messages {
		entry := &l.entries[i]
		var extra []string
		if i == imageIdx {
			extra = req.Images
		}
		if len(extra) > 0 || hasImages(msg) {
			*entry = encodedMessage{}
			var w messageWriter
			writeChatMessage(&w, providerName, modelName, msg, extra)
			for _, chunk := range w.finish() {
				out.add(chunk)
			}
			continue
		}
		if !entry.cached || !sameWireMessage(entry.msg, msg) {
			*entry = encodedMessage{msg: snapshotMessage(msg), enc: encodeMessage(providerName, modelName, msg), cached: true}
		}
		out.add(entry.enc)
		size += len(entry.enc)
	}
	return size
}

// bodyMessages appends message encodings to the "messages" array of a
// body. Every message encoding starts with a comma; the first chunk added
// loses it. That first chunk is always a message's own bytes, never a
// shared image literal (see messageWriter).
type bodyMessages struct {
	body    *wire.Body
	started bool
}

func (m *bodyMessages) add(chunk []byte) {
	if len(chunk) == 0 {
		return
	}
	if !m.started {
		chunk = chunk[1:]
		m.started = true
	}
	m.body.Append(chunk)
}

// toolsEncoding returns the encoded tool definitions.
func (e *chatEncoder) toolsEncoding(specs []ToolSpec) []byte {
	e.mu.Lock()
	defer e.mu.Unlock()
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

// messageWriter collects one message's encoding as body chunks. Most of it
// is written into buf; an image's data URL is added as a chunk of its own,
// shared with the media cache, instead of being copied: copying and
// re-scanning five 400 KB screenshots on every request cost 2.7 MB and
// most of the time of BenchmarkSendWithImages. A message's first chunk is
// always its own bytes (it starts with the message's comma).
type messageWriter struct {
	chunks [][]byte // finished chunks
	buf    []byte   // the chunk being written
}

// shared adds chunk, which must never be modified, after what was written
// so far.
func (w *messageWriter) shared(chunk []byte) {
	w.flush()
	w.chunks = append(w.chunks, chunk)
}

func (w *messageWriter) flush() {
	if len(w.buf) > 0 {
		w.chunks = append(w.chunks, w.buf)
		w.buf = nil
	}
}

// finish returns the message's chunks.
func (w *messageWriter) finish() [][]byte {
	w.flush()
	return w.chunks
}

// encodeMessage returns the encoding of msg, which has no images, as one
// byte slice.
func encodeMessage(providerName, modelName string, msg Message) []byte {
	var w messageWriter
	writeChatMessage(&w, providerName, modelName, msg, nil)
	if len(w.chunks) == 0 {
		return w.buf // nothing was shared: the usual case
	}
	return bytes.Join(w.finish(), nil)
}

// writeChatMessage writes msg as chat-completions message objects, each
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
func writeChatMessage(w *messageWriter, providerName, modelName string, msg Message, extraImages []string) {
	switch msg.Role {
	case RoleUser:
		if len(msg.ToolResults) > 0 {
			var spill []string
			for _, tr := range msg.ToolResults {
				w.buf = append(w.buf, ',')
				w.buf = wire.AppendToolResult(w.buf, tr.ID, tr.Output)
				spill = append(spill, tr.Images...)
			}
			if msg.Content != "" {
				w.buf = append(w.buf, ',')
				w.buf = wire.AppendUserText(w.buf, msg.Content)
			}
			if literals := imageURLLiterals(spill); len(literals) > 0 {
				w.buf = append(w.buf, ',')
				writeUserParts(w, toolImageNote, literals)
			}
			return
		}
		images := msg.Images
		if len(extraImages) > 0 {
			images = append(images[:len(images):len(images)], extraImages...)
		}
		w.buf = append(w.buf, ',')
		if literals := imageURLLiterals(images); len(literals) > 0 {
			writeUserParts(w, msg.Content, literals)
			return
		}
		w.buf = wire.AppendUserText(w.buf, msg.Content)
	case RoleAssistant:
		w.buf = appendAssistantMessage(w.buf, providerName, modelName, msg)
	}
}

// writeUserParts writes a user message with text and images. literals are
// the images' data URLs as JSON strings, from the media cache.
func writeUserParts(w *messageWriter, text string, literals [][]byte) {
	w.buf = wire.AppendUserPartsStart(w.buf, text)
	for _, literal := range literals {
		w.buf = append(w.buf, wire.ImagePartStart...)
		w.shared(literal)
		w.buf = append(w.buf, wire.ImagePartEnd...)
	}
	w.buf = append(w.buf, wire.UserPartsEnd...)
}

// appendAssistantMessage appends an assistant turn (see writeChatMessage).
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

// imageURLLiterals loads images as data-URL JSON strings, skipping
// unreadable files.
func imageURLLiterals(paths []string) [][]byte {
	var literals [][]byte
	for _, p := range paths {
		if literal, _, ok := requestMedia.urlLiteral(p); ok {
			literals = append(literals, literal)
		}
	}
	return literals
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
