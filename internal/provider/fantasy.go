package provider

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"charm.land/fantasy"
	fantasyanthropic "charm.land/fantasy/providers/anthropic"
	fantasyopenai "charm.land/fantasy/providers/openai"
	fantasyopenaicompat "charm.land/fantasy/providers/openaicompat"

	"spettro/internal/models"
	"spettro/internal/version"
)

func sendWithFantasy(ctx context.Context, providerName, apiKind, modelName, apiKey, baseURL string, req Request) (Response, error) {
	prov, err := newFantasyProvider(providerName, apiKind, apiKey, baseURL, nil)
	if err != nil {
		return Response{}, err
	}

	model, err := prov.LanguageModel(ctx, modelName)
	if err != nil {
		return Response{}, err
	}

	call := buildFantasyCall(providerName, apiKind, modelName, req)
	resp, err := model.Generate(ctx, call)
	if err != nil {
		return Response{}, err
	}

	totalTokens := int(resp.Usage.TotalTokens)
	if totalTokens == 0 {
		totalTokens = int(resp.Usage.InputTokens + resp.Usage.OutputTokens)
	}

	maxOut := sentMaxOutput(call)
	var raw []rawToolCall
	for _, tc := range resp.Content.ToolCalls() {
		// A non-streamed reply has no "arguments finished" event; arguments
		// that are not valid JSON count as unfinished so a call cut off at
		// the output limit is recognized (see finalizeToolCalls).
		complete := strings.TrimSpace(tc.Input) == "" || json.Valid([]byte(tc.Input))
		raw = append(raw, rawToolCall{id: tc.ToolCallID, name: tc.ToolName, input: tc.Input, complete: complete})
	}
	var reasoning []ReasoningBlock
	for _, rc := range resp.Content.Reasoning() {
		sig, redacted := reasoningMetadata(rc.ProviderMetadata)
		reasoning = appendReasoning(reasoning, rc.Text, sig, redacted, providerName, modelName)
	}
	finish := detectTruncation(mapFinishReason(resp.FinishReason), resp.Usage, maxOut)
	toolCalls, finish := finalizeToolCalls(raw, finish, maxOut)
	return Response{
		Content:         fantasyText(resp),
		ToolCalls:       toolCalls,
		EstimatedTokens: totalTokens,
		Usage:           usageFromFantasy(resp.Usage),
		FinishReason:    finish,
		Reasoning:       reasoning,
		MaxOutputTokens: maxOut,
	}, nil
}

// ErrStreamIdle is returned when a streaming response goes silent for longer
// than the idle timeout. It is a transient failure (see ClassifyRetry): the
// connection most likely stalled in a proxy or a local server, and the
// request is safe to resend.
var ErrStreamIdle = errors.New("stream idle timeout: the provider sent no data")

// Stream silence limits. Activity is measured on the raw response body, so
// SSE keep-alives (Anthropic "ping" events, ": comment" lines) count as
// life even when the SDK yields no part: a healthy stream that is buffering
// a large tool input or thinking with its display omitted is not cut off.
// The limits are still generous, because some backends send nothing at all
// while working: OpenAI Responses reasoning models without a summary, and
// local servers processing a long prompt before the first token.
//
//   - DefaultStreamIdleTimeout: the longest silence between two chunks.
//   - streamReasoningIdleTimeout: the same, when the model may reason
//     silently (a thinking level is set, or OpenAI's Responses API).
//   - streamFirstChunkTimeout: before the first body byte.
//   - streamLocalFirstChunkTimeout: before the first body byte from a local
//     endpoint (prompt processing on slow hardware; no proxy can stall it).
//
// SPETTRO_STREAM_IDLE_TIMEOUT (a Go duration or a number of seconds; 0
// disables the watchdog) overrides all of them, as does
// Request.StreamIdleTimeout.
const (
	DefaultStreamIdleTimeout     = 300 * time.Second
	streamReasoningIdleTimeout   = 600 * time.Second
	streamFirstChunkTimeout      = 600 * time.Second
	streamLocalFirstChunkTimeout = 30 * time.Minute
)

