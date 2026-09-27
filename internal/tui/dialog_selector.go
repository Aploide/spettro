package tui

import (
	"fmt"
	"image/color"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"spettro/internal/config"
	"spettro/internal/provider"
	"spettro/internal/termtext"
	"spettro/internal/theme"
)

func (m Model) openSelector(prefix string) Model {
	m.showSelector = true
	m.selFilter = strings.ToLower(strings.TrimSpace(prefix))
	m.selCursor = 0
	m.selItems = m.filterModels(m.selFilter)
	return m
}

func (m Model) filterModels(prefix string) []provider.Model {
	all := m.providers.ConnectedModels(m.cfg.APIKeys)
	if len(all) == 0 {
		all = nil
	}

	var favs, rest []provider.Model
	for _, mod := range all {
		if m.favorites[mod.Provider+":"+mod.Name] {
			favs = append(favs, mod)
		} else {
			rest = append(rest, mod)
		}
	}
	combined := append(favs, rest...)

	if prefix == "" {
		return combined
	}
	q := strings.ToLower(prefix)
	var out []provider.Model
	for _, mod := range combined {
		hay := strings.ToLower(mod.Provider + " " + mod.ProviderName + " " + mod.Name + " " + mod.DisplayName)
		if strings.Contains(hay, q) {
			out = append(out, mod)
		}
	}
	return out
}

func (m Model) favoriteModels() []provider.Model {
	all := m.providers.ConnectedModels(m.cfg.APIKeys)
	out := make([]provider.Model, 0, len(all))
	for _, mod := range all {
		if m.favorites[mod.Provider+":"+mod.Name] {
			out = append(out, mod)
		}
	}
	return out
}

func (m *Model) saveFavorites() {
	favList := make([]string, 0, len(m.favorites))
	for k, v := range m.favorites {
		if v {
			favList = append(favList, k)
		}
	}
	m.cfg.Favorites = favList
	_ = m.updateConfig(func(cfg *config.UserConfig) error {
		cfg.Favorites = append([]string(nil), favList...)
		return nil
	})
}

func (m Model) updateSelector(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc", "ctrl+c":
		m.showSelector = false
		return m, nil
	case "up", "shift+tab":
		if m.selCursor > 0 {
			m.selCursor--
		}
	case "down", "ctrl+n", "tab":
		if m.selCursor < len(m.selItems)-1 {
			m.selCursor++
		}
	case "enter":
		if len(m.selItems) > 0 {
			sel := m.selItems[m.selCursor]
			_ = m.updateConfig(func(cfg *config.UserConfig) error {
				cfg.ActiveProvider = sel.Provider
				cfg.ActiveModel = sel.Name
				return nil
			})
			m.showSelector = false
			m.showBanner(fmt.Sprintf("model → %s:%s", sel.Provider, sel.Name), "success")
		}
	case "f":
		if len(m.selItems) > 0 {
			sel := m.selItems[m.selCursor]
			key := sel.Provider + ":" + sel.Name
			if m.favorites == nil {
				m.favorites = map[string]bool{}
			}
			m.favorites[key] = !m.favorites[key]
			m.saveFavorites()
			m.selItems = m.filterModels(m.selFilter)
			if m.selCursor >= len(m.selItems) {
				m.selCursor = len(m.selItems) - 1
			}
			if m.selCursor < 0 {
				m.selCursor = 0
			}
		}
	case "c":
		m.showSelector = false
		m = m.openConnect()
		return m, nil
	case "backspace":
		if len(m.selFilter) > 0 {
			m.selFilter = m.selFilter[:len(m.selFilter)-1]
			m.selItems = m.filterModels(m.selFilter)
			m.selCursor = 0
		}
	default:
		if len(msg.String()) == 1 {
			m.selFilter += msg.String()
			m.selItems = m.filterModels(m.selFilter)
			m.selCursor = 0
		}
	}

	return m, nil
}

