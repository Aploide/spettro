package acp

import (
	"context"
	"strings"
	"sync"
	"time"

	"spettro/internal/config"
	"spettro/internal/provider"
	"spettro/internal/spettro"
)

// Spettro Subscription (device-flow login) over ACP.
//
// The TUI drives this flow from its own event loop (internal/tui/model_spettro.go);
// ACP has no such loop, so the bridge owns the poller: login/start kicks off a
// background goroutine that polls the backend and pushes
// `_spettro/account/update` notifications as the state advances. Clients that
// miss a notification (or reconnect mid-flow) can call login/poll to read the
// current state synchronously — the poller, not the client, talks to the
// backend, so pulling never races the flow forward.

// accountLogin is the in-flight device-flow login. Guarded by bridge.loginMu.
type accountLogin struct {
	id         string
	browserURL string
	// status is "starting" | "pending" | "complete" | "expired" | "error".
	status    string
	errMsg    string
	startedAt time.Time
	cancel    context.CancelFunc
}

// loginPollInterval matches the TUI's cadence (docs/subscription.md).
const loginPollInterval = 2 * time.Second

// loginMaxWait bounds a forgotten flow so a background goroutine can't poll
// the backend forever if the client vanishes without cancelling.
const loginMaxWait = 10 * time.Minute

type loginRegistry struct {
	mu    sync.Mutex
	login *accountLogin
}

// AccountStatus is the reply to `_spettro/account/status`, and the payload of
// the `_spettro/account/update` notification. It is deliberately free of any
// secret material: the ep_ key stays in the encrypted store.
type AccountStatus struct {
	SignedIn   bool   `json:"signedIn"`
	Email      string `json:"email,omitempty"`
	Plan       string `json:"plan,omitempty"`
	PlanStatus string `json:"planStatus,omitempty"`
	// Credits are present only when a live account fetch succeeded; the
	// cached config fields carry no usage data.
	CreditsUsed      *float64 `json:"creditsUsed,omitempty"`
	CreditLimit      *float64 `json:"creditLimit,omitempty"`
	RemainingCredits *float64 `json:"remainingCredits,omitempty"`
	// ModelCount is how many models the plan currently exposes.
	ModelCount int `json:"modelCount"`
	// PricingURL lets the client link out without hard-coding the address.
	PricingURL string `json:"pricingUrl"`
	// Login mirrors the in-flight device flow, if any.
	Login *LoginStatus `json:"login,omitempty"`
	// Stale is true when the values came from the on-disk cache because the
	// backend could not be reached, so the client can show them muted.
	Stale bool `json:"stale,omitempty"`
}

// LoginStatus is the reply to the login/start and login/poll methods.
type LoginStatus struct {
	LoginID    string `json:"loginId,omitempty"`
	Status     string `json:"status"`
	BrowserURL string `json:"browserUrl,omitempty"`
	Error      string `json:"error,omitempty"`
}

// accountStatus reports the current subscription state. It reads the cached
// config first (so the client can render immediately), then refreshes from the
// backend when a key is present. A refresh failure is not an error: the cached
// values are returned with Stale set.
func (b *bridge) accountStatus(ctx context.Context) (AccountStatus, error) {
	cfg, err := config.LoadFull()
	if err != nil {
		return AccountStatus{}, extError("could not read configuration: %v", err)
	}

	out := AccountStatus{
		Email:      strings.TrimSpace(cfg.SpettroEmail),
		Plan:       strings.TrimSpace(cfg.SpettroPlan),
		PlanStatus: strings.TrimSpace(cfg.SpettroPlanStatus),
		PricingURL: spettro.PricingURL,
		Login:      b.currentLoginStatus(),
	}

	apiKey := strings.TrimSpace(cfg.APIKeys[spettro.ProviderID])
	if apiKey == "" {
		// Signed out: never report a stale plan from a previous session.
		return AccountStatus{PricingURL: out.PricingURL, Login: out.Login}, nil
	}
	out.SignedIn = true
	if out.Plan == "" {
		out.Plan = "free"
	}

	fetchCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	acc, accErr := spettro.GetAccount(fetchCtx, apiKey)
	infos, modelsErr := spettro.ListModels(fetchCtx, apiKey)
	if accErr != nil && modelsErr != nil {
		out.Stale = true
		return out, nil
	}

	if acc != nil {
		out.Email = acc.Email
		out.Plan = acc.Plan
		out.PlanStatus = acc.PlanStatus
		out.CreditsUsed = &acc.CreditsUsed
		out.CreditLimit = &acc.CreditLimit
		out.RemainingCredits = &acc.RemainingCredits
		// Refresh the cache the TUI's top bar reads on next launch.
		_, _ = config.Update(func(c *config.UserConfig) error {
			c.SpettroEmail = acc.Email
			c.SpettroPlan = acc.Plan
			c.SpettroPlanStatus = acc.PlanStatus
			return nil
		})
	}
	if modelsErr == nil {
		out.ModelCount = len(infos)
		// Keep the live manager in sync so /models and the model selector
		// show the plan's models without waiting for a restart.
		b.opts.Providers.SetSpettro(spettro.InferenceBaseURL(), spettroModelsToProvider(infos))
	}
	return out, nil
}

