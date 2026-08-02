package acp

import (
	"context"
	"slices"
	"sort"
	"strings"
	"time"

	"spettro/internal/config"
	"spettro/internal/provider"
	"spettro/internal/spettro"
)

// Provider and model management over ACP — the wire equivalent of the TUI's
// /connect and /models dialogs (internal/tui/dialog_connect.go).
//
// API keys travel one way only: the client posts a key to connect a provider,
// and reads back nothing but a Connected flag. Keys live in the encrypted
// store (~/.spettro/keys.enc) exactly as they do for the TUI, so neither the
// protocol nor the client ever has to hold them.

// suggestedProviders is the TUI's featured ordering, repeated here so both
// front-ends put the same providers on top.
var suggestedProviders = []string{"anthropic", "openai", "mistral", "x-ai", "zai"}

// ProviderEntry is one row of the connect list.
type ProviderEntry struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// EnvKey is the environment variable this provider's key can come from.
	EnvKey string `json:"envKey,omitempty"`
	// Connected is true when a key is stored (or supplied via the environment).
	Connected bool `json:"connected"`
	// Suggested marks the featured providers the TUI lists first.
	Suggested bool `json:"suggested"`
	// ModelCount is how many catalog models this provider exposes.
	ModelCount int `json:"modelCount"`
}

// LocalEndpointEntry is one connected OpenAI-compatible local server.
type LocalEndpointEntry struct {
	Endpoint string `json:"endpoint"`
	// Name is the friendly server name ("LM Studio", "Ollama", ...).
	Name string `json:"name"`
	// HasKey reports whether this endpoint was saved with an API key.
	HasKey     bool `json:"hasKey"`
	ModelCount int  `json:"modelCount"`
}

// ProvidersListResult is the reply to `_spettro/providers/list`.
type ProvidersListResult struct {
	Providers []ProviderEntry      `json:"providers"`
	Local     []LocalEndpointEntry `json:"local"`
	// Subscription is the Spettro-managed provider, surfaced separately
	// because it is connected by signing in rather than by pasting a key.
	Subscription ProviderEntry `json:"subscription"`
}

// ModelEntry is one selectable model.
type ModelEntry struct {
	Provider     string `json:"provider"`
	ProviderName string `json:"providerName"`
	Name         string `json:"name"`
	DisplayName  string `json:"displayName"`
	Vision       bool   `json:"vision"`
	Reasoning    bool   `json:"reasoning"`
	ToolCall     bool   `json:"toolCall"`
	Context      int    `json:"context"`
	Local        bool   `json:"local"`
	Favorite     bool   `json:"favorite"`
	Active       bool   `json:"active"`
}

// ModelsListResult is the reply to `_spettro/models/list`.
type ModelsListResult struct {
	Models []ModelEntry `json:"models"`
	// ActiveProvider/ActiveModel identify the current selection, which may be
	// absent from Models when its provider was disconnected.
	ActiveProvider string `json:"activeProvider,omitempty"`
	ActiveModel    string `json:"activeModel,omitempty"`
}

