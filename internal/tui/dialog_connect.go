package tui

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"spettro/internal/config"
	"spettro/internal/provider"
)

func (m Model) openConnect() Model {
	m.showConnect = true
	m.connectFilter = ""
	m.connectCursor = 0
	m.connectStep = 0
	m.connectProvider = ""
	m.connectActionCursor = 0
	m.connectEditMode = false
	m.connectItems = m.filterProviders("")
	return m
}

var suggestedProviderIDs = []string{localConnectProviderID, "anthropic", "openai", "mistral", "x-ai", "zai"}

func isSuggested(id string) bool {
	return slices.Contains(suggestedProviderIDs, id)
}

func (m Model) filterProviders(filter string) []provider.ProviderInfo {
	all := m.providers.AllProviderInfos()
	all = append([]provider.ProviderInfo{{
		ID:   localConnectProviderID,
		Name: "Local endpoints (LM Studio/Ollama/llama.cpp/…)",
	}}, all...)

	if filter != "" {
		q := strings.ToLower(filter)
		var out []provider.ProviderInfo
		for _, pi := range all {
			if strings.Contains(strings.ToLower(pi.ID), q) || strings.Contains(strings.ToLower(pi.Name), q) {
				out = append(out, pi)
			}
		}
		all = out
	}

	suggOrder := make(map[string]int, len(suggestedProviderIDs))
	for i, id := range suggestedProviderIDs {
		suggOrder[id] = i
	}
	sugg := make([]provider.ProviderInfo, len(suggestedProviderIDs))
	suggFilled := make([]bool, len(suggestedProviderIDs))
	var rest []provider.ProviderInfo
	for _, pi := range all {
		if idx, ok := suggOrder[pi.ID]; ok {
			sugg[idx] = pi
			suggFilled[idx] = true
		} else {
			rest = append(rest, pi)
		}
	}
	var out []provider.ProviderInfo
	for i, pi := range sugg {
		if suggFilled[i] {
			out = append(out, pi)
		}
	}
	return append(out, rest...)
}

func (m Model) localEndpointConnected() bool {
	return len(m.cfg.LocalEndpoints) > 0
}

func (m Model) hasLocalEndpoint(endpoint string) bool {
	return slices.Contains(m.cfg.LocalEndpoints, endpoint)
}

// isLocalProviderID reports whether a connect-dialog provider ID is a local
// endpoint URL rather than a catalog provider.
func isLocalProviderID(id string) bool {
	return strings.HasPrefix(id, "http://") || strings.HasPrefix(id, "https://")
}

// localEndpointLabel renders a local endpoint as "LM Studio (localhost:1234)".
func localEndpointLabel(endpoint string) string {
	short := strings.TrimPrefix(strings.TrimPrefix(endpoint, "http://"), "https://")
	return provider.LocalProviderName(endpoint) + " (" + short + ")"
}

// localProbeDoneMsg delivers the result of an asynchronous ProbeLocalServer
// round-trip (see probeLocalServerCmd) back to the Update loop.
type localProbeDoneMsg struct {
	endpoint string
	apiKey   string
	models   []provider.Model
	err      error
}

// probeLocalServerCmd probes a local OpenAI-compatible endpoint off the UI
// thread. The 10s timeout bounds the blocking HTTP call so the TUI never hangs.
func probeLocalServerCmd(endpoint, apiKey string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		models, err := provider.ProbeLocalServer(ctx, endpoint, apiKey)
		return localProbeDoneMsg{endpoint: endpoint, apiKey: apiKey, models: models, err: err}
	}
}

