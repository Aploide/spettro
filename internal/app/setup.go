package app

import (
	"fmt"
	"strings"

	"spettro/internal/config"
	"spettro/internal/provider"
)

func (a *App) startSetup() error {
	a.setup = &setupWizard{}
	a.printLine(a.ui.Panel(string(a.mode), "Initial Setup", "Let's configure Spettro.\nType /cancel to abort setup at any step."))
	a.printLine("Select provider:")
	a.printLine("1) openai-compatible")
	a.printLine("2) anthropic")
	a.printLine("Enter provider name or number:")
	return nil
}

func (a *App) handleSetupInput(line string) error {
	if strings.EqualFold(line, "/cancel") {
		a.setup = nil
		a.printLine("setup canceled")
		return nil
	}

	switch a.setup.step {
	case 0:
		switch strings.TrimSpace(strings.ToLower(line)) {
		case "1", "openai-compatible":
			a.setup.provider = "openai-compatible"
		case "2", "anthropic":
			a.setup.provider = "anthropic"
		default:
			return fmt.Errorf("invalid provider, choose 1/2 or provider name")
		}
		a.setup.step = 1
		a.printLine("Select model:")
		for _, m := range a.providers.Models() {
			if m.Provider == a.setup.provider {
				a.printLine(fmt.Sprintf("- %s", m.Name))
			}
		}
		a.printLine("Enter model name:")
		return nil
	case 1:
		model := strings.TrimSpace(line)
		if !a.providers.HasModel(a.setup.provider, model) {
			return fmt.Errorf("unknown model for provider %s", a.setup.provider)
		}
		a.setup.model = model
		a.setup.step = 2
		a.printLine("Paste API key (input is not masked in current terminal):")
		return nil
	case 2:
		key := strings.TrimSpace(line)
		if key == "" {
			return fmt.Errorf("api key cannot be empty")
		}
		if err := config.SaveAPIKey(a.setup.provider, key); err != nil {
			return err
		}
		a.setup.step = 3
		a.printLine("Choose default permission:")
		a.printLine("1) ask-first")
		a.printLine("2) restricted")
		a.printLine("3) yolo")
		a.printLine("Enter value:")
		return nil
	case 3:
		switch strings.TrimSpace(strings.ToLower(line)) {
		case "1", "ask-first":
			a.cfg.Permission = config.PermissionAskFirst
		case "2", "restricted":
			a.cfg.Permission = config.PermissionRestricted
		case "3", "yolo":
			a.cfg.Permission = config.PermissionYOLO
		default:
			return fmt.Errorf("invalid permission, choose ask-first/restricted/yolo")
		}

		if err := a.updateConfig(func(cfg *config.UserConfig) error {
			cfg.ActiveProvider = a.setup.provider
			cfg.ActiveModel = a.setup.model
			cfg.Permission = a.cfg.Permission
			return nil
		}); err != nil {
			return err
		}
		a.setup = nil
		a.printLine(a.ui.Panel(string(a.mode), "Setup Complete", fmt.Sprintf("Active provider/model: %s:%s", a.cfg.ActiveProvider, a.cfg.ActiveModel)))
		a.printStatus()
		return nil
	default:
		return fmt.Errorf("invalid setup state")
	}
}

func (a *App) startModelPicker(prefix string) {
	a.modelPicker = &modelPicker{filter: strings.ToLower(strings.TrimSpace(prefix))}
	a.modelPicker.items = a.modelPickerMatches(a.modelPicker.filter)
	if len(a.modelPicker.items) == 0 {
		a.printLine("no model matches found")
		a.modelPicker = nil
		return
	}
	a.printLine(a.ui.Panel(string(a.mode), "Model Picker", "Type a number to select model.\nType text to filter.\nType /cancel to close picker."))
	for i, m := range a.modelPicker.items {
		a.printLine(a.ui.Info(fmt.Sprintf("%d) %s:%s (vision=%t)", i+1, m.Provider, m.Name, m.Vision)))
	}
}

func (a *App) handleModelPickerInput(line string) error {
	if strings.EqualFold(line, "/cancel") {
		a.modelPicker = nil
		a.printLine("model picker closed")
		return nil
	}
	if n, err := parseSelection(line); err == nil {
		if n < 1 || n > len(a.modelPicker.items) {
			return fmt.Errorf("selection out of range")
		}
		selected := a.modelPicker.items[n-1]
		if err := a.updateConfig(func(cfg *config.UserConfig) error {
			cfg.ActiveProvider = selected.Provider
			cfg.ActiveModel = selected.Name
			return nil
		}); err != nil {
			return err
		}
		a.modelPicker = nil
		a.printStatus()
		return nil
	}
	a.startModelPicker(line)
	return nil
}

func parseSelection(line string) (int, error) {
	var n int
	_, err := fmt.Sscanf(strings.TrimSpace(line), "%d", &n)
	return n, err
}

func (a *App) modelPickerMatches(prefix string) []provider.Model {
	matches := make([]provider.Model, 0)
	for _, m := range a.providers.Models() {
		full := strings.ToLower(m.Provider + ":" + m.Name)
		if prefix == "" || strings.Contains(full, prefix) {
			matches = append(matches, m)
		}
	}
	return matches
}