// streamIdleEnv names the environment override for the stream watchdog.
const streamIdleEnv = "SPETTRO_STREAM_IDLE_TIMEOUT"

// streamIdleOverride parses SPETTRO_STREAM_IDLE_TIMEOUT. ok is false when it
// is unset or unparsable; a zero duration disables the watchdog.
func streamIdleOverride() (time.Duration, bool) {
	v := strings.TrimSpace(os.Getenv(streamIdleEnv))
	if v == "" {
		return 0, false
	}
	if n, err := strconv.Atoi(v); err == nil && n >= 0 {
		return time.Duration(n) * time.Second, true
	}
	if d, err := time.ParseDuration(v); err == nil && d >= 0 {
		return d, true
	}
	return 0, false
}

// streamTimeouts returns the (first-chunk, between-chunk) silence limits for
// req on providerName. An explicit Request.StreamIdleTimeout, then the
// environment override, applies to both; 0 means no watchdog.
func streamTimeouts(providerName string, req Request) (first, idle time.Duration) {
	if req.StreamIdleTimeout > 0 {
		return req.StreamIdleTimeout, req.StreamIdleTimeout
	}
	if d, ok := streamIdleOverride(); ok {
		return d, d
	}
	idle = DefaultStreamIdleTimeout
	if (req.Thinking != "" && req.Thinking != ThinkingOff) || providerName == "openai" {
		idle = streamReasoningIdleTimeout
	}
	first = streamFirstChunkTimeout
	if req.localEndpoint {
		first = streamLocalFirstChunkTimeout
	}
	return first, idle
}

// streamWatchdog cancels a stream with ErrStreamIdle once no response bytes
// arrived for too long: first before any byte, idle after. touch is safe
// from any goroutine (the SDK reads the body on its own).
type streamWatchdog struct {
	last    atomic.Int64 // unix nanos of the last activity
	started atomic.Bool  // a body byte arrived
	done    chan struct{}
	once    sync.Once
}

func startStreamWatchdog(first, idle time.Duration, cancel context.CancelCauseFunc) *streamWatchdog {
	w := &streamWatchdog{done: make(chan struct{})}
	w.last.Store(time.Now().UnixNano())
	if first <= 0 || idle <= 0 {
		return w
	}
	tick := min(first, idle) / 8
	tick = max(min(tick, time.Second), time.Millisecond)
	go func() {
		t := time.NewTicker(tick)
		defer t.Stop()
		for {
			select {
			case <-w.done:
				return
			case <-t.C:
				limit := first
				if w.started.Load() {
					limit = idle
				}
				if time.Since(time.Unix(0, w.last.Load())) > limit {
					cancel(ErrStreamIdle)
					return
				}
			}
		}
	}()
	return w
}

// touch records activity: a response body read that returned data.
func (w *streamWatchdog) touch() {
	w.last.Store(time.Now().UnixNano())
	w.started.Store(true)
}

func (w *streamWatchdog) stop() { w.once.Do(func() { close(w.done) }) }

// activityHTTPClient is the SDK HTTP client for a watched stream: it reports
// every chunk of response body to onRead, keep-alives included.
type activityHTTPClient struct{ onRead func() }

func (c activityHTTPClient) Do(r *http.Request) (*http.Response, error) {
	resp, err := http.DefaultClient.Do(r)
	if resp != nil && resp.Body != nil {
		resp.Body = activityBody{ReadCloser: resp.Body, onRead: c.onRead}
	}
	return resp, err
}

type activityBody struct {
	io.ReadCloser
	onRead func()
}

func (b activityBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		b.onRead()
	}
	return n, err
}

