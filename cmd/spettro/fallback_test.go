package main

import (
	"context"
	"testing"

	"spettro/internal/config"
	"spettro/internal/provider"
)

// The TUI saves the model it falls back to, so when only a local endpoint
// can supply one it must wait for discovery; otherwise it must not.
func TestFallbackNeedsDiscovery(t *testing.T) {
	local := fakeModelsServer(t, "local-model", 0)
	catalogKeyed := provider.NewManager() // fallback catalog models
	cases := []struct {
		name string
		cfg  config.UserConfig
		pm   *provider.Manager
		want bool
	}{
		{"no local endpoints", config.UserConfig{ActiveProvider: "openai", APIKeys: map[string]string{}}, provider.NewManager(), false},
		{"configured provider usable", config.UserConfig{ActiveProvider: "openai", APIKeys: map[string]string{"openai": "k"}, LocalEndpoints: []string{local}}, catalogKeyed, false},
		{"another provider connected", config.UserConfig{ActiveProvider: "openai", APIKeys: map[string]string{"anthropic": "k"}, LocalEndpoints: []string{local}}, catalogKeyed, false},
		{"only a local endpoint", config.UserConfig{ActiveProvider: "openai", APIKeys: map[string]string{}, LocalEndpoints: []string{local}}, provider.NewManager(), true},
		{"nothing configured yet", config.UserConfig{APIKeys: map[string]string{}, LocalEndpoints: []string{local}}, provider.NewManager(), true},
	}
	for _, c := range cases {
		if got := fallbackNeedsDiscovery(c.cfg, c.pm); got != c.want {
			t.Errorf("%s: fallbackNeedsDiscovery = %v, want %v", c.name, got, c.want)
		}
	}

	// And after the wait the fallback is the local model, as before
	// discovery moved to the background.
	cfg := cases[3].cfg
	pm := provider.NewManager()
	d := startModelDiscovery(context.Background(), cfg, pm, false)
	if fallbackNeedsDiscovery(cfg, pm) {
		d.Wait(sessionModelsWait)
	}
	prov, model := pm.ResolveActive(cfg.ActiveProvider, cfg.ActiveModel, cfg.APIKeys)
	if prov != provider.LocalProviderID(local) || model != "local-model" {
		t.Fatalf("fallback = %q/%q, want the local model", prov, model)
	}
}
