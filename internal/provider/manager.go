package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"math/rand/v2"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"charm.land/fantasy"
	openai "github.com/openai/openai-go/v3"

	"spettro/internal/budget"
	"spettro/internal/models"
)

// spettroProviderID is the provider key for the Spettro Subscription. Models
// under this provider are routed to the Spettro backend's OpenAI-compatible
// inference proxy rather than to a third-party LLM provider.
const spettroProviderID = "spettro"

type Manager struct {
	mu            sync.RWMutex
	catalog       []Model
	localModels   []Model
	spettroModels []Model
	apiKeys       map[string]string
	providerAPIs  map[string]string
	providerKinds map[string]string // provider id -> models.APIOpenAI | models.APIAnthropic
	usageRec      usageRecorder
	// streamAll routes every request through the streaming path, even when
	// the caller wants no live tokens (see SetStreamAll).
	streamAll bool
	// effortDowngrades remembers, per provider, model and thinking level, the
	// lower level Send stepped down to after the backend rejected the
	// reasoning_effort value, so later sends start there instead of walking
	// the ladder again on every call (see rememberedThinking).
	effortDowngrades map[string]ThinkingLevel
}

func NewManager() *Manager {
	return &Manager{
		apiKeys:       map[string]string{},
		providerAPIs:  map[string]string{},
		providerKinds: map[string]string{},
	}
}

// SetStreamAll makes every request stream, including those of callers that
// set no OnStream (sub-agents, headless runs, compaction). Streaming is what
// lets the idle watchdog turn a stalled connection into a retryable
// ErrStreamIdle instead of an indefinite hang, and what recovers tool calls
// cut off at the output limit on OpenAI-compatible backends. Production
// hosts turn it on; it is off by default so plain JSON test servers keep
// working. Anthropic-protocol requests always stream regardless.
func (m *Manager) SetStreamAll(on bool) {
	m.mu.Lock()
	m.streamAll = on
	m.mu.Unlock()
}

func (m *Manager) SetAPIKeys(keys map[string]string) {
	m.mu.Lock()
	m.apiKeys = make(map[string]string, len(keys))
	maps.Copy(m.apiKeys, keys)
	m.mu.Unlock()
}

func (m *Manager) SetCatalog(cat models.Catalog) {
	built := buildModels(cat)
	apis := make(map[string]string, len(cat.Providers))
	kinds := make(map[string]string, len(cat.Providers))
	for id, prov := range cat.Providers {
		if prov.BaseURL != "" {
			apis[id] = prov.BaseURL
		}
		kinds[id] = prov.API
	}
	m.mu.Lock()
	m.catalog = built
	m.providerKinds = kinds
	for k, v := range m.providerAPIs {
		if strings.HasPrefix(k, "http://") || strings.HasPrefix(k, "https://") {
			apis[k] = v
		}
	}
	// Preserve the Spettro Subscription endpoint across catalog refreshes.
	if v, ok := m.providerAPIs[spettroProviderID]; ok {
		apis[spettroProviderID] = v
	}
	m.providerAPIs = apis
	m.mu.Unlock()
}

// SetSpettro registers the Spettro Subscription models and inference endpoint.
// Passing an empty model list clears the models but keeps the endpoint so that
// in-flight inference still resolves while a fresh list is being fetched.
func (m *Manager) SetSpettro(inferenceBaseURL string, models []Model) {
	m.mu.Lock()
	m.spettroModels = models
	if inferenceBaseURL != "" {
		m.providerAPIs[spettroProviderID] = inferenceBaseURL
	}
	m.mu.Unlock()
}

// ClearSpettro removes the Spettro Subscription models and endpoint (logout).
func (m *Manager) ClearSpettro() {
	m.mu.Lock()
	m.spettroModels = nil
	delete(m.providerAPIs, spettroProviderID)
	m.mu.Unlock()
}

func (m *Manager) AddLocalModels(models []Model) {
	if len(models) == 0 {
		return
	}
	providerID := models[0].Provider
	baseURL := strings.TrimRight(providerID, "/") + "/v1"
	m.mu.Lock()
	filtered := m.localModels[:0:0]
	for _, mod := range m.localModels {
		if mod.Provider != providerID {
			filtered = append(filtered, mod)
		}
	}
	m.localModels = append(filtered, models...)
	m.providerAPIs[providerID] = baseURL
	m.mu.Unlock()
}