func (m Model) viewSelector() string {
	mc := m.currentColor()

	dialogWidth := 70
	if m.width < dialogWidth+4 {
		dialogWidth = m.width - 4
	}
	if dialogWidth < 30 {
		dialogWidth = 30
	}
	innerW := dialogInnerWidth(dialogWidth)

	titleLabel := lipgloss.NewStyle().Bold(true).Foreground(mc).Render("◈ select model")
	title := diagFillTitle(titleLabel, innerW)

	if len(m.providers.ConnectedModels(m.cfg.APIKeys)) == 0 {
		// This box pads four cells a side, not two, so its title rule is
		// four cells shorter than innerW; at innerW it wrapped onto a
		// second row.
		msg := lipgloss.JoinVertical(lipgloss.Left,
			diagFillTitle(titleLabel, innerW-4),
			"",
			styleMuted.Render("no providers connected yet"),
			"",
			styleSuccess.Render("press c to connect a provider"),
			styleMuted.Render("or use /connect from the main screen"),
		)
		dialog := lipgloss.NewStyle().
			BorderStyle(lipgloss.RoundedBorder()).
			BorderForeground(mc).
			Width(dialogWidth+2).
			Padding(2, 4).
			Render(msg)
		return lipgloss.Place(m.width, m.height,
			lipgloss.Center, lipgloss.Center,
			dialog,
			lipgloss.WithWhitespaceChars(" "),
			lipgloss.WithWhitespaceStyle(lipgloss.NewStyle().Foreground(theme.Current().Rule)),
		)
	}

	cursor := lipgloss.NewStyle().Foreground(mc).Render("▊")
	promptStyle := lipgloss.NewStyle().Foreground(mc).Bold(true)
	filterLine := promptStyle.Render(">") + " " +
		lipgloss.NewStyle().Foreground(theme.Current().Text).Render(m.selFilter) +
		cursor

	var rows []string
	selectedRow := 0
	currentProvider := ""
	for i, mod := range m.selItems {
		if mod.Provider != currentProvider {
			currentProvider = mod.Provider
			if len(rows) > 0 {
				rows = append(rows, "")
			}
			provLabel := mod.ProviderName
			if provLabel == "" {
				provLabel = mod.Provider
			}
			rows = append(rows, lipgloss.NewStyle().
				Foreground(theme.Current().TextMuted).Bold(true).
				Render(termtext.Fit("  ─ "+provLabel, innerW)))
		}
		if i == m.selCursor {
			selectedRow = len(rows)
		}
		rows = append(rows, m.selectorRow(mod, i == m.selCursor, innerW, mc))
	}
	if len(m.selItems) == 0 {
		rows = append(rows, styleMuted.Render("  no matches"))
	}

	d := listDialog{
		title:  title,
		head:   []string{filterLine},
		rows:   rows,
		hints:  []string{"↑↓ navigate", "enter select", "f favorite", "c connect", "esc close"},
		border: mc,
	}
	_, visible := d.layout(innerW, m.height)
	dialog := d.view(dialogWidth, m.height, windowStart(len(rows), visible, selectedRow))
	return lipgloss.Place(m.width, m.height,
		lipgloss.Center, lipgloss.Center,
		dialog,
		lipgloss.WithWhitespaceChars(" "),
		lipgloss.WithWhitespaceStyle(lipgloss.NewStyle().Foreground(theme.Current().Rule)),
	)
}

// selectorRow renders one model of the selector on a single row of innerW
// cells: badges (favourite, current), the name and the tag ("img think 32k
// ctx"). A name too long for the row is cut with "…" and the tag goes with
// it, rather than the row wrapping and growing the dialog.
func (m Model) selectorRow(mod provider.Model, selected bool, innerW int, mc color.Color) string {
	isCurrent := mod.Provider == m.cfg.ActiveProvider && mod.Name == m.cfg.ActiveModel
	isFav := m.favorites[mod.Provider+":"+mod.Name]
	displayName := mod.DisplayName
	if displayName == "" {
		displayName = mod.Name
	}
	tag := mod.Tag()

	if selected {
		label := "› "
		if isFav {
			label += "★ "
		}
		if isCurrent {
			label += "● "
		}
		label += displayName
		if tag != "" {
			label += "  " + tag
		}
		return lipgloss.NewStyle().
			Background(theme.Current().BgSelection).
			Foreground(theme.Current().Text).
			Bold(true).
			Width(innerW).
			Render(termtext.Fit(label, innerW))
	}

	var badges string
	if isFav {
		badges += lipgloss.NewStyle().Foreground(theme.Current().WarningSoft).Render("★ ")
	}
	if isCurrent {
		badges += lipgloss.NewStyle().Foreground(mc).Render("● ")
	}
	row := "  " + badges + lipgloss.NewStyle().Foreground(theme.Current().TextMuted).Render(displayName)
	if tag != "" {
		row += "  " + lipgloss.NewStyle().Foreground(theme.Current().TextDim).Render(tag)
	}
	return termtext.Fit(row, innerW)
}
