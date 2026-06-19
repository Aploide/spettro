package provider

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

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
}

func NewManager() *Manager {
	return &Manager{
		apiKeys:      map[string]string{},
		providerAPIs: map[string]string{},
	}
}

func (m *Manager) SetAPIKeys(keys map[string]string) {
	m.mu.Lock()
	m.apiKeys = make(map[string]string, len(keys))
	for k, v := range keys {
		m.apiKeys[k] = v
	}
	m.mu.Unlock()
}

func (m *Manager) SetCatalog(cat models.Catalog) {
	built := buildModels(cat)
	apis := make(map[string]string, len(cat))
	for id, prov := range cat {
		if prov.API != "" {
			apis[id] = prov.API
		}
	}
	m.mu.Lock()
	m.catalog = built
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

func (m *Manager) SupportsReasoning(providerName, modelName string) bool {
	for _, item := range m.Models() {
		if item.Provider == providerName && item.Name == modelName {
			return item.Reasoning
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

func (m *Manager) Send(ctx context.Context, providerName, modelName string, req Request) (Response, error) {
	m.mu.RLock()
	apiKey := m.apiKeys[providerName]
	baseURL := m.providerAPIs[providerName]
	m.mu.RUnlock()

	if len(req.Images) > 0 && !m.SupportsVision(providerName, modelName) {
		return Response{}, fmt.Errorf("model does not support vision: %s/%s", providerName, modelName)
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
	allParts = append(allParts, req.Images...)
	if err := budget.Validate(req.MaxTokens, allParts...); err != nil {
		return Response{}, err
	}

	if len(req.Images) == 0 {
		if req.OnStream != nil {
			resp, err := sendWithFantasyStream(ctx, providerName, modelName, apiKey, baseURL, req)
			if err == nil {
				return finalizeResponse(resp, providerName, modelName, allParts), nil
			}
			if !shouldFallbackToLegacy(err) {
				// Streaming failed for a non-fallback reason (e.g. the provider
				// does not support the stream endpoint). Retry once without
				// streaming before surfacing the error so a run never dies just
				// because live tokens were unavailable.
				noStream := req
				noStream.OnStream = nil
				if resp, rerr := sendWithFantasy(ctx, providerName, modelName, apiKey, baseURL, noStream); rerr == nil {
					return finalizeResponse(resp, providerName, modelName, allParts), nil
				}
				return Response{}, err
			}
		} else {
			resp, err := sendWithFantasy(ctx, providerName, modelName, apiKey, baseURL, req)
			if err == nil {
				return finalizeResponse(resp, providerName, modelName, allParts), nil
			}
			if !shouldFallbackToLegacy(err) {
				return Response{}, err
			}
		}
	}

	adapter, err := legacyAdapterFor(providerName, apiKey, baseURL)
	if err != nil {
		return Response{}, err
	}
	resp, err := adapter.Send(ctx, modelName, req)
	if err != nil {
		return Response{}, err
	}
	return finalizeResponse(resp, providerName, modelName, allParts), nil
}

func legacyAdapterFor(providerName, apiKey, baseURL string) (Adapter, error) {
	if providerName == "anthropic" {
		return AnthropicAdapter{APIKey: apiKey}, nil
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
	if known, ok := knownBaseURLs[providerName]; ok {
		return known, nil
	}
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
	m.mu.RUnlock()

	if baseURL == "" {
		if known, ok := knownBaseURLs[providerID]; ok {
			baseURL = known
		} else if strings.HasPrefix(providerID, "http://") || strings.HasPrefix(providerID, "https://") {
			baseURL = strings.TrimRight(providerID, "/") + "/v1"
		} else {
			baseURL = "https://api.openai.com/v1"
		}
	}
	base := strings.TrimRight(baseURL, "/")

	var testURL string
	headers := map[string]string{}
	lenient := false // when true, only 401 counts as failure

	switch providerID {
	case "anthropic":
		testURL = "https://api.anthropic.com/v1/models"
		headers["x-api-key"] = apiKey
		headers["anthropic-version"] = "2023-06-01"

	case "google":
		// Google's native endpoint uses a query-param key, not a Bearer header.
		testURL = "https://generativelanguage.googleapis.com/v1beta/models?key=" + url.QueryEscape(apiKey)

	case "openrouter":
		// OpenRouter exposes /credits for validation instead of /models.
		testURL = base + "/credits"
		headers["Authorization"] = "Bearer " + apiKey

	case "zai":
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