// sendWithFantasyStream is the streaming counterpart of sendWithFantasy. It
// forwards text and reasoning deltas to req.OnStream as they arrive while still
// accumulating the full answer text (reasoning is delivered live but not folded
// into Response.Content, matching the non-streaming path). A watchdog cancels
// the stream with ErrStreamIdle when the provider goes silent (see
// streamTimeouts).
func sendWithFantasyStream(ctx context.Context, providerName, apiKind, modelName, apiKey, baseURL string, req Request) (Response, error) {
	firstTimeout, idleTimeout := streamTimeouts(providerName, req)
	streamCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	watchdog := startStreamWatchdog(firstTimeout, idleTimeout, cancel)
	defer watchdog.stop()

	prov, err := newFantasyProvider(providerName, apiKind, apiKey, baseURL, &activityHTTPClient{onRead: watchdog.touch})
	if err != nil {
		return Response{}, err
	}

	model, err := prov.LanguageModel(ctx, modelName)
	if err != nil {
		return Response{}, err
	}
	idleErr := func(err error) error {
		if ctx.Err() == nil && errors.Is(context.Cause(streamCtx), ErrStreamIdle) {
			return ErrStreamIdle
		}
		return err
	}

	call := buildFantasyCall(providerName, apiKind, modelName, req)
	stream, err := model.Stream(streamCtx, call)
	if err != nil {
		return Response{}, idleErr(err)
	}

	var (
		textSB    strings.Builder
		usage     fantasy.Usage
		finish    FinishReason
		streamErr error
		// Tool calls in first-seen order. Inputs are accumulated from the
		// delta events too: the OpenAI-compatible stream only emits a
		// ToolCall part once the arguments parse as JSON, so a call cut off
		// at the output limit (or malformed) would otherwise vanish.
		calls     []*rawToolCall
		callsByID = map[string]*rawToolCall{}
		// Reasoning blocks in first-seen order, keyed by stream part ID.
		thoughts     []*streamReasoning
		thoughtsByID = map[string]*streamReasoning{}
	)
	toolCall := func(id, name string) *rawToolCall {
		if tc, ok := callsByID[id]; ok {
			if tc.name == "" {
				tc.name = name
			}
			return tc
		}
		tc := &rawToolCall{id: id, name: name}
		callsByID[id] = tc
		calls = append(calls, tc)
		return tc
	}
	thought := func(id string) *streamReasoning {
		if r, ok := thoughtsByID[id]; ok {
			return r
		}
		r := &streamReasoning{}
		thoughtsByID[id] = r
		thoughts = append(thoughts, r)
		return r
	}
	for part := range stream {
		watchdog.touch()
		switch part.Type {
		case fantasy.StreamPartTypeTextDelta:
			textSB.WriteString(part.Delta)
			if req.OnStream != nil && part.Delta != "" {
				req.OnStream(StreamEvent{Kind: StreamText, Delta: part.Delta})
			}
		case fantasy.StreamPartTypeReasoningStart, fantasy.StreamPartTypeReasoningDelta, fantasy.StreamPartTypeReasoningEnd:
			r := thought(part.ID)
			r.text.WriteString(part.Delta)
			if sig, redacted := reasoningMetadata(part.ProviderMetadata); sig != "" || redacted != "" {
				if sig != "" {
					r.signature = sig
				}
				if redacted != "" {
					r.redacted = redacted
				}
			}
			if part.Type == fantasy.StreamPartTypeReasoningDelta && req.OnStream != nil && part.Delta != "" {
				req.OnStream(StreamEvent{Kind: StreamReasoning, Delta: part.Delta})
			}
		case fantasy.StreamPartTypeToolInputStart:
			if !part.ProviderExecuted {
				toolCall(part.ID, part.ToolCallName)
			}
		case fantasy.StreamPartTypeToolInputDelta:
			if tc, ok := callsByID[part.ID]; ok && !tc.complete {
				tc.input += part.Delta
			}
		case fantasy.StreamPartTypeToolCall:
			if part.ProviderExecuted {
				continue
			}
			tc := toolCall(part.ID, part.ToolCallName)
			tc.input = part.ToolCallInput
			tc.complete = true
		case fantasy.StreamPartTypeFinish:
			usage = part.Usage
			finish = mapFinishReason(part.FinishReason)
		case fantasy.StreamPartTypeError:
			if part.Error != nil {
				streamErr = part.Error
			}
		}
	}
	watchdog.stop()
	if streamErr != nil {
		return Response{}, idleErr(streamErr)
	}
	if err := context.Cause(streamCtx); err != nil && errors.Is(err, ErrStreamIdle) && ctx.Err() == nil {
		// The watchdog fired but the iterator ended without an error part.
		return Response{}, ErrStreamIdle
	}

	totalTokens := int(usage.TotalTokens)
	if totalTokens == 0 {
		totalTokens = int(usage.InputTokens + usage.OutputTokens)
	}

	maxOut := sentMaxOutput(call)
	raw := make([]rawToolCall, 0, len(calls))
	for _, tc := range calls {
		raw = append(raw, *tc)
	}
	finish = detectTruncation(finish, usage, maxOut)
	toolCalls, finish := finalizeToolCalls(raw, finish, maxOut)
	var reasoning []ReasoningBlock
	for _, r := range thoughts {
		reasoning = appendReasoning(reasoning, r.text.String(), r.signature, r.redacted, providerName, modelName)
	}

	return Response{
		Content:         textSB.String(),
		ToolCalls:       toolCalls,
		EstimatedTokens: totalTokens,
		Usage:           usageFromFantasy(usage),
		FinishReason:    finish,
		Reasoning:       reasoning,
		MaxOutputTokens: maxOut,
	}, nil
}

