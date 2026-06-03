package tui

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"spettro/internal/config"
	"spettro/internal/provider"
)

type onboardingState struct {
	step     int              // 0=pick model, 1=enter key, 2=verifying, 3=error
	provider string           // selected provider ID
	provName string           // display name of selected provider
	model    string           // selected model name
	filter   string           // search filter for model picker (step 0)
	cursor   int              // list cursor (step 0)
	items    []provider.Model // filtered model list (step 0)
	errMsg   string           // verification error message (step 3)
}

type verifyKeyDoneMsg struct {
	provider string
	model    string
	apiKey   string
	err      error
}

func (m Model) allOnboardingModels(filter string) []provider.Model {
	q := strings.ToLower(strings.TrimSpace(filter))
	var out []provider.Model
	for _, mod := range m.providers.Models() {
		if mod.Local {
			continue
		}
		if q != "" {
			hay := strings.ToLower(mod.Provider + " " + mod.ProviderName + " " + mod.Name + " " + mod.DisplayName)
			if !strings.Contains(hay, q) {
				continue
			}
		}
		out = append(out, mod)
	}
	return out
}

// updateOnboarding dispatches key events to the active step handler.
func (m Model) updateOnboarding(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch m.onboarding.step {
	case 0:
		return m.updateOnboardingPicker(msg)
	case 1:
		return m.updateOnboardingKeyEntry(msg)
	case 2:
		// Verifying — block input, wait for verifyKeyDoneMsg.
		if msg.String() == "ctrl+c" {
			return m, tea.Quit
		}
		return m, nil
	case 3:
		// Error — any key returns to key entry.
		switch msg.String() {
		case "esc", "enter":
			m.onboarding.step = 1
			m.onboarding.errMsg = ""
			m.ta.Reset()
			m.ta.Focus()
		case "ctrl+c":
			return m, tea.Quit
		}
		return m, nil
	}
	return m, nil
}

func (m Model) updateOnboardingPicker(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc", "ctrl+c":
		return m, tea.Quit
	case "up", "shift+tab":
		if m.onboarding.cursor > 0 {
			m.onboarding.cursor--
		}
	case "down", "tab", "ctrl+n":
		if m.onboarding.cursor < len(m.onboarding.items)-1 {
			m.onboarding.cursor++
		}
	case "enter":
		if len(m.onboarding.items) == 0 {
			return m, nil
		}
		sel := m.onboarding.items[m.onboarding.cursor]
		m.onboarding.provider = sel.Provider
		m.onboarding.model = sel.Name
		m.onboarding.provName = sel.ProviderName
		if m.onboarding.provName == "" {
			m.onboarding.provName = sel.Provider
		}
		m.onboarding.step = 1
		m.ta.Reset()
		m.ta.Placeholder = "enter your API key…"
		m.ta.Focus()
	case "backspace":
		if len(m.onboarding.filter) > 0 {
			runes := []rune(m.onboarding.filter)
			m.onboarding.filter = string(runes[:len(runes)-1])
			m.onboarding.items = m.allOnboardingModels(m.onboarding.filter)
			m.onboarding.cursor = 0
		}
	default:
		if s := msg.String(); len([]rune(s)) == 1 {
			m.onboarding.filter += s
			m.onboarding.items = m.allOnboardingModels(m.onboarding.filter)
			m.onboarding.cursor = 0
		}
	}
	return m, nil
}

func (m Model) updateOnboardingKeyEntry(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.onboarding.step = 0
		m.onboarding.filter = ""
		m.onboarding.items = m.allOnboardingModels("")
		m.onboarding.cursor = 0
		m.ta.Reset()
		m.ta.Placeholder = "enter message…"
	case "ctrl+c":
		return m, tea.Quit
	case "enter":
		key := strings.TrimSpace(m.ta.Value())
		if key == "" {
			return m, nil
		}
		m.onboarding.step = 2
		m.ta.Reset()
		providerID := m.onboarding.provider
		modelName := m.onboarding.model
		pm := m.providers
		return m, func() tea.Msg {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			err := pm.VerifyKey(ctx, providerID, key)
			return verifyKeyDoneMsg{provider: providerID, model: modelName, apiKey: key, err: err}
		}
	default:
		var cmd tea.Cmd
		m.ta, cmd = m.ta.Update(msg)
		return m, cmd
	}
	return m, nil
}