func (m *Manager) RemoveLocalModels(providerID string) {
	m.mu.Lock()
	filtered := m.localModels[:0:0]
	for _, mod := range m.localModels {
		if mod.Provider != providerID {
			filtered = append(filtered, mod)
		}
	}
	m.localModels = filtered
	delete(m.providerAPIs, providerID)
	m.mu.Unlock()
}

func (m *Manager) Models() []Model {
	m.mu.RLock()
	cat := m.catalog
	local := m.localModels
	spettro := m.spettroModels
	m.mu.RUnlock()
	base := cat
	if len(base) == 0 {
		base = fallbackModels
	}
	out := make([]Model, 0, len(spettro)+len(base)+len(local))
	out = append(out, spettro...)
	out = append(out, base...)
	out = append(out, local...)
	return out
}

func (m *Manager) ConnectedModels(apiKeys map[string]string) []Model {
	var out []Model
	for _, mod := range m.Models() {
		if mod.Local {
			out = append(out, mod)
			continue
		}
		if key, ok := apiKeys[mod.Provider]; ok && key != "" {
			out = append(out, mod)
		}
	}
	return out
}

// HasCredentials reports whether providerID is usable with the given keys.
// Local endpoint providers (identified by an http(s) URL) need no key.
func HasCredentials(apiKeys map[string]string, providerID string) bool {
	if providerID == "" {
		return false
	}
	if strings.HasPrefix(providerID, "http://") || strings.HasPrefix(providerID, "https://") {
		return true
	}
	return strings.TrimSpace(apiKeys[providerID]) != ""
}

// PreferredModel picks the model to activate when none is configured or the
// configured one has no credentials: the first tool-capable connected model in
// display order — Spettro Subscription models first (the backend lists its
// default fast model first), then catalog providers with a key, then local
// endpoints.
func (m *Manager) PreferredModel(apiKeys map[string]string) (Model, bool) {
	connected := m.ConnectedModels(apiKeys)
	for _, mod := range connected {
		if mod.ToolCall {
			return mod, true
		}
	}
	if len(connected) > 0 {
		return connected[0], true
	}
	return Model{}, false
}

// ResolveActive keeps providerID/model when that provider has credentials and
// otherwise substitutes the preferred connected model. It returns empty
// strings when nothing at all is usable.
func (m *Manager) ResolveActive(providerID, model string, apiKeys map[string]string) (string, string) {
	if HasCredentials(apiKeys, providerID) {
		return providerID, model
	}
	if pref, ok := m.PreferredModel(apiKeys); ok {
		return pref.Provider, pref.Name
	}
	return "", ""
}