// handleLocalProbeDone finishes the local-endpoint connect once the async probe
// resolves: it registers the discovered models, persists the endpoint, and
// closes the connect dialog. A failure leaves the dialog open with an error
// banner so the user can correct the URL.
func (m Model) handleLocalProbeDone(msg localProbeDoneMsg) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		m.showBanner("local endpoint error: "+msg.err.Error(), "error")
		return m, nil
	}
	if len(msg.models) == 0 {
		m.showBanner("local endpoint returned no models", "error")
		return m, nil
	}
	m.providers.AddLocalModels(msg.models)
	normalized := msg.models[0].Provider
	// The endpoint's key lives in the encrypted store under the normalized URL,
	// which is also the local provider ID — so the adapter picks it up like any
	// catalog provider key.
	if msg.apiKey != "" {
		_ = config.SaveAPIKey(normalized, msg.apiKey)
	} else {
		_ = config.RemoveAPIKey(normalized)
	}
	_ = m.updateConfig(func(cfg *config.UserConfig) error {
		if slices.Contains(cfg.LocalEndpoints, normalized) {
			return nil
		}
		cfg.LocalEndpoints = append(cfg.LocalEndpoints, normalized)
		return nil
	})
	m.showConnect = false
	m.ta.Reset()
	m.ta.Focus()
	m.showBanner(fmt.Sprintf("connected %s ✓", provider.LocalProviderName(normalized)), "success")
	return m, nil
}

var connectManageOptions = []string{
	"Edit key",
	"Remove provider",
	"Cancel",
}

var connectConfirmOptions = []string{
	"Yes, remove",
	"Cancel",
}