// rawToolCall is one tool call as the model produced it, before argument
// normalization. complete is false when the stream ended before the provider
// considered the arguments finished.
type rawToolCall struct {
	id, name, input string
	complete        bool
}

type streamReasoning struct {
	text                strings.Builder
	signature, redacted string
}

// finalizeToolCalls normalizes every call's arguments (see
// normalizeToolArgs) and returns the finish reason, upgraded to FinishLength
// when the reply evidently ended mid-call: the last call never finished
// streaming and its arguments stop in the middle of a JSON value. (The
// OpenAI-style adapters report "tool-calls" in that case, hiding the length
// stop, and no output cap may have been sent to compare usage against.)
// Unnamed fragments — an argument delta for a call the stream never
// introduced — are dropped: there is nothing to route them to.
func finalizeToolCalls(raw []rawToolCall, finish FinishReason, maxOut int) ([]NativeTool, FinishReason) {
	if n := len(raw); n > 0 && finish != FinishLength {
		if last := raw[n-1]; !last.complete && endsMidJSON(last.input) {
			finish = FinishLength
		}
	}
	var out []NativeTool
	for _, tc := range raw {
		if tc.name == "" {
			continue
		}
		args, argsErr := normalizeToolArgs(tc.input, finish == FinishLength, maxOut)
		out = append(out, NativeTool{ID: tc.id, Name: tc.name, Args: args, ArgsError: argsErr})
	}
	return out, finish
}

// detectTruncation reports FinishLength when the provider said so, and also
// when it did not but the reply demonstrably hit the cap: the OpenAI-style
// adapters rewrite the finish reason to "tool-calls" whenever the reply
// contains any tool call, hiding a length stop. A reply that used every
// output token it was allowed is therefore treated as truncated. (A tool
// call that never finished streaming below the cap is malformed, not
// truncated: it goes through the repair pass instead.)
func detectTruncation(finish FinishReason, usage fantasy.Usage, maxOut int) FinishReason {
	if finish == FinishLength {
		return finish
	}
	if maxOut > 0 && usage.OutputTokens >= int64(maxOut) {
		return FinishLength
	}
	return finish
}

func mapFinishReason(r fantasy.FinishReason) FinishReason {
	switch r {
	case fantasy.FinishReasonStop:
		return FinishStop
	case fantasy.FinishReasonLength:
		return FinishLength
	case fantasy.FinishReasonToolCalls:
		return FinishToolCalls
	case fantasy.FinishReasonContentFilter:
		return FinishContentFilter
	case fantasy.FinishReasonError:
		return FinishError
	case fantasy.FinishReasonOther:
		return FinishOther
	}
	return ""
}