func (m *Manager) AllProviderInfos() []ProviderInfo {
	m.mu.RLock()
	cat := m.catalog
	m.mu.RUnlock()

	src := cat
	if len(src) == 0 {
		src = fallbackModels
	}

	seen := map[string]bool{}
	var out []ProviderInfo
	for _, mod := range src {
		if seen[mod.Provider] {
			continue
		}
		seen[mod.Provider] = true
		out = append(out, ProviderInfo{
			ID:   mod.Provider,
			Name: mod.ProviderName,
			Env:  mod.EnvKey,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ID == "anthropic" {
			return true
		}
		if out[j].ID == "anthropic" {
			return false
		}
		return out[i].ID < out[j].ID
	})
	return out
}

func (m *Manager) ProviderEnvKey(providerID string) string {
	for _, mod := range m.Models() {
		if mod.Provider == providerID && mod.EnvKey != "" {
			return mod.EnvKey
		}
	}
	return ""
}

func (m *Manager) ProviderNames() []string {
	seen := map[string]bool{}
	for _, mod := range m.Models() {
		seen[mod.Provider] = true
	}
	names := make([]string, 0, len(seen))
	for k := range seen {
		names = append(names, k)
	}
	sort.Slice(names, func(i, j int) bool {
		if names[i] == "anthropic" {
			return true
		}
		if names[j] == "anthropic" {
			return false
		}
		return names[i] < names[j]
	})
	return names
}

func (m *Manager) SupportsVision(providerName, modelName string) bool {
	for _, item := range m.Models() {
		if item.Provider == providerName && item.Name == modelName {
			return item.Vision
		}
	}
	return false
}

func (m *Manager) SupportsToolCalls(providerName, modelName string) bool {
	for _, item := range m.Models() {
		if item.Provider == providerName && item.Name == modelName {
			return item.ToolCall
		}
	}
	return false
}

func (m *Manager) ModelContext(providerName, modelName string) int {
	for _, item := range m.Models() {
		if item.Provider == providerName && item.Name == modelName {
			return item.Context
		}
	}
	return 0
}

// SupportsReasoning reports whether the thinking switcher should be offered
// for a model. Catalog entries are authoritative. Models the catalog cannot
// vouch for — local servers (whose /v1/models probe carries no reasoning
// flag) and custom endpoints not in the catalog at all — get best-effort
// true: OpenAI-compatible servers that don't know reasoning_effort ignore
// it, and ones that reject it trigger the downgrade ladder, so offering the
// switcher is safe and refusing it would lock out genuinely reasoning-capable
// local models. Spettro Subscription models are offered it unless the plan's
// model list marks them reasoning:false (NoReasoning): the inference proxy
// forwards reasoning_effort to its upstream, the list need not flag
// reasoning, and a rejection steps down the same ladder.
func (m *Manager) SupportsReasoning(providerName, modelName string) bool {
	for _, item := range m.Models() {
		if item.Provider == providerName && item.Name == modelName {
			if item.NoReasoning {
				return false
			}
			return item.Reasoning || item.Local || providerName == spettroProviderID
		}
	}
	return true
}

// ConfiguredThinking is the thinking level a run on the model sends for the
// user's thinking_level setting: the level itself when the model supports
// reasoning (see SupportsReasoning), "" (no thinking parameter) otherwise.
// Every host (TUI, headless, ACP) starts its runs from it, so the setting
// means the same everywhere: a token budget on Anthropic, reasoning_effort
// on OpenAI and OpenAI-compatible backends (the Spettro Subscription
// included).
func (m *Manager) ConfiguredThinking(providerName, modelName, level string) ThinkingLevel {
	level = strings.TrimSpace(level)
	if level == "" || !m.SupportsReasoning(providerName, modelName) {
		return ""
	}
	return ThinkingLevel(level)
}

// isLocalEndpoint reports whether the model is served by a local endpoint
// (a probed local server, or a provider identified by its URL).
func (m *Manager) isLocalEndpoint(providerName, modelName string) bool {
	if strings.HasPrefix(providerName, "http://") || strings.HasPrefix(providerName, "https://") {
		return true
	}
	for _, item := range m.Models() {
		if item.Provider == providerName && item.Name == modelName {
			return item.Local
		}
	}
	return false
}

func (m *Manager) HasModel(providerName, modelName string) bool {
	for _, item := range m.Models() {
		if item.Provider == providerName && item.Name == modelName {
			return true
		}
	}
	return false
}

// Send dispatches req and transparently waits out rate limits rather than
// surfacing them as errors. The only rate limit this currently applies to is
// the Spettro Subscription overflow tier: pro/max accounts get throttled onto
// a free-tier model once their credit budget is exhausted, and the backend
// returns 429 with a bounded Retry-After for that specific case. The waits
// back off exponentially with jitter (see nextRateLimitWait), so parallel
// sessions throttled together do not retry in lockstep, and they are
// bounded in time: once waiting would pass rateLimitMaxWait, the 429 is
// returned, wrapped in ErrRateLimitRetriesExhausted. Any other error
// (including 429s from other providers) is returned immediately.
func (m *Manager) Send(ctx context.Context, providerName, modelName string, req Request) (Response, error) {
	req.Thinking = m.rememberedThinking(providerName, modelName, req.Thinking)
	// limited counts the sends rate limited so far; waited, the time spent
	// waiting them out.
	limited := 0
	var waited time.Duration
	// unflaggedFrom is the level a bare 400 made Send drop on a Spettro
	// model the plan does not flag as reasoning (see
	// unflaggedThinkingFallback); remembered only if the retry succeeds.
	var unflaggedFrom ThinkingLevel
	for {
		resp, err := m.sendOnce(ctx, providerName, modelName, req)
		if err == nil {
			if unflaggedFrom != "" {
				m.recordEffortDowngrade(providerName, modelName, unflaggedFrom, "")
			}
			m.usageRec.record(providerName, modelName, resp.Usage)
			resp.Thinking = req.Thinking
			return resp, nil
		}
		// A model may reject the requested thinking level (e.g. an effort enum
		// value it doesn't define, or a thinking budget above its cap). Rather
		// than aborting the run, step the level down and retry so the user
		// keeps continuity; at "" no thinking parameter is sent at all.
		if next, ok := m.downgradedThinking(providerName, req.Thinking, err); ok {
			if !isAnthropicAPI(providerName, m.providerKind(providerName)) && isReasoningEffortError(err) {
				m.recordEffortDowngrade(providerName, modelName, req.Thinking, next)
			}
			req.Thinking = next
			continue
		}
		if unflaggedFrom == "" && m.unflaggedThinkingFallback(providerName, modelName, req.Thinking, err) {
			unflaggedFrom = req.Thinking
			req.Thinking = ""
			continue
		}
		retryAfter, ok := rateLimitRetryAfter(providerName, err)
		if !ok {
			return Response{}, err
		}
		limited++
		delay, ok := nextRateLimitWait(limited, waited, retryAfter)
		if !ok {
			return Response{}, fmt.Errorf("%w (%d attempts over %s): %w", ErrRateLimitRetriesExhausted, limited, waited.Round(time.Second), err)
		}
		waited += delay
		if req.OnRateLimit != nil {
			req.OnRateLimit(delay)
		}
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return Response{}, ctx.Err()
		}
	}
}