// spettroModelsToProvider mirrors the TUI's conversion (model_spettro.go) so
// plan models register under the same provider ID and flags.
func spettroModelsToProvider(infos []spettro.ModelInfo) []provider.Model {
	out := make([]provider.Model, 0, len(infos))
	for _, mi := range infos {
		out = append(out, provider.Model{
			Provider:     spettro.ProviderID,
			ProviderName: spettro.ProviderName,
			Name:         mi.ID,
			DisplayName:  mi.ID,
			ToolCall:     true,
			Vision:       mi.Vision,
			Reasoning:    mi.Reasoning,
			Context:      mi.ContextWindow,
		})
	}
	return out
}

// accountLoginStart begins a device-flow login and returns the URL the user
// must open. The caller does NOT poll the backend: the bridge does, pushing
// `_spettro/account/update` as the flow advances.
func (b *bridge) accountLoginStart(ctx context.Context) (LoginStatus, error) {
	b.logins.mu.Lock()
	// A second start supersedes any flow still running, so a client that
	// retried after a dropped notification doesn't leave two pollers racing
	// to write the same key.
	if prev := b.logins.login; prev != nil && prev.cancel != nil {
		prev.cancel()
	}

	id := spettro.NewSessionID()
	pollCtx, cancel := context.WithCancel(context.Background())
	login := &accountLogin{id: id, status: "starting", startedAt: time.Now(), cancel: cancel}
	b.logins.login = login
	b.logins.mu.Unlock()

	initCtx, initCancel := context.WithTimeout(ctx, 30*time.Second)
	defer initCancel()
	url, err := spettro.Initiate(initCtx, id)
	if err != nil {
		cancel()
		b.setLoginState(id, "error", "", err.Error())
		return LoginStatus{LoginID: id, Status: "error", Error: err.Error()}, nil
	}

	b.logins.mu.Lock()
	if b.logins.login != login {
		// Superseded while Initiate was in flight.
		b.logins.mu.Unlock()
		cancel()
		return LoginStatus{LoginID: id, Status: "cancelled"}, nil
	}
	login.browserURL = url
	login.status = "pending"
	b.logins.mu.Unlock()

	go b.pollLogin(pollCtx, login)

	return LoginStatus{LoginID: id, Status: "pending", BrowserURL: url}, nil
}

// pollLogin drives one device-flow login to completion in the background.
func (b *bridge) pollLogin(ctx context.Context, login *accountLogin) {
	deadline := time.After(loginMaxWait)
	ticker := time.NewTicker(loginPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-deadline:
			b.setLoginState(login.id, "expired", "", "the login link expired — please sign in again")
			return
		case <-ticker.C:
		}

		pollCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		res, err := spettro.Poll(pollCtx, login.id)
		cancel()
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			b.setLoginState(login.id, "error", "", err.Error())
			return
		}

		switch res.Status {
		case "complete":
			if res.APIKey == "" {
				b.setLoginState(login.id, "error", "", "login completed but no key was returned — please try again")
				return
			}
			if err := b.finishLogin(ctx, res.APIKey); err != nil {
				b.setLoginState(login.id, "error", "", err.Error())
				return
			}
			b.setLoginState(login.id, "complete", "", "")
			return
		case "expired":
			b.setLoginState(login.id, "expired", "", "the login link expired — please sign in again")
			return
		default: // pending — keep waiting
		}
	}
}

