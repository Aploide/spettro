package acp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	acpsdk "github.com/coder/acp-go-sdk"

	"spettro/internal/config"
	"spettro/internal/provider"
	"spettro/internal/spettro"
)

// extTestBridge returns a bridge with a sandboxed HOME, so the extension
// handlers' config.Update / SaveAPIKey writes never touch the developer's
// real ~/.spettro.
func extTestBridge(t *testing.T) *bridge {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	return newBridge(Options{Providers: provider.NewManager()})
}

func TestHandleExtensionMethod_UnknownMethodIsNotFound(t *testing.T) {
	b := extTestBridge(t)

	_, err := b.HandleExtensionMethod(context.Background(), "_spettro/nope", nil)
	if err == nil {
		t.Fatal("expected an error for an unknown extension method")
	}
	var reqErr *acpsdk.RequestError
	if !errorsAs(err, &reqErr) {
		t.Fatalf("expected *acpsdk.RequestError, got %T: %v", err, err)
	}
	if reqErr.Code != -32601 {
		t.Fatalf("expected method-not-found (-32601), got %d", reqErr.Code)
	}
}

func TestAccountStatus_SignedOut(t *testing.T) {
	b := extTestBridge(t)

	status, err := b.accountStatus(context.Background())
	if err != nil {
		t.Fatalf("accountStatus: %v", err)
	}
	if status.SignedIn {
		t.Fatal("expected signedIn=false with no stored key")
	}
	if status.PricingURL != spettro.PricingURL {
		t.Fatalf("expected the pricing URL to be advertised, got %q", status.PricingURL)
	}
	if status.Login != nil {
		t.Fatalf("expected no in-flight login, got %+v", status.Login)
	}
}

// A signed-out status must not leak a plan cached from a previous session:
// the config fields survive a manual key removal, and reporting them would
// render a "Pro" badge for an account that can no longer authenticate.
func TestAccountStatus_SignedOutIgnoresCachedPlan(t *testing.T) {
	b := extTestBridge(t)

	if _, err := config.Update(func(c *config.UserConfig) error {
		c.SpettroEmail = "stale@example.com"
		c.SpettroPlan = "pro"
		c.SpettroPlanStatus = "active"
		return nil
	}); err != nil {
		t.Fatalf("seed config: %v", err)
	}

	status, err := b.accountStatus(context.Background())
	if err != nil {
		t.Fatalf("accountStatus: %v", err)
	}
	if status.SignedIn || status.Email != "" || status.Plan != "" {
		t.Fatalf("stale plan leaked into a signed-out status: %+v", status)
	}
}

func TestAccountLoginPoll_IdleWhenNoFlow(t *testing.T) {
	b := extTestBridge(t)

	status, err := b.accountLoginPoll()
	if err != nil {
		t.Fatalf("accountLoginPoll: %v", err)
	}
	if status.Status != "idle" {
		t.Fatalf("expected idle, got %q", status.Status)
	}
}

func TestAccountLogout_NoopWhenSignedOut(t *testing.T) {
	b := extTestBridge(t)

	status, err := b.accountLogout()
	if err != nil {
		t.Fatalf("accountLogout: %v", err)
	}
	if status.SignedIn {
		t.Fatal("expected signedIn=false")
	}
}

