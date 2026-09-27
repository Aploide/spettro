package tui

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"spettro/internal/config"
	"spettro/internal/homedir"
	"spettro/internal/provider"
	"spettro/internal/spettro"
	"spettro/internal/termtext"
	"spettro/internal/theme"
)

// spettroOnboardingMarker is the sentinel model name for the synthetic
// "Sign in to Spettro" entry shown at the top of onboarding before login.
const spettroOnboardingMarker = "__spettro_login__"

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

	// Offer the Spettro Subscription as the first onboarding option. When the
	// user is not yet signed in we show a synthetic "sign in" entry that opens
	// the device-flow login instead of asking for an API key.
	if strings.TrimSpace(m.cfg.APIKeys[spettro.ProviderID]) == "" {
		entry := provider.Model{
			Provider:     spettro.ProviderID,
			ProviderName: spettro.ProviderName,
			Name:         spettroOnboardingMarker,
			DisplayName:  "Sign in to your Spettro subscription",
		}
		hay := strings.ToLower(entry.Provider + " " + entry.ProviderName + " " + entry.DisplayName + " subscription plan")
		if q == "" || strings.Contains(hay, q) {
			out = append(out, entry)
		}
	}

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
func (m Model) updateOnboarding(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
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

func (m Model) updateOnboardingPicker(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
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
		// The synthetic Spettro entry opens the device-flow login instead of
		// the API-key entry step.
		if sel.Provider == spettro.ProviderID && sel.Name == spettroOnboardingMarker {
			return m.startLogin(true)
		}
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

func (m Model) updateOnboardingKeyEntry(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
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
	textW := max(m.width-onboardingIndent, 1)

	// Wrapped here, then styled row by row, so the layout below can count
	// the rows the instruction takes on a narrow terminal.
	instructionRows := termtext.Wrap("To start, let's choose a provider and model.", textW)
	for i, row := range instructionRows {
		instructionRows[i] = lipgloss.NewStyle().Foreground(mc).Render(row)
	}
	topPad, maxListH := onboardingPickerLayout(contentH, len(instructionRows))

	cursor := lipgloss.NewStyle().Foreground(mc).Render("▊")
	promptStyle := lipgloss.NewStyle().Foreground(mc).Bold(true)
	filterLine := promptStyle.Render(">") + " " +
		lipgloss.NewStyle().Foreground(theme.Current().Text).Render(m.onboarding.filter) +
		cursor

	var rows []string
	selectedRow := 0
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
			selectedRow = len(rows)
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

	// Scroll window so selected item stays visible. The blank rows around
	// the list double as "↑ N more" / "↓ N more" markers when it scrolls.
	start := windowStart(len(rows), maxListH, selectedRow)
	end := min(start+maxListH, len(rows))
	above := moreMarker("↑", start, textW)
	below := moreMarker("↓", len(rows)-end, textW)
	rows = rows[start:end]

	hint := styleMuted.Render(termtext.Fit("↑↓ choose  •  enter confirm", textW))

	var lines []string
	for i := 0; i < topPad; i++ {
		lines = append(lines, "")
	}
	lines = append(lines, instructionRows...)
	lines = append(lines, "", filterLine, above)
	for _, row := range rows {
		lines = append(lines, termtext.Fit(row, textW))
	}
	lines = append(lines, below, hint)

	return lipgloss.NewStyle().
		PaddingLeft(onboardingIndent).
		Render(strings.Join(lines, "\n"))
}

// onboardingIndent is the left margin of the onboarding screens.
const onboardingIndent = 2

// onboardingPickerLayout splits the rows under the header between the top
// padding and the model list. The picker's fixed rows are the instruction
// (instructionRows, it wraps on a narrow terminal), the filter line, the
// blank (or "more") rows around the list and the key hint. The top padding,
// a third of the screen, gives way first when the list would get fewer than
// minOnboardingListRows, so on a small terminal (40x15) the key hint stays
// on screen instead of being pushed off the bottom.
func onboardingPickerLayout(contentH, instructionRows int) (topPad, listRows int) {
	fixed := instructionRows + 5 // blank, filter, spacer, spacer, hint
	topPad = max(contentH/3, 2)
	listRows = contentH - fixed - topPad
	if listRows < minOnboardingListRows {
		topPad = max(contentH-fixed-minOnboardingListRows, 0)
		listRows = contentH - fixed - topPad
	}
	return topPad, max(listRows, 1)
}

// minOnboardingListRows is the fewest model rows the onboarding picker
// shows before it gives up its top padding.
const minOnboardingListRows = 4

func (m Model) viewOnboardingKeyEntry() string {
	mc := m.currentColor()
	contentH := m.height - 1
	topPad := max(contentH*2/5, 2)

	provName := m.onboarding.provName
	if provName == "" {
		provName = m.onboarding.provider
	}

	heading := lipgloss.NewStyle().Foreground(mc).Render("Enter your ") +
		lipgloss.NewStyle().Foreground(mc).Bold(true).Render(provName+" Key") +
		styleMuted.Render(".")

	promptStyle := lipgloss.NewStyle().Foreground(mc).Bold(true)
	inputLine := promptStyle.Render(">") + " " + m.ta.View()

	home, _ := homedir.Dir()
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
	topPad := max(contentH*2/5, 2)

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
	for i := range blockW {
		if idx := pos + i; idx < barInner {
			filled[idx] = '█'
		}
	}
	spinFrame := spinnerFrames[m.eyeFrame%len(spinnerFrames)]
	barStr := lipgloss.NewStyle().Foreground(mc).Render("▐") +
		lipgloss.NewStyle().Foreground(mc).Render(string(filled)) +
		lipgloss.NewStyle().Foreground(mc).Render("▌")
	spinLine := lipgloss.NewStyle().Foreground(mc).Render(spinFrame+" ") + barStr

	home, _ := homedir.Dir()
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
	topPad := max(contentH*2/5, 2)

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
