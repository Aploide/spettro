package acp

import (
	"strings"

	acpsdk "github.com/coder/acp-go-sdk"

	"spettro/internal/config"
	"spettro/internal/provider"
)

// Session config option IDs. These are the stable identifiers echoed back by
// the client in session/set_config_option, so they must not change casually.
const (
	configIDMode       = "mode"
	configIDModel      = "model"
	configIDPermission = "permission"
	configIDThinking   = "thinking"
	configIDUltra      = "ultra"
)

// buildConfigOptions renders Spettro's live state (agent mode, model,
// permission level, thinking level) as ACP session configuration options —
// the mechanism modern clients (Zed, ...) use to draw the mode/model/permission
// selectors in their editor toolbar. This supersedes the deprecated
// SessionModeState "modes" field, which newer clients no longer render.
//
// Order matters: the array is the agent's preferred priority, so mode and
// model come first. Caller holds the bridge mutex (reads s.manifest/agentID).
func buildConfigOptions(s *acpSession, cfg *config.UserConfig, pm *provider.Manager) []acpsdk.SessionConfigOption {
	// The thinking selector is ALWAYS present (showing "Off" when disabled):
	// clients build their toolbar from this list once per config update, and
	// the model selector lives in the same UI — hiding thinking based on the
	// active model would make the control flicker in and out (and race model
	// lists that load after the session starts). Non-reasoning models simply
	// never receive the parameter (see internal/provider).
	return []acpsdk.SessionConfigOption{
		modeConfigOption(s),
		modelConfigOption(cfg, pm),
		permissionConfigOption(cfg),
		thinkingConfigOption(cfg),
		ultraConfigOption(cfg),
	}
}

// userFacingModes returns the enabled orchestrator agents — the ones a user
// picks between as a session "mode" (Plan, Coding, Ask). Worker/subagent roles
// are internal delegation targets and are intentionally hidden from the
// selector. Falls back to all enabled agents if none are orchestrators.
func userFacingModes(manifest config.AgentManifest) []config.AgentSpec {
	all := manifest.EnabledAgents()
	out := make([]config.AgentSpec, 0, len(all))
	for _, a := range all {
		if a.Mode == "orchestrator" {
			out = append(out, a)
		}
	}
	if len(out) == 0 {
		return all
	}
	return out
}

func modeConfigOption(s *acpSession) acpsdk.SessionConfigOption {
	modes := userFacingModes(s.manifest)
	options := make(acpsdk.SessionConfigSelectOptionsUngrouped, 0, len(modes))
	for _, a := range modes {
		opt := acpsdk.SessionConfigSelectOption{
			Name:  a.Name,
			Value: acpsdk.SessionConfigValueId(a.ID),
		}
		if a.Description != "" {
			opt.Description = acpsdk.Ptr(a.Description)
		}
		options = append(options, opt)
	}
	return acpsdk.SessionConfigOption{Select: &acpsdk.SessionConfigOptionSelect{
		Id:           configIDMode,
		Name:         "Mode",
		Category:     acpsdk.Ptr(acpsdk.SessionConfigOptionCategoryMode),
		CurrentValue: acpsdk.SessionConfigValueId(s.agentID),
		Options:      acpsdk.SessionConfigSelectOptions{Ungrouped: &options},
		Type:         "select",
	}}
}