func (m Model) handleVerifyKeyDone(msg verifyKeyDoneMsg) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		m.onboarding.step = 3
		m.onboarding.errMsg = msg.err.Error()
		return m, nil
	}

	_ = config.SaveAPIKey(msg.provider, msg.apiKey)
	_ = m.updateConfig(func(cfg *config.UserConfig) error {
		cfg.ActiveProvider = msg.provider
		cfg.ActiveModel = msg.model
		return nil
	})

	m.showOnboarding = false
	m.ta.Reset()
	m.ta.Placeholder = "enter message…"
	provName := m.onboarding.provName
	if provName == "" {
		provName = msg.provider
	}
	m.pushSystemMsg(fmt.Sprintf("connected %s ✓ — ready to use %s", provName, msg.model))
	m.refreshViewport()
	return m, nil
}

// ── Views ──────────────────────────────────────────────────────────────────

func (m Model) viewOnboarding() string {
	header := m.viewHeader()
	var body string
	switch m.onboarding.step {
	case 1:
		body = m.viewOnboardingKeyEntry()
	case 2:
		body = m.viewOnboardingVerifying()
	case 3:
		body = m.viewOnboardingError()
	default:
		body = m.viewOnboardingPicker()
	}
	return lipgloss.JoinVertical(lipgloss.Left, header, body)
}

func (m Model) viewOnboardingPicker() string {
	mc := m.currentColor()
	contentH := m.height - 1
	topPad := contentH / 3
	if topPad < 2 {
		topPad = 2
	}

	instruction := lipgloss.NewStyle().Foreground(mc).Render("To start, let's choose a provider and model.")

	cursor := lipgloss.NewStyle().Foreground(mc).Render("▊")
	promptStyle := lipgloss.NewStyle().Foreground(mc).Bold(true)
	filterLine := promptStyle.Render(">") + " " +
		lipgloss.NewStyle().Foreground(colorText).Render(m.onboarding.filter) +
		cursor

	maxListH := contentH - topPad - 8
	if maxListH < 4 {
		maxListH = 4
	}
	var rows []string
	currentProvider := ""
	for i, mod := range m.onboarding.items {
		if mod.Provider != currentProvider {
			currentProvider = mod.Provider
			if len(rows) > 0 {
				rows = append(rows, "")
			}
			provLabel := mod.ProviderName
			if provLabel == "" {
				provLabel = mod.Provider
			}
			rows = append(rows, styleMuted.Render(provLabel))
		}
		isSelected := i == m.onboarding.cursor
		displayName := mod.DisplayName
		if displayName == "" {
			displayName = mod.Name
		}
		tag := mod.Tag()
		if isSelected {
			label := "› " + displayName
			if tag != "" {
				label += "  " + styleDim.Render(tag)
			}
			rows = append(rows, lipgloss.NewStyle().Foreground(mc).Bold(true).Render(label))
		} else {
			row := "  " + styleMuted.Render(displayName)
			if tag != "" {
				row += "  " + styleDim.Render(tag)
			}
			rows = append(rows, row)
		}
	}
	if len(m.onboarding.items) == 0 {
		rows = append(rows, styleMuted.Render("  no models found"))
	}

	// Scroll window so selected item stays visible.
	start := 0
	if len(rows) > maxListH {
		start = m.onboarding.cursor - maxListH/2
		if start < 0 {
			start = 0
		}
		if start+maxListH > len(rows) {
			start = len(rows) - maxListH
		}
		rows = rows[start : start+maxListH]
	}

	hint := styleMuted.Render("↑↓ choose  •  enter confirm")

	var lines []string
	for i := 0; i < topPad; i++ {
		lines = append(lines, "")
	}
	lines = append(lines, instruction, "", filterLine, "")
	lines = append(lines, rows...)
	lines = append(lines, "", hint)

	return lipgloss.NewStyle().
		Width(m.width).
		PaddingLeft(2).
		Render(strings.Join(lines, "\n"))
}