// finishLogin persists the key and registers the plan's models, mirroring the
// TUI's handleLoginPolled/handleSpettroLoaded. The active model is switched to
// the plan's first model only when nothing else is configured, so signing in
// never silently steals a working setup from under the user.
func (b *bridge) finishLogin(ctx context.Context, apiKey string) error {
	if err := config.SaveAPIKey(spettro.ProviderID, apiKey); err != nil {
		return err
	}
	if keys, err := config.LoadAPIKeys(); err == nil {
		b.opts.Providers.SetAPIKeys(keys)
	}

	fetchCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	infos, err := spettro.ListModels(fetchCtx, apiKey)
	if err != nil {
		// The key is saved and valid enough to retry with; surface the
		// catalog failure without unwinding the sign-in.
		b.opts.Providers.SetSpettro(spettro.InferenceBaseURL(), nil)
		return err
	}
	models := spettroModelsToProvider(infos)
	b.opts.Providers.SetSpettro(spettro.InferenceBaseURL(), models)

	acc, _ := spettro.GetAccount(fetchCtx, apiKey)
	_, err = config.Update(func(c *config.UserConfig) error {
		if acc != nil {
			c.SpettroEmail = acc.Email
			c.SpettroPlan = acc.Plan
			c.SpettroPlanStatus = acc.PlanStatus
		}
		if c.ActiveProvider == "" && len(models) > 0 {
			c.ActiveProvider = models[0].Provider
			c.ActiveModel = models[0].Name
		}
		return nil
	})
	return err
}

// accountLoginPoll returns the current state of the in-flight login without
// touching the backend.
func (b *bridge) accountLoginPoll() (LoginStatus, error) {
	if status := b.currentLoginStatus(); status != nil {
		return *status, nil
	}
	return LoginStatus{Status: "idle"}, nil
}

// accountLoginCancel abandons an in-flight login.
func (b *bridge) accountLoginCancel() (LoginStatus, error) {
	b.logins.mu.Lock()
	login := b.logins.login
	b.logins.login = nil
	b.logins.mu.Unlock()

	if login == nil {
		return LoginStatus{Status: "idle"}, nil
	}
	if login.cancel != nil {
		login.cancel()
	}
	return LoginStatus{LoginID: login.id, Status: "cancelled"}, nil
}

// accountLogout removes the subscription key and clears cached plan state,
// mirroring the TUI's /logout.
func (b *bridge) accountLogout() (AccountStatus, error) {
	cfg, err := config.LoadFull()
	if err != nil {
		return AccountStatus{}, extError("could not read configuration: %v", err)
	}
	if strings.TrimSpace(cfg.APIKeys[spettro.ProviderID]) == "" {
		return AccountStatus{PricingURL: spettro.PricingURL}, nil
	}
	if err := config.RemoveAPIKey(spettro.ProviderID); err != nil {
		return AccountStatus{}, extError("could not sign out: %v", err)
	}
	b.opts.Providers.ClearSpettro()

	if _, err := config.Update(func(c *config.UserConfig) error {
		delete(c.APIKeys, spettro.ProviderID)
		c.SpettroEmail = ""
		c.SpettroPlan = ""
		c.SpettroPlanStatus = ""
		// Signing out of the provider the session was using would leave the
		// agent pointing at a model it can no longer reach: fall back to
		// whatever else is connected.
		if c.ActiveProvider == spettro.ProviderID {
			c.ActiveProvider, c.ActiveModel = b.opts.Providers.ResolveActive("", "", c.APIKeys)
		}
		return nil
	}); err != nil {
		return AccountStatus{}, extError("could not update configuration: %v", err)
	}

	out := AccountStatus{PricingURL: spettro.PricingURL}
	b.notifyAccount(out)
	return out, nil
}

// currentLoginStatus snapshots the in-flight login, or nil when idle.
func (b *bridge) currentLoginStatus() *LoginStatus {
	b.logins.mu.Lock()
	defer b.logins.mu.Unlock()
	login := b.logins.login
	if login == nil {
		return nil
	}
	return &LoginStatus{
		LoginID:    login.id,
		Status:     login.status,
		BrowserURL: login.browserURL,
		Error:      login.errMsg,
	}
}

// setLoginState advances the flow and pushes the new account state to the
// client. Stale updates from a superseded flow are dropped.
func (b *bridge) setLoginState(id, status, browserURL, errMsg string) {
	b.logins.mu.Lock()
	login := b.logins.login
	if login == nil || login.id != id {
		b.logins.mu.Unlock()
		return
	}
	login.status = status
	login.errMsg = errMsg
	if browserURL != "" {
		login.browserURL = browserURL
	}
	b.logins.mu.Unlock()

	// A finished flow's account state is worth a full refresh (plan, credits,
	// model count); a still-pending one only needs the login leg echoed.
	if status == "complete" {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if state, err := b.accountStatus(ctx); err == nil {
			b.notifyAccount(state)
			return
		}
	}
	b.notifyAccount(AccountStatus{
		PricingURL: spettro.PricingURL,
		Login:      b.currentLoginStatus(),
	})
}

// notifyAccount pushes an account state change to the client. Best-effort:
// clients that don't understand the extension simply ignore it.
func (b *bridge) notifyAccount(state AccountStatus) {
	if b.conn == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = b.conn.NotifyExtension(ctx, extAccountUpdate, state)
}
