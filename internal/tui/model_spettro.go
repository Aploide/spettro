package tui

import (
	"context"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"spettro/internal/config"
	"spettro/internal/provider"
	"spettro/internal/spettro"
)

// loginState drives the Spettro Subscription device-flow login overlay.
type loginState struct {
	step           string // "initiating" | "waiting" | "loading" | "error"
	sessionID      string
	browserURL     string
	errMsg         string
	fromOnboarding bool
}

// ── Messages ─────────────────────────────────────────────────────────────────

type loginInitiatedMsg struct {
	sessionID  string
	browserURL string
	err        error
}

type loginPolledMsg struct {
	sessionID string
	result    spettro.PollResult
	err       error
}

// spettroLoadedMsg carries the result of fetching the plan's model list and
// account info. fromLogin distinguishes an interactive login (show overlay
// feedback) from a silent background refresh on startup.
type spettroLoadedMsg struct {
	apiKey    string
	models    []provider.Model
	account   *spettro.Account
	err       error
	activate  bool // set the first model as active on success
	fromLogin bool
}

// ── Commands ─────────────────────────────────────────────────────────────────

func startLoginCmd() tea.Cmd {
	return func() tea.Msg {
		sessionID := spettro.NewSessionID()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		url, err := spettro.Initiate(ctx, sessionID)
		return loginInitiatedMsg{sessionID: sessionID, browserURL: url, err: err}
	}
}

// pollLoginCmd waits a couple of seconds then polls the session once.
func pollLoginCmd(sessionID string) tea.Cmd {
	return tea.Tick(2*time.Second, func(time.Time) tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		res, err := spettro.Poll(ctx, sessionID)
		return loginPolledMsg{sessionID: sessionID, result: res, err: err}
	})
}

// loadSpettroCmd fetches the plan's available models and account info.
func loadSpettroCmd(apiKey string, activate, fromLogin bool) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		infos, err := spettro.ListModels(ctx, apiKey)
		if err != nil {
			return spettroLoadedMsg{apiKey: apiKey, err: err, activate: activate, fromLogin: fromLogin}
		}
		acc, _ := spettro.GetAccount(ctx, apiKey)
		return spettroLoadedMsg{
			apiKey:    apiKey,
			models:    spettroModelsToProvider(infos),
			account:   acc,
			activate:  activate,
			fromLogin: fromLogin,
		}
	}
}

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
			Context:      mi.ContextWindow,
		})
	}
	return out
}

func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("cmd", "/c", "start", "", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	_ = cmd.Start()
}

// ── Flow control ─────────────────────────────────────────────────────────────

// startLogin opens the login overlay and kicks off the device flow.
func (m Model) startLogin(fromOnboarding bool) (tea.Model, tea.Cmd) {
	m.showLogin = true
	m.login = loginState{step: "initiating", fromOnboarding: fromOnboarding}
	return m, startLoginCmd()
}

func (m Model) handleLoginInitiated(msg loginInitiatedMsg) (tea.Model, tea.Cmd) {
	if !m.showLogin {
		return m, nil // canceled before initiation returned
	}
	if msg.err != nil {
		m.login.step = "error"
		m.login.errMsg = msg.err.Error()
		return m, nil
	}
	m.login.sessionID = msg.sessionID
	m.login.browserURL = msg.browserURL
	m.login.step = "waiting"
	openBrowser(msg.browserURL)
	return m, pollLoginCmd(msg.sessionID)
}

func (m Model) handleLoginPolled(msg loginPolledMsg) (tea.Model, tea.Cmd) {
	// Ignore stale polls from a canceled or superseded session.
	if !m.showLogin || m.login.sessionID != msg.sessionID {
		return m, nil
	}
	if msg.err != nil {
		m.login.step = "error"
		m.login.errMsg = msg.err.Error()
		return m, nil
	}
	switch msg.result.Status {
	case "complete":
		if msg.result.APIKey == "" {
			m.login.step = "error"
			m.login.errMsg = "login completed but no key was returned — please try again"
			return m, nil
		}
		_ = config.SaveAPIKey(spettro.ProviderID, msg.result.APIKey)
		if keys, err := config.LoadAPIKeys(); err == nil {
			m.providers.SetAPIKeys(keys)
		}
		m.providers.SetSpettro(spettro.InferenceBaseURL(), nil)
		m.login.step = "loading"
		return m, loadSpettroCmd(msg.result.APIKey, m.login.fromOnboarding, true)
	case "expired":
		m.login.step = "error"
		m.login.errMsg = "the login link expired — please run /login again"
		return m, nil
	default: // pending
		return m, pollLoginCmd(msg.sessionID)
	}
}