func TestAccountLogout_ClearsKeyAndPlan(t *testing.T) {
	b := extTestBridge(t)

	if err := config.SaveAPIKey(spettro.ProviderID, "ep_test_key"); err != nil {
		t.Fatalf("seed key: %v", err)
	}
	if _, err := config.Update(func(c *config.UserConfig) error {
		c.SpettroEmail = "user@example.com"
		c.SpettroPlan = "pro"
		c.SpettroPlanStatus = "active"
		c.ActiveProvider = spettro.ProviderID
		c.ActiveModel = "some-model"
		return nil
	}); err != nil {
		t.Fatalf("seed config: %v", err)
	}

	if _, err := b.accountLogout(); err != nil {
		t.Fatalf("accountLogout: %v", err)
	}

	keys, err := config.LoadAPIKeys()
	if err != nil {
		t.Fatalf("LoadAPIKeys: %v", err)
	}
	if strings.TrimSpace(keys[spettro.ProviderID]) != "" {
		t.Fatal("the subscription key survived logout")
	}
	cfg, err := config.LoadFull()
	if err != nil {
		t.Fatalf("LoadFull: %v", err)
	}
	if cfg.SpettroEmail != "" || cfg.SpettroPlan != "" || cfg.SpettroPlanStatus != "" {
		t.Fatalf("cached plan state survived logout: %+v", cfg)
	}
	if cfg.ActiveProvider == spettro.ProviderID {
		t.Fatal("active provider still points at the signed-out subscription")
	}
}

func TestProvidersList_ReportsConnectionState(t *testing.T) {
	b := extTestBridge(t)

	if err := config.SaveAPIKey("anthropic", "sk-ant-test"); err != nil {
		t.Fatalf("seed key: %v", err)
	}
	// The manager reads keys from config; refresh it the way the handlers do.
	if keys, err := config.LoadAPIKeys(); err == nil {
		b.opts.Providers.SetAPIKeys(keys)
	}

	list, err := b.providersList()
	if err != nil {
		t.Fatalf("providersList: %v", err)
	}
	if len(list.Providers) == 0 {
		t.Fatal("expected at least one provider entry")
	}

	var anthropic *ProviderEntry
	for i := range list.Providers {
		if list.Providers[i].ID == "anthropic" {
			anthropic = &list.Providers[i]
		}
		if list.Providers[i].ID == spettro.ProviderID {
			t.Fatal("the subscription must be reported separately, not as a keyed provider")
		}
	}
	if anthropic == nil {
		t.Fatal("anthropic missing from the provider list")
	}
	if !anthropic.Connected {
		t.Fatal("anthropic should report connected after a key was saved")
	}
	if !anthropic.Suggested {
		t.Fatal("anthropic should be flagged as a suggested provider")
	}
	if list.Subscription.Connected {
		t.Fatal("the subscription should not report connected without a key")
	}
}

// The featured providers lead the list, in the same order the TUI's connect
// dialog uses, so both front-ends present the same choice architecture.
func TestProvidersList_SuggestedProvidersLead(t *testing.T) {
	b := extTestBridge(t)

	list, err := b.providersList()
	if err != nil {
		t.Fatalf("providersList: %v", err)
	}

	var seenUnsuggested bool
	for _, p := range list.Providers {
		if !p.Suggested {
			seenUnsuggested = true
			continue
		}
		if seenUnsuggested {
			t.Fatalf("suggested provider %q appears after an unsuggested one", p.ID)
		}
	}
}

func TestProvidersConnect_RejectsEmptyAndSubscription(t *testing.T) {
	b := extTestBridge(t)

	if _, err := b.providersConnect(context.Background(), ConnectProviderParams{}); err == nil {
		t.Fatal("expected an error for a missing providerId")
	}
	if _, err := b.providersConnect(context.Background(), ConnectProviderParams{ProviderID: "anthropic"}); err == nil {
		t.Fatal("expected an error for a missing API key")
	}
	_, err := b.providersConnect(context.Background(), ConnectProviderParams{
		ProviderID: spettro.ProviderID, APIKey: "ep_x"})
	if err == nil {
		t.Fatal("expected the subscription to refuse a pasted API key")
	}
}

// A key that fails verification must not reach the encrypted store — the TUI
// verifies before saving, and a client driving the same flow must not be able
// to poison the config with a bad key.
func TestProvidersConnect_DoesNotSaveUnverifiedKey(t *testing.T) {
	b := extTestBridge(t)

	// An unknown provider has no adapter, so VerifyKey fails without any
	// network access.
	_, err := b.providersConnect(context.Background(), ConnectProviderParams{
		ProviderID: "definitely-not-a-provider", APIKey: "nope"})
	if err == nil {
		t.Fatal("expected verification to fail for an unknown provider")
	}

	keys, loadErr := config.LoadAPIKeys()
	if loadErr != nil {
		t.Fatalf("LoadAPIKeys: %v", loadErr)
	}
	if _, ok := keys["definitely-not-a-provider"]; ok {
		t.Fatal("an unverified key was written to the encrypted store")
	}
}