func modelConfigOption(cfg *config.UserConfig, pm *provider.Manager) acpsdk.SessionConfigOption {
	current := cfg.ActiveProvider + ":" + cfg.ActiveModel

	// Group connected models under their provider name for a readable dropdown.
	type group struct {
		name    string
		options []acpsdk.SessionConfigSelectOption
	}
	var groups []*group
	byProvider := map[string]*group{}
	currentListed := false
	for _, m := range pm.ConnectedModels(cfg.APIKeys) {
		value := m.Provider + ":" + m.Name
		if value == current {
			currentListed = true
		}
		label := m.DisplayName
		if label == "" {
			label = m.Name
		}
		g, ok := byProvider[m.Provider]
		if !ok {
			providerName := m.ProviderName
			if providerName == "" {
				providerName = m.Provider
			}
			g = &group{name: providerName}
			byProvider[m.Provider] = g
			groups = append(groups, g)
		}
		g.options = append(g.options, acpsdk.SessionConfigSelectOption{
			Name:  label,
			Value: acpsdk.SessionConfigValueId(value),
		})
	}

	grouped := make(acpsdk.SessionConfigSelectOptionsGrouped, 0, len(groups)+1)
	// Ensure the active model is always selectable even if no key is connected
	// for it, so CurrentValue references a real option.
	if !currentListed && cfg.ActiveModel != "" {
		grouped = append(grouped, acpsdk.SessionConfigSelectGroup{
			Group:   acpsdk.SessionConfigGroupId("active"),
			Name:    "Active",
			Options: []acpsdk.SessionConfigSelectOption{{Name: current, Value: acpsdk.SessionConfigValueId(current)}},
		})
	}
	for i, g := range groups {
		grouped = append(grouped, acpsdk.SessionConfigSelectGroup{
			Group:   acpsdk.SessionConfigGroupId(g.name),
			Name:    g.name,
			Options: groups[i].options,
		})
	}

	return acpsdk.SessionConfigOption{Select: &acpsdk.SessionConfigOptionSelect{
		Id:           configIDModel,
		Name:         "Model",
		Description:  acpsdk.Ptr("Active model for this session"),
		Category:     acpsdk.Ptr(acpsdk.SessionConfigOptionCategoryModel),
		CurrentValue: acpsdk.SessionConfigValueId(current),
		Options:      acpsdk.SessionConfigSelectOptions{Grouped: &grouped},
		Type:         "select",
	}}
}

func permissionConfigOption(cfg *config.UserConfig) acpsdk.SessionConfigOption {
	options := acpsdk.SessionConfigSelectOptionsUngrouped{
		{Name: "Ask first", Value: acpsdk.SessionConfigValueId(config.PermissionAskFirst), Description: acpsdk.Ptr("Prompt before running tools, edits, or commands")},
		{Name: "Restricted", Value: acpsdk.SessionConfigValueId(config.PermissionRestricted), Description: acpsdk.Ptr("Allow safe actions; prompt for sensitive ones")},
		{Name: "YOLO", Value: acpsdk.SessionConfigValueId(config.PermissionYOLO), Description: acpsdk.Ptr("Automatically approve all tool, path, and command requests")},
	}
	current := string(cfg.Permission)
	if current == "" {
		current = string(config.PermissionAskFirst)
	}
	return acpsdk.SessionConfigOption{Select: &acpsdk.SessionConfigOptionSelect{
		Id:           configIDPermission,
		Name:         "Permission",
		Description:  acpsdk.Ptr("How Spettro requests approval for actions"),
		CurrentValue: acpsdk.SessionConfigValueId(current),
		Options:      acpsdk.SessionConfigSelectOptions{Ungrouped: &options},
		Type:         "select",
	}}
}

func thinkingConfigOption(cfg *config.UserConfig) acpsdk.SessionConfigOption {
	options := acpsdk.SessionConfigSelectOptionsUngrouped{
		{Name: "Off", Value: acpsdk.SessionConfigValueId(provider.ThinkingOff)},
		{Name: "Low", Value: acpsdk.SessionConfigValueId(provider.ThinkingLow)},
		{Name: "Medium", Value: acpsdk.SessionConfigValueId(provider.ThinkingMedium)},
		{Name: "High", Value: acpsdk.SessionConfigValueId(provider.ThinkingHigh)},
		{Name: "X-High", Value: acpsdk.SessionConfigValueId(provider.ThinkingXHigh)},
		{Name: "Max", Value: acpsdk.SessionConfigValueId(provider.ThinkingMax)},
	}
	current := strings.TrimSpace(cfg.ThinkingLevel)
	if current == "" {
		current = string(provider.ThinkingOff)
	}
	return acpsdk.SessionConfigOption{Select: &acpsdk.SessionConfigOptionSelect{
		Id:           configIDThinking,
		Name:         "Thinking",
		Description:  acpsdk.Ptr("Extended-thinking effort"),
		Category:     acpsdk.Ptr(acpsdk.SessionConfigOptionCategoryThoughtLevel),
		CurrentValue: acpsdk.SessionConfigValueId(current),
		Options:      acpsdk.SessionConfigSelectOptions{Ungrouped: &options},
		Type:         "select",
	}}
}