// imageOmittedNote is the placeholder substituted for images when the active
// model cannot consume vision input.
const imageOmittedNote = "[image omitted: the current model does not support vision]"

func imageOmittedText(n int) string {
	if n == 1 {
		return imageOmittedNote
	}
	return fmt.Sprintf("[%d images omitted: the current model does not support vision]", n)
}

// stripImages returns a copy of req with every image removed and replaced by a
// text placeholder, so a non-vision model can continue a chat whose history
// contains images. The caller's Request (and thus the stored chat history) is
// left untouched.
func stripImages(req Request) Request {
	out := req
	if n := len(req.Images); n > 0 {
		out.Images = nil
		if len(req.Messages) == 0 {
			out.Prompt = strings.TrimSpace(req.Prompt + "\n\n" + imageOmittedText(n))
		}
	}
	if len(req.Messages) == 0 {
		return out
	}
	out.Messages = make([]Message, len(req.Messages))
	copy(out.Messages, req.Messages)
	for i := range out.Messages {
		msg := &out.Messages[i]
		if n := len(msg.Images); n > 0 {
			msg.Images = nil
			msg.Content = strings.TrimSpace(msg.Content + "\n\n" + imageOmittedText(n))
		}
		if len(msg.ToolResults) == 0 {
			continue
		}
		trs := make([]ToolResult, len(msg.ToolResults))
		copy(trs, msg.ToolResults)
		for j := range trs {
			if n := len(trs[j].Images); n > 0 {
				trs[j].Images = nil
				trs[j].Output = strings.TrimSpace(trs[j].Output + "\n\n" + imageOmittedText(n))
			}
		}
		msg.ToolResults = trs
	}
	// Request-level images attach to the last user message in multi-turn mode;
	// note the omission there.
	if n := len(req.Images); n > 0 {
		for i, v := range slices.Backward(out.Messages) {
			if v.Role == RoleUser && len(v.ToolResults) == 0 {
				out.Messages[i].Content = strings.TrimSpace(v.Content + "\n\n" + imageOmittedText(n))
				break
			}
		}
	}
	return out
}