func TestLocalEndpoint_RequiresEndpoint(t *testing.T) {
	b := extTestBridge(t)

	if _, err := b.localProbe(context.Background(), LocalEndpointParams{}); err == nil {
		t.Fatal("expected an error for a missing endpoint")
	}
	if _, err := b.localRemove(context.Background(), LocalEndpointParams{}); err == nil {
		t.Fatal("expected an error for a missing endpoint")
	}
}

func TestModelsFavorite_TogglesPersistently(t *testing.T) {
	b := extTestBridge(t)

	if _, err := b.modelsFavorite(context.Background(), FavoriteModelParams{
		Provider: "anthropic", Model: "claude-sonnet-4", Favorite: true}); err != nil {
		t.Fatalf("favorite on: %v", err)
	}
	cfg, err := config.LoadFull()
	if err != nil {
		t.Fatalf("LoadFull: %v", err)
	}
	if len(cfg.Favorites) != 1 || cfg.Favorites[0] != "anthropic:claude-sonnet-4" {
		t.Fatalf("favorite not stored: %v", cfg.Favorites)
	}

	// Toggling on twice must not duplicate the entry.
	if _, err := b.modelsFavorite(context.Background(), FavoriteModelParams{
		Provider: "anthropic", Model: "claude-sonnet-4", Favorite: true}); err != nil {
		t.Fatalf("favorite on (repeat): %v", err)
	}
	if cfg, err = config.LoadFull(); err != nil {
		t.Fatalf("LoadFull: %v", err)
	}
	if len(cfg.Favorites) != 1 {
		t.Fatalf("re-favoriting duplicated the entry: %v", cfg.Favorites)
	}

	if _, err := b.modelsFavorite(context.Background(), FavoriteModelParams{
		Provider: "anthropic", Model: "claude-sonnet-4", Favorite: false}); err != nil {
		t.Fatalf("favorite off: %v", err)
	}
	if cfg, err = config.LoadFull(); err != nil {
		t.Fatalf("LoadFull: %v", err)
	}
	if len(cfg.Favorites) != 0 {
		t.Fatalf("favorite not removed: %v", cfg.Favorites)
	}
}

func TestModelsFavorite_RequiresProviderAndModel(t *testing.T) {
	b := extTestBridge(t)

	if _, err := b.modelsFavorite(context.Background(), FavoriteModelParams{Model: "x"}); err == nil {
		t.Fatal("expected an error for a missing provider")
	}
	if _, err := b.modelsFavorite(context.Background(), FavoriteModelParams{Provider: "x"}); err == nil {
		t.Fatal("expected an error for a missing model")
	}
}

// Malformed params must surface as invalid-params rather than being silently
// decoded to a zero value and acted on.
func TestHandleExtensionMethod_MalformedParams(t *testing.T) {
	b := extTestBridge(t)

	_, err := b.HandleExtensionMethod(context.Background(), extModelsFavorite, json.RawMessage(`{"provider":`))
	if err == nil {
		t.Fatal("expected an error for malformed params")
	}
	var reqErr *acpsdk.RequestError
	if !errorsAs(err, &reqErr) {
		t.Fatalf("expected *acpsdk.RequestError, got %T", err)
	}
	if reqErr.Code != -32602 {
		t.Fatalf("expected invalid-params (-32602), got %d", reqErr.Code)
	}
}

// errorsAs is a local errors.As to keep the import list minimal.
func errorsAs(err error, target **acpsdk.RequestError) bool {
	for err != nil {
		if re, ok := err.(*acpsdk.RequestError); ok {
			*target = re
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