// providersList enumerates every known provider with its connection state.
func (b *bridge) providersList() (ProvidersListResult, error) {
	cfg, err := config.LoadFull()
	if err != nil {
		return ProvidersListResult{}, extError("could not read configuration: %v", err)
	}

	modelsByProvider := map[string]int{}
	for _, m := range b.opts.Providers.Models() {
		modelsByProvider[m.Provider]++
	}

	out := ProvidersListResult{
		Subscription: ProviderEntry{
			ID:         spettro.ProviderID,
			Name:       spettro.ProviderName,
			Connected:  strings.TrimSpace(cfg.APIKeys[spettro.ProviderID]) != "",
			ModelCount: modelsByProvider[spettro.ProviderID],
		},
	}

	for _, pi := range b.opts.Providers.AllProviderInfos() {
		if pi.ID == spettro.ProviderID {
			continue // reported as Subscription
		}
		out.Providers = append(out.Providers, ProviderEntry{
			ID:         pi.ID,
			Name:       pi.Name,
			EnvKey:     pi.Env,
			Connected:  provider.HasCredentials(cfg.APIKeys, pi.ID),
			Suggested:  slices.Contains(suggestedProviders, pi.ID),
			ModelCount: modelsByProvider[pi.ID],
		})
	}

	// Featured providers first (in the TUI's order), then the rest
	// alphabetically, so the list is stable across calls.
	rank := func(id string) int {
		if i := slices.Index(suggestedProviders, id); i >= 0 {
			return i
		}
		return len(suggestedProviders)
	}
	sort.SliceStable(out.Providers, func(i, j int) bool {
		ri, rj := rank(out.Providers[i].ID), rank(out.Providers[j].ID)
		if ri != rj {
			return ri < rj
		}
		return out.Providers[i].Name < out.Providers[j].Name
	})

	for _, endpoint := range cfg.LocalEndpoints {
		normalized := provider.LocalProviderName(endpoint)
		out.Local = append(out.Local, LocalEndpointEntry{
			Endpoint:   endpoint,
			Name:       normalized,
			HasKey:     strings.TrimSpace(cfg.APIKeys[endpoint]) != "",
			ModelCount: modelsByProvider[endpoint],
		})
	}
	return out, nil
}

// ConnectProviderParams is the payload of `_spettro/providers/connect`.
type ConnectProviderParams struct {
	ProviderID string `json:"providerId"`
	APIKey     string `json:"apiKey"`
	// Activate switches the active model to one of this provider's models
	// once the key verifies. Used by first-run onboarding.
	Activate bool `json:"activate,omitempty"`
}

// ConnectProviderResult reports the outcome of a connect attempt.
type ConnectProviderResult struct {
	Connected  bool `json:"connected"`
	ModelCount int  `json:"modelCount"`
	// ActiveModel is set when Activate was requested and a model was chosen.
	ActiveModel string `json:"activeModel,omitempty"`
}

// providersConnect verifies an API key against the provider's own API and, on
// success, saves it to the encrypted store — the same two-step the TUI's
// onboarding performs, so a bad key is never persisted.
func (b *bridge) providersConnect(ctx context.Context, p ConnectProviderParams) (ConnectProviderResult, error) {
	id := strings.TrimSpace(p.ProviderID)
	key := strings.TrimSpace(p.APIKey)
	if id == "" {
		return ConnectProviderResult{}, extError("providerId is required")
	}
	if key == "" {
		return ConnectProviderResult{}, extError("an API key is required")
	}
	if id == spettro.ProviderID {
		return ConnectProviderResult{}, extError("the Spettro Subscription is connected by signing in, not with an API key")
	}

	verifyCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := b.opts.Providers.VerifyKey(verifyCtx, id, key); err != nil {
		return ConnectProviderResult{}, extError("%v", err)
	}
	if err := config.SaveAPIKey(id, key); err != nil {
		return ConnectProviderResult{}, extError("could not save the key: %v", err)
	}

	keys, err := config.LoadAPIKeys()
	if err != nil {
		return ConnectProviderResult{}, extError("could not reload keys: %v", err)
	}
	b.opts.Providers.SetAPIKeys(keys)

	result := ConnectProviderResult{Connected: true}
	for _, m := range b.opts.Providers.Models() {
		if m.Provider == id {
			result.ModelCount++
		}
	}

	if p.Activate {
		if _, err := config.Update(func(c *config.UserConfig) error {
			c.APIKeys = keys
			p, m := b.opts.Providers.ResolveActive(id, "", keys)
			c.ActiveProvider, c.ActiveModel = p, m
			result.ActiveModel = m
			return nil
		}); err != nil {
			return result, extError("key saved, but the active model could not be set: %v", err)
		}
	}
	return result, nil
}

// DisconnectProviderParams is the payload of `_spettro/providers/disconnect`.
type DisconnectProviderParams struct {
	ProviderID string `json:"providerId"`
}