func (m *Manager) sendOnce(ctx context.Context, providerName, modelName string, req Request) (Response, error) {
	m.mu.RLock()
	apiKey := m.apiKeys[providerName]
	baseURL := m.providerAPIs[providerName]
	apiKind := m.providerKinds[providerName]
	streamAll := m.streamAll
	m.mu.RUnlock()
	if providerName == "anthropic" {
		apiKind = models.APIAnthropic
	}

	hasImages := len(req.Images) > 0
	for _, msg := range req.Messages {
		if len(msg.Images) > 0 {
			hasImages = true
			break
		}
		for _, tr := range msg.ToolResults {
			if len(tr.Images) > 0 {
				hasImages = true
				break
			}
		}
	}
	if hasImages && !m.SupportsVision(providerName, modelName) {
		// The chat may carry images from an earlier vision-capable model. Keep
		// the conversation usable: strip the images from this request only
		// (history retains them, so switching back restores vision) and leave a
		// text placeholder so the model knows something was omitted.
		req = stripImages(req)
	}

	var allParts []string
	if len(req.Messages) > 0 {
		allParts = append(allParts, req.System)
		for _, m := range req.Messages {
			allParts = append(allParts, m.Content)
		}
	} else {
		allParts = append(allParts, req.Prompt)
	}
	// The input budget (config token_budget) caps the PROMPT: estimate the
	// whole request, tool results and tool schemas included — they are most
	// of a coding session's context. The output cap is a separate field.
	promptTokens := EstimateRequestTokens(req)
	if req.InputBudget > 0 {
		if err := budget.CheckTokens(req.InputBudget, promptTokens); err != nil {
			return Response{}, err
		}
	}
	window := req.ContextWindow
	if window <= 0 {
		window = m.ModelContext(providerName, modelName)
	}
	req.MaxTokens = m.resolveMaxOutput(providerName, apiKind, modelName, req.MaxTokens, window, promptTokens)
	req.localEndpoint = m.isLocalEndpoint(providerName, modelName)

	// The fantasy path handles images natively (FilePart on user messages), so
	// vision requests take the same primary path as everything else — the
	// legacy adapters below are only the fallback, and they drop native tool
	// definitions, so detouring there would break tool use mid-run.
	//
	// Anthropic-protocol requests always stream, even when the caller wants
	// no live tokens: the SDK refuses non-streaming requests whose max_tokens
	// could take over 10 minutes (anything above ~21k), and the stream path
	// carries the idle watchdog that turns a stalled connection into a
	// retryable error instead of a hang. With streamAll every request does.
	anthropicAPI := isAnthropicAPI(providerName, apiKind)
	if req.OnStream != nil || anthropicAPI || streamAll {
		resp, err := sendWithFantasyStream(ctx, providerName, apiKind, modelName, apiKey, baseURL, req)
		if err == nil {
			return finalizeResponse(resp, providerName, modelName, allParts), nil
		}
		if !shouldFallbackToLegacy(err) {
			// Streaming failed. Only a failure that could be specific to the
			// stream endpoint (an unclassified error, or a 4xx that is not
			// auth, rate limit or context overflow) earns one non-streaming
			// attempt; transient, auth and overflow failures would fail the
			// same way and belong to the caller's retry/compaction policy.
			if anthropicAPI || !worthNonStreamingRetry(err) {
				return Response{}, err
			}
			noStream := req
			noStream.OnStream = nil
			if resp, rerr := sendWithFantasy(ctx, providerName, apiKind, modelName, apiKey, baseURL, noStream); rerr == nil {
				return finalizeResponse(resp, providerName, modelName, allParts), nil
			}
			return Response{}, err
		}
	} else {
		resp, err := sendWithFantasy(ctx, providerName, apiKind, modelName, apiKey, baseURL, req)
		if err == nil {
			return finalizeResponse(resp, providerName, modelName, allParts), nil
		}
		if !shouldFallbackToLegacy(err) {
			return Response{}, err
		}
	}

	adapter, err := legacyAdapterFor(providerName, apiKind, apiKey, baseURL)
	if err != nil {
		return Response{}, err
	}
	resp, err := adapter.Send(ctx, modelName, req)
	if err != nil {
		return Response{}, err
	}
	return finalizeResponse(resp, providerName, modelName, allParts), nil
}

// worthNonStreamingRetry reports whether a failed streaming request should
// be retried once without streaming (see sendOnce).
func worthNonStreamingRetry(err error) bool {
	switch ClassifyRetry(err) {
	case RetryTransient, RetryContextOverflow:
		return false
	}
	if status, _, ok := httpErrorDetails(err); ok && (status == http.StatusUnauthorized || status == http.StatusForbidden) {
		return false
	}
	return !isThinkingLevelError(err)
}