func (m Model) updateConnect(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch m.connectStep {
	case 0:
		switch msg.String() {
		case "esc", "ctrl+c":
			m.showConnect = false
			return m, nil
		case "up", "shift+tab":
			if m.connectCursor > 0 {
				m.connectCursor--
			}
		case "down", "ctrl+n", "tab":
			if m.connectCursor < len(m.connectItems)-1 {
				m.connectCursor++
			}
		case "enter":
			if len(m.connectItems) > 0 {
				m.connectProvider = m.connectItems[m.connectCursor].ID
				switch {
				case m.connectProvider == localConnectProviderID && len(m.cfg.LocalEndpoints) > 0:
					// Existing endpoints: show the endpoint list (manage or add).
					m.connectStep = 4
					m.connectActionCursor = 0
				case m.connectProvider != localConnectProviderID && m.cfg.APIKeys[m.connectProvider] != "":
					m.connectStep = 2
					m.connectActionCursor = 0
				default:
					m.connectStep = 1
					m.connectEditMode = false
					m.ta.Reset()
					m.ta.Focus()
				}
			}
		case "backspace":
			if len(m.connectFilter) > 0 {
				m.connectFilter = m.connectFilter[:len(m.connectFilter)-1]
				m.connectItems = m.filterProviders(m.connectFilter)
				m.connectCursor = 0
			}
		default:
			if len(msg.String()) == 1 {
				m.connectFilter += msg.String()
				m.connectItems = m.filterProviders(m.connectFilter)
				m.connectCursor = 0
			}
		}
	case 1:
		switch msg.String() {
		case "esc":
			switch {
			case m.connectEditMode:
				m.connectStep = 2
				m.connectEditMode = false
			case m.connectProvider == localConnectProviderID && len(m.cfg.LocalEndpoints) > 0:
				m.connectStep = 4
				m.connectActionCursor = 0
			default:
				m.connectStep = 0
			}
			m.ta.Reset()
		case "enter":
			if m.connectProvider == localConnectProviderID {
				endpoint := strings.TrimSpace(m.ta.Value())
				if endpoint == "" {
					m.showBanner("endpoint cannot be empty", "error")
					return m, nil
				}
				// Ask for the (optional) API key before probing, so servers
				// started with authentication (llama-server --api-key, vLLM,
				// unsloth, …) can be connected too.
				m.connectLocalURL = endpoint
				m.connectStep = 5
				m.ta.Reset()
				m.ta.Focus()
				return m, nil
			}
			key := strings.TrimSpace(m.ta.Value())
			if key == "" {
				m.showBanner("key cannot be empty", "error")
				return m, nil
			}
			_ = config.SaveAPIKey(m.connectProvider, key)
			_ = m.updateConfig(nil)
			m.showConnect = false
			m.ta.Reset()
			m.ta.Focus()
			m.showBanner(fmt.Sprintf("connected %s ✓", m.connectProvider), "success")
			return m, nil
		default:
			var cmd tea.Cmd
			m.ta, cmd = m.ta.Update(msg)
			return m, cmd
		}
	case 2:
		switch msg.String() {
		case "esc":
			if isLocalProviderID(m.connectProvider) {
				m.connectStep = 4
			} else {
				m.connectStep = 0
			}
			m.connectActionCursor = 0
		case "up", "shift+tab":
			if m.connectActionCursor > 0 {
				m.connectActionCursor--
			}
		case "down", "ctrl+n", "tab":
			if m.connectActionCursor < len(connectManageOptions)-1 {
				m.connectActionCursor++
			}
		case "enter":
			switch m.connectActionCursor {
			case 0: // Edit key
				if isLocalProviderID(m.connectProvider) {
					// Local endpoint: re-enter the optional-key step; the probe
					// re-validates the endpoint with the new key.
					m.connectLocalURL = m.connectProvider
					m.connectStep = 5
				} else {
					m.connectStep = 1
				}
				m.connectEditMode = true
				m.ta.Reset()
				m.ta.Focus()
			case 1: // Remove provider
				m.connectStep = 3
				m.connectActionCursor = 0
			case 2: // Cancel
				m.connectStep = 0
				m.connectActionCursor = 0
			}
		}
	case 3:
		switch msg.String() {
		case "esc":
			m.connectStep = 2
			m.connectActionCursor = 0
		case "up", "shift+tab":
			if m.connectActionCursor > 0 {
				m.connectActionCursor--
			}
		case "down", "ctrl+n", "tab":
			if m.connectActionCursor < len(connectConfirmOptions)-1 {
				m.connectActionCursor++
			}
		case "enter":
			switch m.connectActionCursor {
			case 0: // Yes, remove
				removedProvider := m.connectProvider
				wasActive := m.cfg.ActiveProvider == removedProvider
				_ = config.RemoveAPIKey(removedProvider)
				if isLocalProviderID(removedProvider) {
					m.providers.RemoveLocalModels(removedProvider)
				}
				_ = m.updateConfig(func(cfg *config.UserConfig) error {
					if isLocalProviderID(removedProvider) {
						cfg.LocalEndpoints = slices.DeleteFunc(cfg.LocalEndpoints, func(e string) bool {
							return e == removedProvider
						})
					}
					if cfg.ActiveProvider == removedProvider {
						connected := m.providers.ConnectedModels(cfg.APIKeys)
						if len(connected) > 0 {
							cfg.ActiveProvider = connected[0].Provider
							cfg.ActiveModel = connected[0].Name
						} else {
							cfg.ActiveProvider = ""
							cfg.ActiveModel = ""
						}
					}
					return nil
				})
				m.showConnect = false
				switch {
				case wasActive && m.cfg.ActiveProvider != "":
					m.showBanner(fmt.Sprintf("removed %s — switched to %s:%s", removedProvider, m.cfg.ActiveProvider, m.cfg.ActiveModel), "info")
				case wasActive:
					m.showBanner(fmt.Sprintf("removed %s — no providers connected", removedProvider), "warn")
				default:
					m.showBanner(fmt.Sprintf("removed %s", removedProvider), "info")
				}
				return m, nil
			case 1: // Cancel
				m.connectStep = 2
				m.connectActionCursor = 0
			}
		}
	case 4: // local endpoint list: manage an existing endpoint or add a new one
		switch msg.String() {
		case "esc", "ctrl+c":
			m.connectStep = 0
			m.connectActionCursor = 0
		case "up", "shift+tab":
			if m.connectActionCursor > 0 {
				m.connectActionCursor--
			}
		case "down", "ctrl+n", "tab":
			if m.connectActionCursor < len(m.cfg.LocalEndpoints) {
				m.connectActionCursor++
			}
		case "enter":
			if m.connectActionCursor < len(m.cfg.LocalEndpoints) {
				m.connectProvider = m.cfg.LocalEndpoints[m.connectActionCursor]
				m.connectStep = 2
				m.connectActionCursor = 0
			} else { // "+ Add new endpoint"
				m.connectStep = 1
				m.connectEditMode = false
				m.ta.Reset()
				m.ta.Focus()
			}
		}
	case 5: // optional API key for a local endpoint
		switch msg.String() {
		case "esc":
			if m.connectEditMode {
				m.connectStep = 2
				m.connectEditMode = false
			} else {
				m.connectStep = 1
				m.ta.Reset()
				m.ta.SetValue(m.connectLocalURL)
				m.ta.Focus()
			}
		case "enter":
			// Empty key is valid: most local servers run without auth.
			m.showBanner("probing local endpoint…", "info")
			return m, probeLocalServerCmd(m.connectLocalURL, strings.TrimSpace(m.ta.Value()))
		default:
			var cmd tea.Cmd
			m.ta, cmd = m.ta.Update(msg)
			return m, cmd
		}
	}

	return m, nil
}