// sentMaxOutput is the output cap the call carries (0 when none is sent).
func sentMaxOutput(call fantasy.Call) int {
	if call.MaxOutputTokens == nil {
		return 0
	}
	return int(*call.MaxOutputTokens)
}

// reasoningMetadata extracts Anthropic's thinking signature / redacted
// payload from a reasoning part's provider metadata.
func reasoningMetadata(md fantasy.ProviderMetadata) (signature, redacted string) {
	if md == nil {
		return "", ""
	}
	if meta := fantasyanthropic.GetReasoningMetadata(fantasy.ProviderOptions(md)); meta != nil {
		return meta.Signature, meta.RedactedData
	}
	return "", ""
}

func appendReasoning(out []ReasoningBlock, text, signature, redacted, providerName, modelName string) []ReasoningBlock {
	if text == "" && signature == "" && redacted == "" {
		return out
	}
	return append(out, ReasoningBlock{
		Text:         text,
		Signature:    signature,
		RedactedData: redacted,
		Provider:     providerName,
		Model:        modelName,
	})
}

// replayReasoning builds the reasoning parts to send back on an assistant
// turn. Blocks are replayed only to the model that produced them (a thinking
// signature is not valid for any other model), and only where the wire
// format carries them:
//   - Anthropic: signed thinking / redacted_thinking blocks, while extended
//     thinking is enabled — required on the in-progress tool-use turn.
//   - OpenAI-compatible chat: reasoning_content, which reasoning models such
//     as DeepSeek and Kimi expect back during a tool loop.
//   - OpenAI Responses: not replayable inline (fantasy drops reasoning items
//     on replay; the IDs are ephemeral without server-side storage), so
//     nothing is sent.
func replayReasoning(providerName, apiKind, modelName string, req Request, blocks []ReasoningBlock) []fantasy.MessagePart {
	if len(blocks) == 0 || providerName == "openai" {
		return nil
	}
	anthropicAPI := isAnthropicAPI(providerName, apiKind)
	if anthropicAPI && ThinkingBudgetTokens(req.Thinking) <= 0 {
		return nil
	}
	var parts []fantasy.MessagePart
	for _, b := range blocks {
		if b.Provider != providerName || b.Model != modelName {
			continue
		}
		if anthropicAPI {
			if b.Signature == "" && b.RedactedData == "" {
				continue
			}
			parts = append(parts, fantasy.ReasoningPart{
				Text: b.Text,
				ProviderOptions: fantasy.ProviderOptions{
					fantasyanthropic.Name: &fantasyanthropic.ReasoningOptionMetadata{Signature: b.Signature, RedactedData: b.RedactedData},
				},
			})
			continue
		}
		if b.Text != "" {
			parts = append(parts, fantasy.ReasoningPart{Text: b.Text})
		}
	}
	return parts
}

// usageFromFantasy maps fantasy's usage block onto Spettro's Usage type.
func usageFromFantasy(u fantasy.Usage) Usage {
	return Usage{
		InputTokens:      int(u.InputTokens),
		OutputTokens:     int(u.OutputTokens),
		CacheReadTokens:  int(u.CacheReadTokens),
		CacheWriteTokens: int(u.CacheCreationTokens),
	}
}

// isAnthropicAPI reports whether the provider uses the Anthropic wire
// protocol, either because it IS the built-in anthropic provider or because
// the catalog marks it as api: "anthropic".
func isAnthropicAPI(providerName, apiKind string) bool {
	return providerName == "anthropic" || apiKind == models.APIAnthropic
}

