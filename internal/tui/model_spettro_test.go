package tui

import (
	"testing"

	"spettro/internal/spettro"
)

// TestHandleSpettroLoaded_FromLogin verifies that a successful login load
// registers the models, caches the plan, sets the active model, and dismisses
// the login/onboarding overlays.
func TestHandleSpettroLoaded_FromLogin(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // isolate config + key writes

	m := NewModelForTesting()
	m.showLogin = true
	m.showOnboarding = true
	m.login = loginState{step: "loading", fromOnboarding: true}

	models := spettroModelsToProvider([]spettro.ModelInfo{
		{ID: "deepseek-v4-flash", OwnedBy: "deepseek"},
		{ID: "qwen3.7-plus", OwnedBy: "alibaba"},
	})
	msg := spettroLoadedMsg{
		apiKey:    "ep_test",
		models:    models,
		account:   &spettro.Account{Email: "u@example.com", Plan: "pro", PlanStatus: "active"},
		activate:  true,
		fromLogin: true,
	}

	updated, _ := m.handleSpettroLoaded(msg)
	nm := updated.(Model)

	if nm.showLogin || nm.showOnboarding {
		t.Fatalf("overlays should be dismissed: login=%v onboarding=%v", nm.showLogin, nm.showOnboarding)
	}
	if nm.cfg.SpettroPlan != "pro" {
		t.Fatalf("plan not cached: %q", nm.cfg.SpettroPlan)
	}
	if nm.cfg.ActiveProvider != spettro.ProviderID || nm.cfg.ActiveModel != "deepseek-v4-flash" {
		t.Fatalf("active model not set: %s/%s", nm.cfg.ActiveProvider, nm.cfg.ActiveModel)
	}
	got := nm.providers.Models()
	var found bool
	for _, mod := range got {
		if mod.Provider == spettro.ProviderID && mod.Name == "qwen3.7-plus" {
			found = true
		}
	}
	if !found {
		t.Fatalf("spettro models not registered in manager: %+v", got)
	}
}

// TestSpettroPlanName confirms the plan name appears only when signed in.
func TestSpettroPlanName(t *testing.T) {
	m := NewModelForTesting()
	if p := m.spettroPlanName(); p != "" {
		t.Fatalf("expected no plan when logged out, got %q", p)
	}
	m.cfg.APIKeys = map[string]string{spettro.ProviderID: "ep_test"}
	m.cfg.SpettroPlan = "max"
	if p := m.spettroPlanName(); p != "max" {
		t.Fatalf("plan = %q", p)
	}
}

// TestOnboardingIncludesSpettro verifies the synthetic sign-in entry is offered
// when logged out and removed once a key exists.
func TestOnboardingIncludesSpettro(t *testing.T) {
	m := NewModelForTesting()
	m.providers.SetSpettro("", nil) // no models yet
	items := m.allOnboardingModels("")
	if len(items) == 0 || items[0].Provider != spettro.ProviderID || items[0].Name != spettroOnboardingMarker {
		t.Fatalf("expected spettro sign-in entry first, got %+v", items)
	}

	m.cfg.APIKeys = map[string]string{spettro.ProviderID: "ep_test"}
	items = m.allOnboardingModels("")
	for _, it := range items {
		if it.Name == spettroOnboardingMarker {
			t.Fatal("sign-in marker should be gone once logged in")
		}
	}
}
