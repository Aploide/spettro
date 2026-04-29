package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

type PermissionLevel string

const (
	PermissionYOLO       PermissionLevel = "yolo"
	PermissionRestricted PermissionLevel = "restricted"
	PermissionAskFirst   PermissionLevel = "ask-first"
)

type UserConfig struct {
	ActiveProvider          string            `json:"active_provider"`
	ActiveModel             string            `json:"active_model"`
	Permission              PermissionLevel   `json:"permission"`
	TokenBudget             int               `json:"token_budget,omitempty"` // max tokens per request; 0 = unlimited
	AutoCompactEnabled      bool              `json:"auto_compact_enabled"`
	AutoCompactThresholdPct int               `json:"auto_compact_threshold_pct,omitempty"`
	AutoCompactMaxFailures  int               `json:"auto_compact_max_failures,omitempty"`
	APIKeys                 map[string]string `json:"api_keys,omitempty"`
	LocalEndpoints          []string          `json:"local_endpoints,omitempty"`
	Favorites               []string          `json:"favorites,omitempty"` // "provider:model"
	LastAgentID             string            `json:"last_agent_id,omitempty"`
	ShowSidePanel           bool              `json:"show_side_panel,omitempty"`
	ShowPermissionDebug     bool              `json:"show_permission_debug,omitempty"`
	// ThinkingLevel selects extended-thinking compute when the active model
	// supports it. Allowed values are "off", "low", "medium", "high", "x-high"
	// (or empty, which is treated as "off"). Toggleable at runtime via the
	// /thinking command and honoured by the Anthropic and Devin Sessions
	// adapters; other providers ignore it.
	ThinkingLevel string `json:"thinking_level,omitempty"`
	// DevinOrgID is required when ActiveProvider is "devin". The Devin v3 API
	// scopes session endpoints under /v3/organizations/{org_id}/sessions, and
	// the org id is discoverable from the Devin dashboard.
	DevinOrgID string `json:"devin_org_id,omitempty"`
}

func Default() UserConfig {
	return UserConfig{
		ActiveProvider:          "openai-compatible",
		ActiveModel:             "gpt-5-mini",
		Permission:              PermissionAskFirst,
		AutoCompactEnabled:      true,
		AutoCompactThresholdPct: 85,
		AutoCompactMaxFailures:  3,
		APIKeys: map[string]string{
			"openai-compatible": "",
			"anthropic":         "",
		},
	}
}

func normalize(cfg UserConfig) (UserConfig, bool) {
	def := Default()
	changed := false
	legacyAutoCompactUnset := cfg.AutoCompactThresholdPct == 0 && cfg.AutoCompactMaxFailures == 0

	if cfg.ActiveProvider == "" {
		cfg.ActiveProvider = def.ActiveProvider
		changed = true
	}
	if cfg.ActiveModel == "" {
		cfg.ActiveModel = def.ActiveModel
		changed = true
	}
	switch cfg.Permission {
	case PermissionYOLO, PermissionRestricted, PermissionAskFirst:
	default:
		cfg.Permission = def.Permission
		changed = true
	}
	if cfg.APIKeys == nil {
		cfg.APIKeys = map[string]string{}
		changed = true
	}
	if legacyAutoCompactUnset && !cfg.AutoCompactEnabled {
		cfg.AutoCompactEnabled = true
		changed = true
	}
	if cfg.AutoCompactThresholdPct <= 0 || cfg.AutoCompactThresholdPct >= 100 {
		cfg.AutoCompactThresholdPct = def.AutoCompactThresholdPct
		changed = true
	}
	if cfg.AutoCompactMaxFailures <= 0 {
		cfg.AutoCompactMaxFailures = def.AutoCompactMaxFailures
		changed = true
	}
	switch cfg.ThinkingLevel {
	case "", "off", "low", "medium", "high", "x-high":
		// valid
	default:
		cfg.ThinkingLevel = ""
		changed = true
	}
	return cfg, changed
}

func Path() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home dir: %w", err)
	}
	return filepath.Join(home, ".spettro", "config.json"), nil
}

func LoadOrCreate() (UserConfig, error) {
	p, err := Path()
	if err != nil {
		return UserConfig{}, err
	}

	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return UserConfig{}, fmt.Errorf("create global config dir: %w", err)
	}

	var cfg UserConfig
	data, err := os.ReadFile(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			cfg = Default()
			if err := Save(cfg); err != nil {
				return UserConfig{}, err
			}
			return cfg, nil
		}
		return UserConfig{}, fmt.Errorf("read config: %w", err)
	}

	if err := json.Unmarshal(data, &cfg); err != nil {
		return UserConfig{}, fmt.Errorf("decode config: %w", err)
	}

	var changed bool
	cfg, changed = normalize(cfg)
	if changed {
		if err := Save(cfg); err != nil {
			return UserConfig{}, err
		}
	}
	return cfg, nil
}

// Load reads the config file without creating it and without persisting
// normalization side effects. Missing files return defaults in-memory.
func Load() (UserConfig, error) {
	p, err := Path()
	if err != nil {
		return UserConfig{}, err
	}
	var cfg UserConfig
	data, err := os.ReadFile(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Default(), nil
		}
		return UserConfig{}, fmt.Errorf("read config: %w", err)
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return UserConfig{}, fmt.Errorf("decode config: %w", err)
	}
	cfg, _ = normalize(cfg)
	return cfg, nil
}

// LoadFull reads persisted config plus encrypted API keys into a single struct.
func LoadFull() (UserConfig, error) {
	cfg, err := LoadOrCreate()
	if err != nil {
		return UserConfig{}, err
	}
	keys, err := LoadAPIKeys()
	if err != nil {
		return UserConfig{}, err
	}
	cfg.APIKeys = keys
	return cfg, nil
}

// Update loads the latest persisted config, applies mut, saves it, and returns
// the updated in-memory view including API keys.
func Update(mut func(*UserConfig) error) (UserConfig, error) {
	cfg, err := LoadFull()
	if err != nil {
		return UserConfig{}, err
	}
	if mut != nil {
		if err := mut(&cfg); err != nil {
			return UserConfig{}, err
		}
	}
	if err := Save(cfg); err != nil {
		return UserConfig{}, err
	}
	return cfg, nil
}

func Save(cfg UserConfig) error {
	p, err := Path()
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return fmt.Errorf("create global config dir: %w", err)
	}

	// Never persist plaintext API keys in config.json.
	scrubbed := cfg
	scrubbed.APIKeys = nil
	raw, err := json.MarshalIndent(scrubbed, "", "  ")
	if err != nil {
		return fmt.Errorf("encode config: %w", err)
	}

	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return fmt.Errorf("write temp config: %w", err)
	}
	return os.Rename(tmp, p)
}