// buildFantasyCall assembles the shared fantasy.Call used by both the streaming
// and non-streaming paths.
func buildFantasyCall(providerName, apiKind, modelName string, req Request) fantasy.Call {
	var prompt fantasy.Prompt
	if len(req.Messages) > 0 {
		if req.System != "" {
			prompt = append(prompt, fantasy.NewSystemMessage(req.System))
		}
		// Request-level images (legacy field) belong to the current turn — the
		// last plain user message — alongside any message-level images.
		imageIdx := lastUserIndex(req.Messages)
		for i, m := range req.Messages {
			switch m.Role {
			case RoleUser:
				if len(m.ToolResults) > 0 {
					// Tool results must use MessageRoleTool so the Anthropic provider
					// routes them through the tool_result content block path.
					parts := make([]fantasy.MessagePart, 0, len(m.ToolResults))
					// Images produced by tools (screenshot, view-image) ride inside
					// the tool_result as media on Anthropic; other providers only
					// accept text tool results, so their images are re-attached as
					// an immediately following user turn instead.
					var spillImages []string
					for _, tr := range m.ToolResults {
						if isAnthropicAPI(providerName, apiKind) && len(tr.Images) > 0 {
							if media, ok := loadToolResultMedia(tr.Images[0], tr.Output); ok {
								parts = append(parts, fantasy.ToolResultPart{
									ToolCallID: tr.ID,
									Output:     media,
								})
								spillImages = append(spillImages, tr.Images[1:]...)
								continue
							}
						}
						parts = append(parts, fantasy.ToolResultPart{
							ToolCallID: tr.ID,
							Output:     fantasy.ToolResultOutputContentText{Text: tr.Output},
						})
						spillImages = append(spillImages, tr.Images...)
					}
					prompt = append(prompt, fantasy.Message{Role: fantasy.MessageRoleTool, Content: parts})
					// If there is accompanying text, append it as a separate user turn.
					if m.Content != "" {
						prompt = append(prompt, fantasy.NewUserMessage(m.Content))
					}
					if imgs := fantasyImageParts(spillImages); len(imgs) > 0 {
						prompt = append(prompt, fantasy.NewUserMessage("[image attached from the tool result above]", imgs...))
					}
				} else {
					imgs := m.Images
					if i == imageIdx {
						imgs = append(imgs[:len(imgs):len(imgs)], req.Images...)
					}
					prompt = append(prompt, fantasy.NewUserMessage(m.Content, fantasyImageParts(imgs)...))
				}
			case RoleAssistant:
				parts := make([]fantasy.MessagePart, 0, 1+len(m.ToolCalls)+len(m.Reasoning))
				// Reasoning goes first: Anthropic requires the thinking block
				// to open the assistant turn it belongs to.
				parts = append(parts, replayReasoning(providerName, apiKind, modelName, req, m.Reasoning)...)
				if m.Content != "" {
					parts = append(parts, fantasy.TextPart{Text: m.Content})
				}
				for _, tc := range m.ToolCalls {
					args := string(tc.Args)
					if args == "" {
						args = "{}"
					}
					parts = append(parts, fantasy.ToolCallPart{
						ToolCallID: tc.ID,
						ToolName:   tc.Name,
						Input:      args,
					})
				}
				if len(parts) == 0 {
					parts = append(parts, fantasy.TextPart{Text: ""})
				}
				prompt = append(prompt, fantasy.Message{Role: fantasy.MessageRoleAssistant, Content: parts})
			}
		}
		if isAnthropicAPI(providerName, apiKind) {
			// Two cache breakpoints: the system prompt and the final message.
			// Marking the FINAL message caches the whole request — including
			// the newest tool results, which are often the largest blocks — so
			// the next call in the loop reads everything before it from cache.
			// (Anthropic looks the prefix up from previously-written
			// breakpoints, so moving the marker forward each step is the
			// intended incremental pattern.)
			cc := anthropicEphemeralOpts()
			prompt[0].ProviderOptions = cc
			if len(prompt) >= 2 {
				prompt[len(prompt)-1].ProviderOptions = cc
			}
		}
	} else {
		prompt = fantasy.Prompt{fantasy.NewUserMessage(req.Prompt, fantasyImageParts(req.Images)...)}
	}
	call := fantasy.Call{
		Prompt:    prompt,
		UserAgent: fantasyUserAgent(),
	}
	if len(req.Tools) > 0 {
		call.Tools = make([]fantasy.Tool, 0, len(req.Tools))
		for _, t := range req.Tools {
			var schema map[string]any
			if err := json.Unmarshal(t.Schema, &schema); err != nil || schema == nil {
				schema = map[string]any{"type": "object", "additionalProperties": true}
			}
			call.Tools = append(call.Tools, fantasy.FunctionTool{
				Name:        t.Name,
				Description: t.Description,
				InputSchema: schema,
			})
		}
		auto := fantasy.ToolChoiceAuto
		call.ToolChoice = &auto
	}
	if req.MaxTokens > 0 {
		maxTokens := int64(req.MaxTokens)
		call.MaxOutputTokens = &maxTokens
	}
	// Fantasy threads thinking config via per-provider ProviderOptions.
	// Anthropic takes a token budget (thinking.budget_tokens); OpenAI and
	// every OpenAI-compatible backend take a reasoning_effort enum. On the
	// Anthropic path both "off" and empty send nothing (no thinking config
	// means extended thinking disabled). On OpenAI-style paths an explicit
	// "off" sends reasoning_effort "none" so models that reason by default
	// actually stop, while empty (never set) sends nothing and keeps the
	// provider default; a server that rejects "none" is retried without the
	// field by the manager's downgrade ladder.
	if isAnthropicAPI(providerName, apiKind) {
		if budget := ThinkingBudgetTokens(ThinkingLevel(req.Thinking)); budget > 0 {
			budgetInt := int64(budget)
			call.ProviderOptions = fantasy.ProviderOptions{
				"anthropic": &fantasyanthropic.ProviderOptions{
					Thinking: &fantasyanthropic.ThinkingProviderOption{BudgetTokens: budgetInt},
				},
			}
			needed := budgetInt + 4096
			if call.MaxOutputTokens == nil || *call.MaxOutputTokens < needed {
				call.MaxOutputTokens = &needed
			}
		}
	} else if effort := ReasoningEffort(ThinkingLevel(req.Thinking)); effort != "" {
		e := fantasyopenai.ReasoningEffort(effort)
		if providerName == "openai" {
			// The official provider routes Responses-capable models through the
			// Responses API, which reads a different options type than the chat
			// path — and the chat path hard-errors on a type mismatch, so the
			// choice must follow fantasy's own routing. Either way the provider
			// only forwards the effort to models it knows are reasoning-capable,
			// so this is safe for gpt-4o etc.
			if fantasyopenai.IsResponsesModel(modelName) {
				call.ProviderOptions = fantasy.ProviderOptions{
					fantasyopenai.Name: &fantasyopenai.ResponsesProviderOptions{ReasoningEffort: &e},
				}
			} else {
				call.ProviderOptions = fantasy.ProviderOptions{
					fantasyopenai.Name: &fantasyopenai.ProviderOptions{ReasoningEffort: &e},
				}
			}
		} else {
			// OpenAI-compatible backends (Groq, xAI, DeepSeek, Spettro
			// Subscription, local servers, …) take reasoning_effort on the chat
			// completions path. Servers that don't know the parameter ignore it.
			call.ProviderOptions = fantasy.ProviderOptions{
				fantasyopenaicompat.Name: &fantasyopenaicompat.ProviderOptions{ReasoningEffort: &e},
			}
		}
	}
	return call
}