// downgradedThinking decides whether err is worth retrying at a lower
// thinking level and returns that level. Only errors that plausibly complain
// about the reasoning/thinking parameter qualify — anything else must surface
// unchanged. For providers on the reasoning_effort wire format the level is
// stepped down until the wire value actually changes (max and x-high both
// serialize to "xhigh", so retrying between them would waste a request).
func (m *Manager) downgradedThinking(providerName string, level ThinkingLevel, err error) (ThinkingLevel, bool) {
	// An explicit "off" is on the ladder too: it sends reasoning_effort
	// "none", and a server that rejects that value gets a retry with the
	// parameter omitted entirely (NextLowerThinking(off) == "").
	if level == "" || !isThinkingLevelError(err) {
		return "", false
	}
	m.mu.RLock()
	apiKind := m.providerKinds[providerName]
	m.mu.RUnlock()
	next := NextLowerThinking(level)
	if !isAnthropicAPI(providerName, apiKind) {
		for next != "" && ReasoningEffort(next) == ReasoningEffort(level) {
			next = NextLowerThinking(next)
		}
	}
	return next, true
}

func effortDowngradeKey(providerName, modelName string, level ThinkingLevel) string {
	return providerName + "\x00" + modelName + "\x00" + string(level)
}

// recordEffortDowngrade records that the model rejected level and Send
// stepped down to next. Only reasoning_effort rejections are recorded: they
// are about the values the model accepts, the same on every call. Anthropic
// thinking errors can depend on the request (a budget against its
// max_tokens, a history without thinking blocks), so those are retried fresh
// each time.
func (m *Manager) recordEffortDowngrade(providerName, modelName string, level, next ThinkingLevel) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.effortDowngrades == nil {
		m.effortDowngrades = map[string]ThinkingLevel{}
	}
	m.effortDowngrades[effortDowngradeKey(providerName, modelName, level)] = next
}

func (m *Manager) providerKind(providerName string) string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.providerKinds[providerName]
}

// unflaggedThinkingFallback reports whether a send to a Spettro Subscription
// model that the plan does not flag as reasoning should be retried once
// without the thinking parameter. Such a model gets reasoning_effort on
// trust (see SupportsReasoning), and the proxy may pass an upstream's
// rejection of it on as a bare 400 that no longer names the parameter, which
// downgradedThinking cannot recognize. When the retry succeeds, Send
// remembers that the model takes no reasoning_effort.
func (m *Manager) unflaggedThinkingFallback(providerName, modelName string, level ThinkingLevel, err error) bool {
	if providerName != spettroProviderID || level == "" || ClassifyRetry(err) != RetryNever {
		return false
	}
	if status, _, ok := httpErrorDetails(err); !ok || status != http.StatusBadRequest {
		return false
	}
	for _, item := range m.Models() {
		if item.Provider == providerName && item.Name == modelName {
			return !item.Reasoning
		}
	}
	return true
}

// rememberedThinking returns the level a send at level starts from: level
// itself, or the lower level an earlier send settled on after the model
// rejected it (see recordEffortDowngrade).
func (m *Manager) rememberedThinking(providerName, modelName string, level ThinkingLevel) ThinkingLevel {
	m.mu.RLock()
	defer m.mu.RUnlock()
	// Each remembered step goes strictly down the ladder, so this ends; the
	// bound only guards against a corrupted map.
	for range 8 {
		if level == "" {
			return level
		}
		next, ok := m.effortDowngrades[effortDowngradeKey(providerName, modelName, level)]
		if !ok {
			return level
		}
		level = next
	}
	return level
}

// isReasoningEffortError reports whether err is a backend rejecting the
// reasoning_effort parameter or its value.
func isReasoningEffortError(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "reasoning_effort") || strings.Contains(msg, "reasoning.effort") || strings.Contains(msg, "reasoning effort")
}

// isThinkingLevelError reports whether err looks like a provider rejecting
// the reasoning/thinking configuration (as opposed to auth, rate limit, or
// any other failure).
func isThinkingLevelError(err error) bool {
	if err == nil {
		return false
	}
	if isReasoningEffortError(err) {
		return true
	}
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "budget_tokens") || strings.Contains(msg, "thinking.enabled") || strings.Contains(msg, "extended thinking") {
		return true
	}
	// "When `thinking` is enabled, a final `assistant` message must start
	// with a thinking block": the history cannot satisfy thinking, so step
	// down (to off) rather than fail the run.
	if strings.Contains(msg, "must start with a thinking block") {
		return true
	}
	return false
}