func (m Model) viewConnect() string {
	mc := m.currentColor()
	dialogWidth := 60
	if m.width < dialogWidth+4 {
		dialogWidth = m.width - 4
	}
	if dialogWidth < 30 {
		dialogWidth = 30
	}
	innerW := dialogInnerWidth(dialogWidth)

	if m.connectStep == 1 || m.connectStep == 5 {
		provName := m.connectProvider
		envHint := ""
		prompt := "paste your API key and press enter:"
		if m.connectProvider == localConnectProviderID {
			provName = "Local endpoint"
			prompt = "enter local endpoint (e.g. localhost:1234) and press enter:"
		}
		if m.connectStep == 5 {
			provName = localEndpointLabel(m.connectLocalURL)
			prompt = "API key — press enter to skip if the server needs none:"
		}
		for _, pi := range m.providers.AllProviderInfos() {
			if pi.ID == m.connectProvider {
				if pi.Name != "" {
					provName = pi.Name
				}
				if pi.Env != "" {
					envHint = styleMuted.Render("env var: " + pi.Env)
				}
				break
			}
		}

		titleLabel := lipgloss.NewStyle().Bold(true).Foreground(mc).Render("◈ connect " + provName)
		title := diagFillTitle(titleLabel, innerW)
		inner := lipgloss.JoinVertical(lipgloss.Left,
			title, "",
			envHint,
			styleMuted.Render(prompt),
			"",
			m.ta.View(),
			"",
			styleMuted.Render("esc: back"),
		)
		dialog := lipgloss.NewStyle().
			BorderStyle(lipgloss.RoundedBorder()).
			BorderForeground(mc).
			Width(dialogWidth+2).
			Padding(1, 2).
			Render(inner)

		return lipgloss.Place(m.width, m.height,
			lipgloss.Center, lipgloss.Center,
			dialog,
			lipgloss.WithWhitespaceChars(" "),
			lipgloss.WithWhitespaceStyle(lipgloss.NewStyle().Foreground(colorDim)),
		)
	}

	if m.connectStep == 2 {
		provName := m.connectProvider
		if isLocalProviderID(provName) {
			provName = localEndpointLabel(provName)
		}
		for _, pi := range m.providers.AllProviderInfos() {
			if pi.ID == m.connectProvider && pi.Name != "" {
				provName = pi.Name
				break
			}
		}
		titleLabel := lipgloss.NewStyle().Bold(true).Foreground(mc).Render("◈ manage " + provName)
		title := diagFillTitle(titleLabel, innerW)
		var rows []string
		for i, opt := range connectManageOptions {
			if i == m.connectActionCursor {
				rows = append(rows, lipgloss.NewStyle().
					Background(colorSelBg).Foreground(colorText).Bold(true).
					Width(innerW).Render("› "+opt))
			} else {
				rows = append(rows, lipgloss.NewStyle().Foreground(colorMuted).Render("  "+opt))
			}
		}
		inner := lipgloss.JoinVertical(lipgloss.Left,
			title, "",
			strings.Join(rows, "\n"),
			"",
			styleMuted.Render("↑↓ navigate  enter select  esc back"),
		)
		dialog := lipgloss.NewStyle().
			BorderStyle(lipgloss.RoundedBorder()).
			BorderForeground(mc).
			Width(dialogWidth+2).
			Padding(1, 2).
			Render(inner)
		return lipgloss.Place(m.width, m.height,
			lipgloss.Center, lipgloss.Center,
			dialog,
			lipgloss.WithWhitespaceChars(" "),
			lipgloss.WithWhitespaceStyle(lipgloss.NewStyle().Foreground(colorDim)),
		)
	}

	if m.connectStep == 3 {
		provName := m.connectProvider
		if isLocalProviderID(provName) {
			provName = localEndpointLabel(provName)
		}
		for _, pi := range m.providers.AllProviderInfos() {
			if pi.ID == m.connectProvider && pi.Name != "" {
				provName = pi.Name
				break
			}
		}
		titleLabel := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#FF5555")).Render("◈ remove " + provName)
		title := diagFillTitle(titleLabel, innerW)
		warning := lipgloss.NewStyle().Foreground(colorText).Render("Remove this provider and delete its API key?")
		var rows []string
		for i, opt := range connectConfirmOptions {
			if i == m.connectActionCursor {
				rows = append(rows, lipgloss.NewStyle().
					Background(colorSelBg).Foreground(colorText).Bold(true).
					Width(innerW).Render("› "+opt))
			} else {
				rows = append(rows, lipgloss.NewStyle().Foreground(colorMuted).Render("  "+opt))
			}
		}
		inner := lipgloss.JoinVertical(lipgloss.Left,
			title, "",
			warning, "",
			strings.Join(rows, "\n"),
			"",
			styleMuted.Render("↑↓ navigate  enter confirm  esc back"),
		)
		dialog := lipgloss.NewStyle().
			BorderStyle(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("#FF5555")).
			Width(dialogWidth+2).
			Padding(1, 2).
			Render(inner)
		return lipgloss.Place(m.width, m.height,
			lipgloss.Center, lipgloss.Center,
			dialog,
			lipgloss.WithWhitespaceChars(" "),
			lipgloss.WithWhitespaceStyle(lipgloss.NewStyle().Foreground(colorDim)),
		)
	}

	if m.connectStep == 4 {
		titleLabel := lipgloss.NewStyle().Bold(true).Foreground(mc).Render("◈ local endpoints")
		title := diagFillTitle(titleLabel, innerW)
		options := make([]string, 0, len(m.cfg.LocalEndpoints)+1)
		for _, ep := range m.cfg.LocalEndpoints {
			label := localEndpointLabel(ep)
			if m.cfg.APIKeys[ep] != "" {
				label += "  🔑"
			}
			options = append(options, label)
		}
		options = append(options, "+ Add new endpoint")
		var rows []string
		for i, opt := range options {
			if i == m.connectActionCursor {
				rows = append(rows, lipgloss.NewStyle().
					Background(colorSelBg).Foreground(colorText).Bold(true).
					Width(innerW).Render("› "+opt))
			} else {
				rows = append(rows, lipgloss.NewStyle().Foreground(colorMuted).Render("  "+opt))
			}
		}
		inner := lipgloss.JoinVertical(lipgloss.Left,
			title, "",
			strings.Join(rows, "\n"),
			"",
			styleMuted.Render("↑↓ navigate  enter select  esc back"),
		)
		dialog := lipgloss.NewStyle().
			BorderStyle(lipgloss.RoundedBorder()).
			BorderForeground(mc).
			Width(dialogWidth+2).
			Padding(1, 2).
			Render(inner)
		return lipgloss.Place(m.width, m.height,
			lipgloss.Center, lipgloss.Center,
			dialog,
			lipgloss.WithWhitespaceChars(" "),
			lipgloss.WithWhitespaceStyle(lipgloss.NewStyle().Foreground(colorDim)),
		)
	}

	titleLabel := lipgloss.NewStyle().Bold(true).Foreground(mc).Render("◈ connect provider")
	title := diagFillTitle(titleLabel, innerW)
	cursor := lipgloss.NewStyle().Foreground(mc).Render("▊")
	promptStyle := lipgloss.NewStyle().Foreground(mc).Bold(true)
	filterLine := promptStyle.Render(">") + " " +
		lipgloss.NewStyle().Foreground(colorText).Render(m.connectFilter) +
		cursor

	var rows []string
	selectedRow := 0
	inSuggested := true
	for i, pi := range m.connectItems {
		nowSugg := isSuggested(pi.ID)
		if i == 0 {
			if nowSugg {
				rows = append(rows, lipgloss.NewStyle().Foreground(colorMuted).Bold(true).Render("  ─ suggested"))
			} else {
				rows = append(rows, lipgloss.NewStyle().Foreground(colorMuted).Bold(true).Render("  ─ all providers"))
				inSuggested = false
			}
		} else if inSuggested && !nowSugg {
			inSuggested = false
			rows = append(rows, "")
			rows = append(rows, lipgloss.NewStyle().Foreground(colorMuted).Bold(true).Render("  ─ all providers"))
		}

		isSelected := i == m.connectCursor
		isConnected := m.cfg.APIKeys[pi.ID] != ""
		if pi.ID == localConnectProviderID {
			isConnected = m.localEndpointConnected()
		}

		name := pi.Name
		if name == "" {
			name = pi.ID
		}

		if isSelected {
			selectedRow = len(rows)
			label := "› " + name
			if isConnected {
				label += "  ✓ connected"
			}
			rows = append(rows, lipgloss.NewStyle().
				Background(colorSelBg).
				Foreground(colorText).
				Bold(true).
				Width(innerW).
				Render(label))
		} else {
			nameStyle := lipgloss.NewStyle().Foreground(colorMuted)
			suffix := ""
			if isConnected {
				suffix = "  " + lipgloss.NewStyle().Foreground(colorSuccess).Render("✓ connected")
			}
			rows = append(rows, "  "+nameStyle.Render(name)+suffix)
		}
	}
	if len(m.connectItems) == 0 {
		rows = append(rows, styleMuted.Render("  no matches"))
	}

	hint := styleMuted.Render("↑↓ navigate  enter connect  esc close")
	maxRows := max(m.height-12, 4)
	start := 0
	if len(rows) > maxRows {
		start = max(selectedRow-maxRows/2, 0)
		if start+maxRows > len(rows) {
			start = len(rows) - maxRows
		}
		rows = rows[start : start+maxRows]
	}

	dialog := lipgloss.NewStyle().
		BorderStyle(lipgloss.RoundedBorder()).
		BorderForeground(mc).
		Width(dialogWidth+2).
		Padding(1, 2).
		Render(lipgloss.JoinVertical(lipgloss.Left,
			title, "",
			filterLine, "",
			strings.Join(rows, "\n"),
			"",
			hint,
		))

	return lipgloss.Place(m.width, m.height,
		lipgloss.Center, lipgloss.Center,
		dialog,
		lipgloss.WithWhitespaceChars(" "),
		lipgloss.WithWhitespaceStyle(lipgloss.NewStyle().Foreground(colorDim)),
	)
}