func (m Model) viewOnboardingKeyEntry() string {
	mc := m.currentColor()
	contentH := m.height - 1
	topPad := contentH * 2 / 5
	if topPad < 2 {
		topPad = 2
	}

	provName := m.onboarding.provName
	if provName == "" {
		provName = m.onboarding.provider
	}

	heading := lipgloss.NewStyle().Foreground(mc).Render("Enter your ") +
		lipgloss.NewStyle().Foreground(mc).Bold(true).Render(provName+" Key") +
		styleMuted.Render(".")

	promptStyle := lipgloss.NewStyle().Foreground(mc).Bold(true)
	inputLine := promptStyle.Render(">") + " " + m.ta.View()

	home, _ := os.UserHomeDir()
	keysPath := filepath.Join(home, ".spettro", "keys.enc")

	var lines []string
	for i := 0; i < topPad; i++ {
		lines = append(lines, "")
	}
	lines = append(lines,
		heading,
		"",
		inputLine,
		"",
		styleMuted.Render("This will be written to your global configuration:"),
		styleMuted.Render(keysPath),
		"",
		styleMuted.Render("enter submit  •  esc back"),
	)

	return lipgloss.NewStyle().
		Width(m.width).
		PaddingLeft(2).
		Render(strings.Join(lines, "\n"))
}

func (m Model) viewOnboardingVerifying() string {
	mc := m.currentColor()
	contentH := m.height - 1
	topPad := contentH * 2 / 5
	if topPad < 2 {
		topPad = 2
	}

	provName := m.onboarding.provName
	if provName == "" {
		provName = m.onboarding.provider
	}

	heading := lipgloss.NewStyle().Foreground(mc).Render("Verifying your ") +
		lipgloss.NewStyle().Foreground(mc).Bold(true).Render(provName+" Key") +
		styleMuted.Render("...")

	// Animated bounce bar
	barInner := 36
	pos := (m.eyeFrame / 2) % (barInner * 2)
	if pos >= barInner {
		pos = barInner*2 - pos
	}
	blockW := 8
	filled := make([]rune, barInner)
	for i := range filled {
		filled[i] = ' '
	}
	for i := 0; i < blockW; i++ {
		if idx := pos + i; idx < barInner {
			filled[idx] = '█'
		}
	}
	spinFrame := spinnerFrames[m.eyeFrame%len(spinnerFrames)]
	barStr := lipgloss.NewStyle().Foreground(mc).Render("▐") +
		lipgloss.NewStyle().Foreground(mc).Render(string(filled)) +
		lipgloss.NewStyle().Foreground(mc).Render("▌")
	spinLine := lipgloss.NewStyle().Foreground(mc).Render(spinFrame+" ") + barStr

	home, _ := os.UserHomeDir()
	keysPath := filepath.Join(home, ".spettro", "keys.enc")

	var lines []string
	for i := 0; i < topPad; i++ {
		lines = append(lines, "")
	}
	lines = append(lines,
		heading,
		"",
		spinLine,
		"",
		styleMuted.Render("This will be written to your global configuration:"),
		styleMuted.Render(keysPath),
	)

	return lipgloss.NewStyle().
		Width(m.width).
		PaddingLeft(2).
		Render(strings.Join(lines, "\n"))
}

func (m Model) viewOnboardingError() string {
	contentH := m.height - 1
	topPad := contentH * 2 / 5
	if topPad < 2 {
		topPad = 2
	}

	provName := m.onboarding.provName
	if provName == "" {
		provName = m.onboarding.provider
	}

	heading := styleError.Render("Failed to verify your " + provName + " key.")
	errDetail := styleMuted.Render(m.onboarding.errMsg)
	hint := styleMuted.Render("enter / esc — try again")

	var lines []string
	for i := 0; i < topPad; i++ {
		lines = append(lines, "")
	}
	lines = append(lines, heading, "", errDetail, "", hint)

	return lipgloss.NewStyle().
		Width(m.width).
		PaddingLeft(2).
		Render(strings.Join(lines, "\n"))
}