func ultraConfigOption(cfg *config.UserConfig) acpsdk.SessionConfigOption {
	// Ultra is a boolean config option so clients render an on/off toggle
	// instead of a two-entry dropdown.
	return acpsdk.SessionConfigOption{Boolean: &acpsdk.SessionConfigOptionBoolean{
		Id:           configIDUltra,
		Name:         "Ultra",
		Description:  acpsdk.Ptr("Swarm of parallel sub-agents for hard tasks (works with any model)"),
		CurrentValue: cfg.Ultra,
		Type:         "boolean",
	}}
}

// applyConfigOption mutates session/config state in response to a
// session/set_config_option request, mirroring the equivalent slash commands.
// Persistent settings (model, permission, thinking) are written to the user
// config so a concurrent TUI and the next turn observe them. Caller holds the
// bridge mutex. An unknown option or value returns an error the SDK surfaces to
// the client; a nil error means the change was applied.
func (b *bridge) applyConfigOption(s *acpSession, cfg *config.UserConfig, configID, value string) error {
	switch configID {
	case configIDMode:
		if _, ok := s.manifest.AgentByID(value); !ok {
			return acpsdk.NewInvalidParams(map[string]any{"error": "unknown mode: " + value})
		}
		s.agentID = value
		return nil

	case configIDModel:
		parts := strings.SplitN(value, ":", 2)
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			return acpsdk.NewInvalidParams(map[string]any{"error": "invalid model: " + value})
		}
		if !b.opts.Providers.HasModel(parts[0], parts[1]) {
			return acpsdk.NewInvalidParams(map[string]any{"error": "unknown model: " + value})
		}
		if _, err := config.Update(func(c *config.UserConfig) error {
			c.ActiveProvider = parts[0]
			c.ActiveModel = parts[1]
			return nil
		}); err != nil {
			return err
		}
		cfg.ActiveProvider = parts[0]
		cfg.ActiveModel = parts[1]
		return nil

	case configIDPermission:
		level := config.PermissionLevel(value)
		switch level {
		case config.PermissionYOLO, config.PermissionRestricted, config.PermissionAskFirst:
		default:
			return acpsdk.NewInvalidParams(map[string]any{"error": "invalid permission: " + value})
		}
		if _, err := config.Update(func(c *config.UserConfig) error {
			c.Permission = level
			return nil
		}); err != nil {
			return err
		}
		cfg.Permission = level
		// Also update the session's live level so an in-flight run switches
		// enforcement immediately (caller holds b.mu).
		s.permission = level
		return nil

	case configIDThinking:
		level := strings.ToLower(strings.TrimSpace(value))
		if !provider.IsValidThinkingLevel(level) {
			return acpsdk.NewInvalidParams(map[string]any{"error": "invalid thinking level: " + value})
		}
		if level == string(provider.ThinkingOff) {
			level = ""
		}
		if _, err := config.Update(func(c *config.UserConfig) error {
			c.ThinkingLevel = level
			return nil
		}); err != nil {
			return err
		}
		cfg.ThinkingLevel = level
		return nil

	case configIDUltra:
		var enabled bool
		// Accept the boolean wire value ("true"/"false") plus the legacy
		// select values ("on"/"off") so older clients still work.
		switch strings.ToLower(strings.TrimSpace(value)) {
		case "on", "true":
			enabled = true
		case "off", "false":
			enabled = false
		default:
			return acpsdk.NewInvalidParams(map[string]any{"error": "invalid ultra value: " + value})
		}
		// A swarm runs many sub-agents concurrently; per-action approval
		// prompts would flood the client, so Ultra requires restricted or yolo.
		if enabled && cfg.Permission == config.PermissionAskFirst {
			return acpsdk.NewInvalidParams(map[string]any{"error": "ultra requires the Restricted or YOLO permission level — change Permission first"})
		}
		if _, err := config.Update(func(c *config.UserConfig) error {
			c.Ultra = enabled
			return nil
		}); err != nil {
			return err
		}
		cfg.Ultra = enabled
		return nil
	}
	return acpsdk.NewInvalidParams(map[string]any{"error": "unknown config option: " + configID})
}