// Rate-limit waits in Manager.Send. The wait before retry n is
// rateLimitBaseDelay doubled n-1 times, capped at the server's Retry-After
// when it sends one (but never below rateLimitBaseDelay) and at
// rateLimitMaxDelay otherwise, with equal jitter (half fixed, half random)
// so clients throttled together spread out even at the cap. A Retry-After
// is the refill time of the backend's bucket, a worst case: an early retry
// often gets through, and a late one only wastes time.
//
// The retries are bounded in time, not in count: early retries are cheap,
// and a throttled request should keep its place for as long as the bound
// allows (a sub-agent that gives up is re-run from scratch or fails, its
// progress lost). Past rateLimitMaxWait of waiting the 429 is returned.
const rateLimitMaxDelay = 20 * time.Second

// rateLimitBaseDelay and rateLimitMaxWait are variables so tests can shrink
// them.
var (
	rateLimitBaseDelay = time.Second
	rateLimitMaxWait   = 3 * time.Minute
)

// ErrRateLimitRetriesExhausted wraps the 429 Manager.Send returns once it
// has stopped waiting out a rate limit. Neither the agent loop nor the
// ultra and workflow sub-agent runners retry it again (see ClassifyRetry):
// the waiting already happened here.
var ErrRateLimitRetriesExhausted = errors.New("rate limited: gave up retrying")

// rateLimitJitter returns a random fraction in [0, 1); swappable in tests.
var rateLimitJitter = rand.Float64

// rateLimitDelay is the wait before retrying a request that has now been
// rate limited attempts times (1 after the first 429). retryAfter is the
// server's hint, 0 when it sent none.
func rateLimitDelay(attempts int, retryAfter time.Duration) time.Duration {
	ceiling := rateLimitMaxDelay
	if retryAfter > 0 {
		ceiling = max(retryAfter, rateLimitBaseDelay)
	}
	d := rateLimitBaseDelay << min(max(attempts-1, 0), 16)
	d = min(d, ceiling)
	return d/2 + time.Duration(rateLimitJitter()*float64(d/2))
}

// nextRateLimitWait reports how long to wait before retrying a request that
// has now been rate limited attempts times after waiting waited in all, or
// false once that wait would take the total past rateLimitMaxWait.
func nextRateLimitWait(attempts int, waited, retryAfter time.Duration) (time.Duration, bool) {
	delay := rateLimitDelay(attempts, retryAfter)
	if waited+delay > rateLimitMaxWait {
		return 0, false
	}
	return delay, true
}

// rateLimitRetryAfter reports whether err is a rate limit the CLI should
// wait out, with the server's Retry-After (0 when it sent none). Only the
// Spettro Subscription provider is eligible: it is the sole source of the
// overflow-tier 429 (pro/max accounts throttled onto a free model once their
// budget is exhausted), which resolves on its own within seconds. 429s from
// any other provider are treated as ordinary errors (the agent loop's
// RetryPolicy handles them).
func rateLimitRetryAfter(providerName string, err error) (time.Duration, bool) {
	if providerName != spettroProviderID {
		return 0, false
	}
	statusCode, header, ok := httpErrorDetails(err)
	if !ok || statusCode != http.StatusTooManyRequests {
		return 0, false
	}
	hint, _ := parseRetryAfter(header)
	return hint, true
}

// httpErrorDetails unwraps err looking for the HTTP status code and response
// headers of the failed request, checking both the fantasy SDK's wrapper
// (used by the streaming/non-streaming Spettro path) and the raw openai-go
// error (used by the legacy adapter path, e.g. when images are attached).
func httpErrorDetails(err error) (statusCode int, header http.Header, ok bool) {
	if providerErr, ok := errors.AsType[*fantasy.ProviderError](err); ok {
		h := make(http.Header, len(providerErr.ResponseHeaders))
		for k, v := range providerErr.ResponseHeaders {
			h.Set(k, v)
		}
		return providerErr.StatusCode, h, true
	}
	var apiErr *openai.Error
	if errors.As(err, &apiErr) && apiErr.Response != nil {
		return apiErr.StatusCode, apiErr.Response.Header, true
	}
	return 0, nil, false
}