// providersDisconnect forgets a provider's stored key.
func (b *bridge) providersDisconnect(_ context.Context, p DisconnectProviderParams) (ProvidersListResult, error) {
	id := strings.TrimSpace(p.ProviderID)
	if id == "" {
		return ProvidersListResult{}, extError("providerId is required")
	}
	if id == spettro.ProviderID {
		// Route through the account path so plan state and models are
		// cleared too, instead of leaving a half-signed-out account.
		if _, err := b.accountLogout(); err != nil {
			return ProvidersListResult{}, err
		}
		return b.providersList()
	}
	if err := config.RemoveAPIKey(id); err != nil {
		return ProvidersListResult{}, extError("could not remove the key: %v", err)
	}
	keys, err := config.LoadAPIKeys()
	if err != nil {
		return ProvidersListResult{}, extError("could not reload keys: %v", err)
	}
	b.opts.Providers.SetAPIKeys(keys)

	if _, err := config.Update(func(c *config.UserConfig) error {
		delete(c.APIKeys, id)
		// Don't leave the session pointed at a provider we can no longer
		// authenticate against.
		if c.ActiveProvider == id {
			c.ActiveProvider, c.ActiveModel = b.opts.Providers.ResolveActive("", "", c.APIKeys)
		}
		return nil
	}); err != nil {
		return ProvidersListResult{}, extError("could not update configuration: %v", err)
	}
	return b.providersList()
}

// LocalEndpointParams is the payload of the local-endpoint methods.
type LocalEndpointParams struct {
	Endpoint string `json:"endpoint"`
	// APIKey is optional: local servers started with authentication
	// (e.g. `llama-server --api-key`) need one, most don't.
	APIKey string `json:"apiKey,omitempty"`
}

// LocalProbeResult lists what an endpoint advertised, without saving it.
type LocalProbeResult struct {
	Endpoint string       `json:"endpoint"`
	Name     string       `json:"name"`
	Models   []ModelEntry `json:"models"`
}

// probeLocal runs the shared probe both localProbe and localAdd need, and
// returns the discovered models plus the normalized endpoint (the form used
// as the local provider ID and as the encrypted store's key name).
func (b *bridge) probeLocal(ctx context.Context, p LocalEndpointParams) (models []provider.Model, normalized string, err error) {
	endpoint := strings.TrimSpace(p.Endpoint)
	if endpoint == "" {
		return nil, "", extError("an endpoint URL is required")
	}
	probeCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	models, err = provider.ProbeLocalServer(probeCtx, endpoint, strings.TrimSpace(p.APIKey))
	if err != nil {
		return nil, "", extError("%v", err)
	}
	if len(models) == 0 {
		return nil, "", extError("that endpoint returned no models")
	}
	return models, models[0].Provider, nil
}

// localProbe queries an OpenAI-compatible server's /v1/models so the client
// can show what it found before the user commits to adding it.
func (b *bridge) localProbe(ctx context.Context, p LocalEndpointParams) (LocalProbeResult, error) {
	models, normalized, err := b.probeLocal(ctx, p)
	if err != nil {
		return LocalProbeResult{}, err
	}
	out := LocalProbeResult{
		Endpoint: normalized,
		Name:     provider.LocalProviderName(strings.TrimSpace(p.Endpoint)),
	}
	for _, m := range models {
		out.Models = append(out.Models, modelEntry(m, nil, "", ""))
	}
	return out, nil
}

// localAdd probes and then persists a local endpoint, registering its models.
func (b *bridge) localAdd(ctx context.Context, p LocalEndpointParams) (ProvidersListResult, error) {
	models, normalized, err := b.probeLocal(ctx, p)
	if err != nil {
		return ProvidersListResult{}, err
	}
	b.opts.Providers.AddLocalModels(models)
	if key := strings.TrimSpace(p.APIKey); key != "" {
		if err := config.SaveAPIKey(normalized, key); err != nil {
			return ProvidersListResult{}, extError("could not save the endpoint key: %v", err)
		}
	} else {
		_ = config.RemoveAPIKey(normalized)
	}

	if _, err := config.Update(func(c *config.UserConfig) error {
		if !slices.Contains(c.LocalEndpoints, normalized) {
			c.LocalEndpoints = append(c.LocalEndpoints, normalized)
		}
		if c.ActiveProvider == "" && len(models) > 0 {
			c.ActiveProvider, c.ActiveModel = models[0].Provider, models[0].Name
		}
		return nil
	}); err != nil {
		return ProvidersListResult{}, extError("could not save the endpoint: %v", err)
	}
	if keys, err := config.LoadAPIKeys(); err == nil {
		b.opts.Providers.SetAPIKeys(keys)
	}
	return b.providersList()
}

