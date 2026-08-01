package tui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"

	"spettro/internal/config"
)

func (m Model) updateSetup(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if msg.String() == "esc" || strings.ToLower(m.ta.Value()) == "/cancel" {
		m.showSetup = false
		m.ta.Reset()
		m.showBanner("setup cancelled", "info")
		return m, nil
	}
	if msg.String() != "enter" {
		var cmd tea.Cmd
		m.ta, cmd = m.ta.Update(msg)
		return m, cmd
	}

	input := strings.TrimSpace(m.ta.Value())
	m.ta.Reset()

	switch m.setup.step {
	case 0:
		providerIDs := m.providers.ProviderNames()
		if n, err := fmt.Sscanf(input, "%d", new(int)); n == 1 && err == nil {
			var idx int
			fmt.Sscanf(input, "%d", &idx)
			idx--
			if idx < 0 || idx >= len(providerIDs) {
				m.pushSystemMsg(fmt.Sprintf("invalid choice — enter 1-%d or provider name", len(providerIDs)))
				m.refreshViewport()
				return m, nil
			}
			m.setup.provider = providerIDs[idx]
		} else {
			found := false
			for _, id := range providerIDs {
				if strings.EqualFold(id, input) {
					m.setup.provider = id
					found = true
					break
				}
			}
			if !found {
				m.pushSystemMsg(fmt.Sprintf("unknown provider — enter 1-%d or provider name", len(providerIDs)))
				m.refreshViewport()
				return m, nil
			}
		}
		m.setup.step = 1
		var names []string
		for _, mod := range m.providers.Models() {
			if mod.Provider == m.setup.provider {
				displayName := mod.DisplayName
				if displayName == "" {
					displayName = mod.Name
				}
				tag := mod.Tag()
				line := "  " + mod.Name
				if displayName != mod.Name {
					line += " (" + displayName + ")"
				}
				if tag != "" {
					line += "  " + tag
				}
				names = append(names, line)
			}
		}
		m.pushSystemMsg("choose model:\n" + strings.Join(names, "\n"))
	case 1:
		if !m.providers.HasModel(m.setup.provider, input) {
			m.pushSystemMsg("unknown model for " + m.setup.provider + " — try again")
			m.refreshViewport()
			return m, nil
		}
		m.setup.model = input
		m.setup.step = 2
		m.pushSystemMsg("paste API key:")
	case 2:
		if input == "" {
			m.pushSystemMsg("key cannot be empty")
			m.refreshViewport()
			return m, nil
		}
		_ = config.SaveAPIKey(m.setup.provider, input)
		m.cfg.ActiveProvider = m.setup.provider
		m.cfg.ActiveModel = m.setup.model
		m.setup.step = 3
		m.pushSystemMsg("choose permission:\n  1) ask-first\n  2) restricted\n  3) yolo")
	case 3:
		switch input {
		case "1", "ask-first":
			m.cfg.Permission = config.PermissionAskFirst
		case "2", "restricted":
			m.cfg.Permission = config.PermissionRestricted
		case "3", "yolo":
			m.cfg.Permission = config.PermissionYOLO
		default:
			m.pushSystemMsg("invalid — enter 1, 2 or 3")
			m.refreshViewport()
			return m, nil
		}
		_ = m.updateConfig(func(cfg *config.UserConfig) error {
			cfg.ActiveProvider = m.setup.provider
			cfg.ActiveModel = m.setup.model
			cfg.Permission = m.cfg.Permission
			return nil
		})
		m.showSetup = false
		m.pushSystemMsg(fmt.Sprintf("setup complete ✓  %s:%s  perm:%s",
			m.cfg.ActiveProvider, m.cfg.ActiveModel, m.cfg.Permission))
	}

	m.refreshViewport()
	return m, nil
}