func legacyAdapterFor(providerName, apiKind, apiKey, baseURL string) (Adapter, error) {
	if apiKind == models.APIAnthropic || providerName == "anthropic" {
		if providerName == "anthropic" {
			baseURL = "" // official endpoint, let the SDK use its default
		}
		return AnthropicAdapter{APIKey: apiKey, BaseURL: baseURL}, nil
	}
	resolvedBaseURL, err := resolveOpenAICompatibleBaseURL(providerName, baseURL)
	if err != nil {
		return nil, err
	}
	if apiKey == "" {
		apiKey = "local"
	}
	return OpenAICompatibleAdapter{APIKey: apiKey, BaseURL: resolvedBaseURL}, nil
}

func resolveOpenAICompatibleBaseURL(providerName, baseURL string) (string, error) {
	if baseURL != "" {
		return baseURL, nil
	}
	if strings.HasPrefix(providerName, "http://") || strings.HasPrefix(providerName, "https://") {
		return strings.TrimRight(providerName, "/") + "/v1", nil
	}
	if providerName == "openai" || providerName == "openai-compatible" {
		return "", nil
	}
	return "", fmt.Errorf("no API endpoint configured for provider %q", providerName)
}

func finalizeResponse(resp Response, providerName, modelName string, allParts []string) Response {
	resp.Provider = providerName
	resp.Model = modelName
	if resp.EstimatedTokens == 0 {
		resp.EstimatedTokens = budget.EstimateTokens(allParts...)
	}
	return resp
}

// VerifyKey checks that apiKey is accepted by the provider using a lightweight
// GET request against the provider's models (or equivalent) endpoint.
// Ported from CRUSH's TestConnection logic.
func (m *Manager) VerifyKey(ctx context.Context, providerID, apiKey string) error {
	m.mu.RLock()
	baseURL := m.providerAPIs[providerID]
	apiKind := m.providerKinds[providerID]
	m.mu.RUnlock()

	if baseURL == "" {
		if strings.HasPrefix(providerID, "http://") || strings.HasPrefix(providerID, "https://") {
			baseURL = strings.TrimRight(providerID, "/") + "/v1"
		} else {
			baseURL = "https://api.openai.com/v1"
		}
	}
	base := strings.TrimRight(baseURL, "/")

	var testURL string
	headers := map[string]string{}
	lenient := false // when true, only 401 counts as failure

	switch {
	case providerID == "anthropic" || apiKind == models.APIAnthropic:
		root := "https://api.anthropic.com"
		if providerID != "anthropic" && baseURL != "" {
			// Strip trailing /v1 if present (the Anthropic SDK path
			// already includes it; we only need the API root).
			root = strings.TrimSuffix(strings.TrimRight(baseURL, "/"), "/v1")
		}
		testURL = root + "/v1/models"
		headers["x-api-key"] = apiKey
		headers["anthropic-version"] = "2023-06-01"

	case providerID == "google":
		// Google's native endpoint uses a query-param key, not a Bearer header.
		testURL = "https://generativelanguage.googleapis.com/v1beta/models?key=" + url.QueryEscape(apiKey)

	case providerID == "openrouter":
		// OpenRouter exposes /credits for validation instead of /models.
		testURL = base + "/credits"
		headers["Authorization"] = "Bearer " + apiKey

	case providerID == "zai":
		// ZAI returns non-200 for unauthenticated requests but not a clean 401,
		// so only treat 401 as a hard failure.
		testURL = base + "/models"
		headers["Authorization"] = "Bearer " + apiKey
		lenient = true

	default:
		testURL = base + "/models"
		headers["Authorization"] = "Bearer " + apiKey
	}

	tctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(tctx, http.MethodGet, testURL, nil)
	if err != nil {
		return fmt.Errorf("failed to build request: %w", err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("connection failed: %w", err)
	}
	defer resp.Body.Close()

	if lenient {
		if resp.StatusCode == http.StatusUnauthorized {
			return fmt.Errorf("key rejected (401)")
		}
		return nil
	}
	if resp.StatusCode == http.StatusOK {
		return nil
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	return fmt.Errorf("key rejected (%d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
}