// localRemove forgets a local endpoint and unregisters its models.
func (b *bridge) localRemove(_ context.Context, p LocalEndpointParams) (ProvidersListResult, error) {
	endpoint := strings.TrimSpace(p.Endpoint)
	if endpoint == "" {
		return ProvidersListResult{}, extError("an endpoint URL is required")
	}
	b.opts.Providers.RemoveLocalModels(endpoint)
	_ = config.RemoveAPIKey(endpoint)

	if _, err := config.Update(func(c *config.UserConfig) error {
		c.LocalEndpoints = slices.DeleteFunc(c.LocalEndpoints, func(e string) bool { return e == endpoint })
		delete(c.APIKeys, endpoint)
		if c.ActiveProvider == endpoint {
			c.ActiveProvider, c.ActiveModel = b.opts.Providers.ResolveActive("", "", c.APIKeys)
		}
		return nil
	}); err != nil {
		return ProvidersListResult{}, extError("could not update configuration: %v", err)
	}
	if keys, err := config.LoadAPIKeys(); err == nil {
		b.opts.Providers.SetAPIKeys(keys)
	}
	return b.providersList()
}

// modelsList returns every model reachable with the current credentials,
// flagged with favorite/active state — the data behind the TUI's /models
// dialog.
func (b *bridge) modelsList() (ModelsListResult, error) {
	cfg, err := config.LoadFull()
	if err != nil {
		return ModelsListResult{}, extError("could not read configuration: %v", err)
	}
	favorites := map[string]bool{}
	for _, f := range cfg.Favorites {
		favorites[f] = true
	}

	out := ModelsListResult{
		ActiveProvider: cfg.ActiveProvider,
		ActiveModel:    cfg.ActiveModel,
	}
	for _, m := range b.opts.Providers.ConnectedModels(cfg.APIKeys) {
		out.Models = append(out.Models, modelEntry(m, favorites, cfg.ActiveProvider, cfg.ActiveModel))
	}
	return out, nil
}

// FavoriteModelParams is the payload of `_spettro/models/favorite`.
type FavoriteModelParams struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Favorite bool   `json:"favorite"`
}

// modelsFavorite toggles a model's favorite flag (the TUI's `f` key in
// /models, and what F2 cycles through).
func (b *bridge) modelsFavorite(_ context.Context, p FavoriteModelParams) (ModelsListResult, error) {
	providerID := strings.TrimSpace(p.Provider)
	modelName := strings.TrimSpace(p.Model)
	if providerID == "" || modelName == "" {
		return ModelsListResult{}, extError("provider and model are required")
	}
	ref := providerID + ":" + modelName

	if _, err := config.Update(func(c *config.UserConfig) error {
		has := slices.Contains(c.Favorites, ref)
		switch {
		case p.Favorite && !has:
			c.Favorites = append(c.Favorites, ref)
		case !p.Favorite && has:
			c.Favorites = slices.DeleteFunc(c.Favorites, func(f string) bool { return f == ref })
		}
		return nil
	}); err != nil {
		return ModelsListResult{}, extError("could not update favorites: %v", err)
	}
	return b.modelsList()
}

// modelEntry converts a provider model to its wire form. favorites may be nil
// when the caller has no favorite state to apply (e.g. a probe preview).
func modelEntry(m provider.Model, favorites map[string]bool, activeProvider, activeModel string) ModelEntry {
	display := m.DisplayName
	if display == "" {
		display = m.Name
	}
	return ModelEntry{
		Provider:     m.Provider,
		ProviderName: m.ProviderName,
		Name:         m.Name,
		DisplayName:  display,
		Vision:       m.Vision,
		Reasoning:    m.Reasoning,
		ToolCall:     m.ToolCall,
		Context:      m.Context,
		Local:        m.Local,
		Favorite:     favorites[m.Provider+":"+m.Name],
		Active:       m.Provider == activeProvider && m.Name == activeModel,
	}
}