func (m Model) handleSpettroLoaded(msg spettroLoadedMsg) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		if msg.fromLogin {
			m.showLogin = true
			m.login.step = "error"
			m.login.errMsg = msg.err.Error()
		}
		// Silent on background refresh failures (e.g. backend unreachable at
		// startup): the cached plan badge stays and inference surfaces errors.
		return m, nil
	}

	m.providers.SetSpettro(spettro.InferenceBaseURL(), msg.models)

	_ = m.updateConfig(func(cfg *config.UserConfig) error {
		if msg.account != nil {
			cfg.SpettroEmail = msg.account.Email
			cfg.SpettroPlan = msg.account.Plan
			cfg.SpettroPlanStatus = msg.account.PlanStatus
		}
		return nil
	})

	if msg.activate && len(msg.models) > 0 {
		first := msg.models[0]
		_ = m.updateConfig(func(cfg *config.UserConfig) error {
			cfg.ActiveProvider = first.Provider
			cfg.ActiveModel = first.Name
			return nil
		})
	}

	if msg.fromLogin {
		m.showLogin = false
		m.showOnboarding = false
		m.ta.Reset()
		m.ta.Placeholder = "enter message…"
		plan := "free"
		if msg.account != nil && msg.account.Plan != "" {
			plan = msg.account.Plan
		}
		m.pushSystemMsg(fmt.Sprintf("signed in to Spettro Subscription ✓ — plan: %s", plan))
		if len(msg.models) == 0 {
			m.pushSystemMsg("your plan has no models enabled yet — upgrade at " + spettro.PricingURL)
		} else if msg.activate {
			m.pushSystemMsg("active model set to " + msg.models[0].Name)
		}
		m.refreshViewport()
	}
	return m, nil
}

// handleLogout removes the Spettro API key and clears all subscription state.
func (m Model) handleLogout() (tea.Model, tea.Cmd) {
	if strings.TrimSpace(m.cfg.APIKeys[spettro.ProviderID]) == "" {
		m.showBanner("not signed in to Spettro Subscription", "info")
		return m, nil
	}
	if err := config.RemoveAPIKey(spettro.ProviderID); err != nil {
		m.showBanner("logout failed: "+err.Error(), "error")
		return m, nil
	}
	m.providers.ClearSpettro()
	_ = m.updateConfig(func(cfg *config.UserConfig) error {
		delete(cfg.APIKeys, spettro.ProviderID)
		cfg.SpettroEmail = ""
		cfg.SpettroPlan = ""
		cfg.SpettroPlanStatus = ""
		if cfg.ActiveProvider == spettro.ProviderID {
			cfg.ActiveProvider, cfg.ActiveModel = m.providers.ResolveActive("", "", cfg.APIKeys)
		}
		return nil
	})
	m.pushSystemMsg("signed out of Spettro Subscription")
	return m, nil
}

// ── Key handling ─────────────────────────────────────────────────────────────

func (m Model) updateLogin(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c":
		return m, tea.Quit
	case "esc":
		// Cancel the login. Stray poll results are ignored via the showLogin /
		// sessionID guards in handleLoginPolled.
		wasOnboarding := m.login.fromOnboarding
		m.showLogin = false
		m.login = loginState{}
		if wasOnboarding {
			// Return to the onboarding picker so a model can still be chosen.
			m.showOnboarding = true
			m.onboarding.step = 0
		}
		return m, nil
	case "enter", "r":
		if m.login.step == "error" {
			return m.startLogin(m.login.fromOnboarding)
		}
	}
	return m, nil
}

// ── Views ────────────────────────────────────────────────────────────────────

func (m Model) viewLogin() string {
	header := m.viewHeader()
	mc := m.currentColor()
	contentH := m.height - 1
	topPad := contentH * 2 / 5
	if topPad < 2 {
		topPad = 2
	}

	var lines []string
	for i := 0; i < topPad; i++ {
		lines = append(lines, "")
	}

	title := lipgloss.NewStyle().Foreground(mc).Bold(true).Render("Sign in")

	switch m.login.step {
	case "error":
		lines = append(lines,
			title,
			"",
			styleError.Render("Sign-in failed."),
			styleMuted.Render(m.login.errMsg),
			"",
			styleMuted.Render("enter — try again  •  esc — cancel"),
		)
	case "loading":
		spinFrame := spinnerFrames[m.eyeFrame%len(spinnerFrames)]
		lines = append(lines,
			title,
			"",
			lipgloss.NewStyle().Foreground(mc).Render(spinFrame+" Signed in — loading your plan…"),
		)
	case "waiting":
		spinFrame := spinnerFrames[m.eyeFrame%len(spinnerFrames)]
		lines = append(lines,
			title,
			"",
			lipgloss.NewStyle().Foreground(mc).Render(spinFrame+" Waiting for you to sign in…"),
			"",
			styleMuted.Render("A browser window should have opened. If not, open this URL:"),
			lipgloss.NewStyle().Foreground(colorText).Render(m.login.browserURL),
			"",
			styleMuted.Render("esc — cancel"),
		)
	default: // initiating
		spinFrame := spinnerFrames[m.eyeFrame%len(spinnerFrames)]
		lines = append(lines,
			title,
			"",
			lipgloss.NewStyle().Foreground(mc).Render(spinFrame+" Starting sign-in…"),
		)
	}

	body := lipgloss.NewStyle().
		Width(m.width).
		PaddingLeft(2).
		Render(strings.Join(lines, "\n"))
	return lipgloss.JoinVertical(lipgloss.Left, header, body)
}

// ── Header plan ──────────────────────────────────────────────────────────────

// spettroPlanName returns the current plan name ("free", "lite", "plus", "pro",
// "max") when a Spettro Subscription is connected, or "" otherwise.
func (m Model) spettroPlanName() string {
	if strings.TrimSpace(m.cfg.APIKeys[spettro.ProviderID]) == "" {
		return ""
	}
	plan := strings.ToLower(strings.TrimSpace(m.cfg.SpettroPlan))
	if plan == "" {
		plan = "free"
	}
	return plan
}