// loadToolResultMedia reads an image file into a media tool-result output
// (base64 + mime), keeping the tool's text output alongside it. Returns false
// when the file cannot be read so the caller falls back to a text-only result.
func loadToolResultMedia(path, text string) (fantasy.ToolResultOutputContentMedia, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return fantasy.ToolResultOutputContentMedia{}, false
	}
	return fantasy.ToolResultOutputContentMedia{
		Data:      base64.StdEncoding.EncodeToString(data),
		MediaType: mediaTypeFromPath(path),
		Text:      text,
	}, true
}

// fantasyImageParts loads image files into fantasy FileParts. Unreadable
// paths are skipped (matching the legacy adapters) so a vanished temp file
// degrades to a text-only turn instead of failing the whole request.
func fantasyImageParts(paths []string) []fantasy.FilePart {
	var parts []fantasy.FilePart
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		parts = append(parts, fantasy.FilePart{
			Filename:  filepath.Base(p),
			Data:      data,
			MediaType: mediaTypeFromPath(p),
		})
	}
	return parts
}

// newFantasyProvider builds the fantasy provider for one request. A non-nil
// client replaces the SDK's default HTTP client (the stream watchdog uses it
// to observe body activity).
func newFantasyProvider(providerName, apiKind, apiKey, baseURL string, client *activityHTTPClient) (fantasy.Provider, error) {
	switch {
	case providerName == "anthropic" || apiKind == models.APIAnthropic:
		opts := []fantasyanthropic.Option{
			fantasyanthropic.WithUserAgent(fantasyUserAgent()),
		}
		if client != nil {
			opts = append(opts, fantasyanthropic.WithHTTPClient(*client))
		}
		if apiKey != "" {
			opts = append(opts, fantasyanthropic.WithAPIKey(apiKey))
		}
		// The official provider uses the SDK's default endpoint; only
		// anthropic-compatible third parties need an explicit base URL.
		// The Anthropic SDK appends /v1/messages to the base URL, so we
		// must strip any trailing /v1 from the catalog's base URL to
		// avoid doubling it (e.g. /v1/v1/messages).
		if providerName != "anthropic" && baseURL != "" {
			stripped := strings.TrimSuffix(baseURL, "/v1")
			opts = append(opts, fantasyanthropic.WithBaseURL(stripped))
		}
		return fantasyanthropic.New(opts...)
	case providerName == "openai":
		opts := []fantasyopenai.Option{
			fantasyopenai.WithUserAgent(fantasyUserAgent()),
			fantasyopenai.WithUseResponsesAPI(),
		}
		if client != nil {
			opts = append(opts, fantasyopenai.WithHTTPClient(*client))
		}
		if apiKey != "" {
			opts = append(opts, fantasyopenai.WithAPIKey(apiKey))
		}
		return fantasyopenai.New(opts...)
	default:
		resolvedBaseURL, err := resolveOpenAICompatibleBaseURL(providerName, baseURL)
		if err != nil {
			return nil, err
		}
		if apiKey == "" {
			apiKey = "local"
		}

		opts := []fantasyopenaicompat.Option{
			fantasyopenaicompat.WithName(providerName),
			fantasyopenaicompat.WithAPIKey(apiKey),
			fantasyopenaicompat.WithUserAgent(fantasyUserAgent()),
		}
		if client != nil {
			opts = append(opts, fantasyopenaicompat.WithHTTPClient(*client))
		}
		if resolvedBaseURL != "" {
			opts = append(opts, fantasyopenaicompat.WithBaseURL(resolvedBaseURL))
		}
		if providerName == "openai-compatible" && resolvedBaseURL == "" {
			opts = append(opts, fantasyopenaicompat.WithUseResponsesAPI())
		}
		return fantasyopenaicompat.New(opts...)
	}
}

func shouldFallbackToLegacy(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "not a chat model") || strings.Contains(msg, "v1/completions")
}

func fantasyText(resp *fantasy.Response) string {
	if resp == nil {
		return ""
	}
	var sb strings.Builder
	for _, part := range resp.Content {
		if text, ok := fantasy.AsContentType[fantasy.TextContent](part); ok {
			sb.WriteString(text.Text)
		}
	}
	return sb.String()
}

func fantasyUserAgent() string {
	return "Spettro/" + version.App + " via fantasy"
}

func anthropicEphemeralOpts() fantasy.ProviderOptions {
	return fantasyanthropic.NewProviderCacheControlOptions(&fantasyanthropic.ProviderCacheControlOptions{
		CacheControl: fantasyanthropic.CacheControl{Type: "ephemeral"},
	})
}
