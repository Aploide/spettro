package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"spettro/internal/compact"
	"spettro/internal/fsperm"
	"spettro/internal/homedir"
	"spettro/internal/safeio"
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
	TokenBudget             int               `json:"token_budget,omitempty"` // max INPUT (prompt) tokens per request; 0 = unlimited
	AutoCompactEnabled      bool              `json:"auto_compact_enabled"`
	AutoCompactThresholdPct int               `json:"auto_compact_threshold_pct,omitempty"`
	AutoCompactMaxFailures  int               `json:"auto_compact_max_failures,omitempty"`
	APIKeys                 map[string]string `json:"api_keys,omitempty"`
	LocalEndpoints          []string          `json:"local_endpoints,omitempty"`
	Favorites               []string          `json:"favorites,omitempty"` // "provider:model"
	LastAgentID             string            `json:"last_agent_id,omitempty"`
	ShowSidePanel           bool              `json:"show_side_panel,omitempty"`
	ShowPermissionDebug     bool              `json:"show_permission_debug,omitempty"`
	// Theme selects the colour palette: "dark", "light" or "auto" (empty is
	// treated as "auto"). Auto asks the terminal for its background colour and
	// degrades to dark when it cannot be determined. The SPETTRO_THEME
	// environment variable overrides this for a single process.
	Theme string `json:"theme,omitempty"`
	// CursorBlink makes the input cursor blink. Off by default: a blinking
	// cursor repaints the frame twice a second for as long as the TUI is
	// open, which was most of an idle TUI's CPU once nothing else woke it.
	CursorBlink bool `json:"cursor_blink,omitempty"`
	// ThinkingLevel selects reasoning compute when the active model supports
	// it. Allowed values are "off", "low", "medium", "high", "x-high", "max",
	// or empty (never set: no thinking parameter is sent and the provider's
	// default applies). Toggleable at runtime via the /thinking command and
	// honoured by the TUI, headless and ACP runs alike: Anthropic gets a
	// thinking token budget; OpenAI and OpenAI-compatible backends (the Spettro
	// Subscription included) get reasoning_effort (low/medium/high, x-high and
	// max as "xhigh", an explicit "off" as "none").
	ThinkingLevel string `json:"thinking_level,omitempty"`
	// MaxOutputTokens caps each model reply (max_tokens on the wire). 0 =
	// auto: the model's known output limit, or 32000 for Anthropic-protocol
	// models whose limit is unknown (their implicit default is only 4096).
	// Distinct from TokenBudget, which limits the prompt.
	MaxOutputTokens int `json:"max_output_tokens,omitempty"`
	// ProviderWire selects the client that carries streamed requests to
	// OpenAI-compatible chat-completions endpoints (OpenAI-compatible
	// catalog providers, the Spettro Subscription, local servers): "native"
	// (empty means native) is Spettro's own client, "fantasy" the fantasy
	// SDK it replaced, kept as a fallback. The SPETTRO_PROVIDER_WIRE
	// environment variable overrides it for one process. Anthropic and the
	// official OpenAI provider always use fantasy.
	ProviderWire string `json:"provider_wire,omitempty"`
	// Ultra, when true, injects the ultra fan-out tool and swarm guidance into
	// the top-level agent so it decomposes hard tasks across many parallel
	// sub-agents. Works with any model (sub-agents inherit the active model).
	// Toggleable at runtime via /ultra (TUI) or the "ultra" ACP config option.
	Ultra bool `json:"ultra,omitempty"`
	// WorkflowSize is the size guideline for workflow runs: "small",
	// "medium", "large" or "unbounded" (empty means medium). It scales how
	// many agents the model plans a workflow around and is exposed to scripts
	// as the size global; it is a guideline, not a hard cap. Set via
	// /workflows size (TUI) or the "workflow_size" ACP config option.
	WorkflowSize string `json:"workflow_size,omitempty"`

	// SkillsCompatDisabled switches off skill discovery in other agents'
	// directories (.agents/skills, .claude/skills, .codex/skills,
	// .openai/skills, in the project and the home directory), leaving only
	// Spettro's own .spettro/skills. The zero value keeps it on, so skills
	// installed for Claude Code or Codex work out of the box. See
	// docs/skills.md.
	SkillsCompatDisabled bool `json:"skills_compat_disabled,omitempty"`
	// DisabledSkills names skills hidden from both the model and the slash
	// menu (case-insensitive). /skill disable and /skill enable edit it; it
	// is kept here rather than as a file in the skill's folder so the
	// Claude Code and Codex directories are never written to.
	DisabledSkills []string `json:"disabled_skills,omitempty"`

	// Spettro Subscription state. The ep_ API key itself lives in the encrypted
	// keys store under the "spettro" provider; these fields cache the last-known
	// plan info so the top bar can render it before the network refresh lands.
	SpettroEmail      string `json:"spettro_email,omitempty"`
	SpettroPlan       string `json:"spettro_plan,omitempty"`
	SpettroPlanStatus string `json:"spettro_plan_status,omitempty"`

	// Notifications: OSC 9 terminal escape (system notification in iTerm2,
	// WezTerm, Ghostty, Kitty; BEL elsewhere) plus a best-effort desktop
	// notification. Disabled flag keeps the zero value meaning "on".
	NotificationsDisabled bool `json:"notifications_disabled,omitempty"`
	NotifyQuietSec        int  `json:"notify_quiet_sec,omitempty"` // min seconds between notifications; 0 → default (5)

	// Checkpointing (/rewind) shadow-git storage. Zero values fall back to
	// the checkpoint package defaults (20 MB / 14 days / 5 GB / 2 GB).
	CheckpointingDisabled   bool `json:"checkpointing_disabled,omitempty"`
	CheckpointMaxFileMB     int  `json:"checkpoint_max_file_mb,omitempty"`    // files above this are not snapshotted
	CheckpointRetentionDays int  `json:"checkpoint_retention_days,omitempty"` // prune checkpoints older than this on open
	CheckpointMaxGB         int  `json:"checkpoint_max_gb,omitempty"`         // shadow-store size cap enforced on open
	CheckpointWarnGB        int  `json:"checkpoint_warn_gb,omitempty"`        // big-repo warning threshold (no project .git)

	// Storage cleanup (/storage clean, `spettro clean`) session policy. Zero
	// values fall back to the storage package defaults (30 days / keep 5).
	CleanSessionAgeDays int `json:"clean_session_age_days,omitempty"` // sessions older than this are clean candidates
	CleanKeepSessions   int `json:"clean_keep_sessions,omitempty"`    // most recent K sessions per project always survive

	// RipgrepDownloadDisabled stops grep from downloading ripgrep into
	// ~/.spettro/bin when rg is not on PATH (see internal/ripgrep); grep
	// then keeps using its built-in Go search.
	RipgrepDownloadDisabled bool `json:"ripgrep_download_disabled,omitempty"`

	// Goal mode (/goal): autonomous run-until-done.
	GoalShellTimeoutSec int `json:"goal_shell_timeout_sec,omitempty"` // per shell/bash tool call in goal runs; 0 → default (600s)
	GoalMaxIterations   int `json:"goal_max_iterations,omitempty"`    // outer-loop safety cap; 0 → unlimited
	GoalNoProgressLimit int `json:"goal_no_progress_limit,omitempty"` // consecutive no-progress iterations before stalling; 0 → default (3)
	GoalIterationSteps  int `json:"goal_iteration_steps,omitempty"`   // LLM steps per goal iteration before yielding to the outer loop; 0 → default (25)
}

// UltraActive reports whether Ultra mode should actually engage: the toggle is
// on AND the permission level allows unattended sub-agents. A swarm under
// ask-first would flood the user with per-action approval prompts, so Ultra is
// suspended (not cleared) while ask-first is selected.
func (c UserConfig) UltraActive() bool {
	return c.Ultra && c.Permission != PermissionAskFirst
}

// Workflow size tiers accepted by WorkflowSize.
const (
	WorkflowSizeSmall     = "small"
	WorkflowSizeMedium    = "medium"
	WorkflowSizeLarge     = "large"
	WorkflowSizeUnbounded = "unbounded"
)

// WorkflowSizes lists the tiers in ascending order, for pickers and help text.
var WorkflowSizes = []string{WorkflowSizeSmall, WorkflowSizeMedium, WorkflowSizeLarge, WorkflowSizeUnbounded}

// WorkflowSizeTier returns the effective size tier: the configured one, or
// medium when none was chosen.
func (c UserConfig) WorkflowSizeTier() string {
	if c.WorkflowSize == "" {
		return WorkflowSizeMedium
	}
	return c.WorkflowSize
}

// CompactConfig maps the user's auto-compaction settings to the compact
// package's policy struct, so every host (TUI, headless, goal, ACP) hands the
// same policy to the run loop's in-loop compaction.
func (c UserConfig) CompactConfig() compact.Config {
	return compact.Config{
		AutoEnabled:      c.AutoCompactEnabled,
		AutoThresholdPct: c.AutoCompactThresholdPct,
		MaxFailures:      c.AutoCompactMaxFailures,
	}
}

func Default() UserConfig {
	return UserConfig{
		// ActiveProvider/ActiveModel intentionally start empty: hardcoding a
		// model here surfaces one the user has no key for. The active model is
		// resolved at startup from whichever credentials actually exist
		// (Spettro sign-in, API key, local endpoint) and set by onboarding.
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
	case "", "off", "low", "medium", "high", "x-high", "max":
		// valid
	default:
		cfg.ThinkingLevel = ""
		changed = true
	}
	switch cfg.Theme {
	case "", "auto", "dark", "light":
		// valid ("" means auto)
	default:
		cfg.Theme = ""
		changed = true
	}
	switch cfg.WorkflowSize {
	case "", WorkflowSizeSmall, WorkflowSizeMedium, WorkflowSizeLarge, WorkflowSizeUnbounded:
		// valid ("" means medium)
	default:
		cfg.WorkflowSize = ""
		changed = true
	}
	if cfg.NotifyQuietSec <= 0 {
		cfg.NotifyQuietSec = 5
		changed = true
	}
	if cfg.GoalShellTimeoutSec <= 0 {
		cfg.GoalShellTimeoutSec = 600 // 10 minutes for long-running installs/builds
		changed = true
	}
	if cfg.GoalNoProgressLimit <= 0 {
		cfg.GoalNoProgressLimit = 3
		changed = true
	}
	if cfg.GoalIterationSteps <= 0 {
		// Bounds each iteration's inner tool loop so the outer goal loop
		// (progress detection, stall guard, iteration cap) actually runs.
		cfg.GoalIterationSteps = 25
		changed = true
	}
	// GoalMaxIterations: 0 means unlimited, no default needed
	return cfg, changed
}

func Path() (string, error) {
	home, err := homedir.Dir()
	if err != nil {
		return "", fmt.Errorf("resolve home dir: %w", err)
	}
	return filepath.Join(home, ".spettro", "config.json"), nil
}

// configMu serializes the read-modify-write cycles below. Several front-ends
// touch the config concurrently — the ACP bridge in particular services
// independent extension requests on their own goroutines — and Update's
// load/mutate/save was previously racy in two ways: concurrent writers shared
// one temp filename (so one's rename destroyed the other's), and interleaved
// updates silently dropped each other's changes.
var configMu sync.Mutex

func LoadOrCreate() (UserConfig, error) {
	configMu.Lock()
	defer configMu.Unlock()
	return loadOrCreateLocked()
}

func loadOrCreateLocked() (UserConfig, error) {
	p, err := Path()
	if err != nil {
		return UserConfig{}, err
	}

	if err := fsperm.SecureMkdirAll(filepath.Dir(p)); err != nil {
		return UserConfig{}, fmt.Errorf("create global config dir: %w", err)
	}

	var cfg UserConfig
	data, err := os.ReadFile(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			cfg = Default()
			if err := saveLocked(cfg); err != nil {
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
		if err := saveLocked(cfg); err != nil {
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
	configMu.Lock()
	defer configMu.Unlock()
	return loadFullLocked()
}

func loadFullLocked() (UserConfig, error) {
	cfg, err := loadOrCreateLocked()
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
// the updated in-memory view including API keys. The whole cycle holds
// configMu, so concurrent callers can't read the same base config and then
// overwrite each other's field changes.
func Update(mut func(*UserConfig) error) (UserConfig, error) {
	configMu.Lock()
	defer configMu.Unlock()

	cfg, err := loadFullLocked()
	if err != nil {
		return UserConfig{}, err
	}
	if mut != nil {
		if err := mut(&cfg); err != nil {
			return UserConfig{}, err
		}
	}
	if err := saveLocked(cfg); err != nil {
		return UserConfig{}, err
	}
	return cfg, nil
}

func Save(cfg UserConfig) error {
	configMu.Lock()
	defer configMu.Unlock()
	return saveLocked(cfg)
}

func saveLocked(cfg UserConfig) error {
	p, err := Path()
	if err != nil {
		return err
	}

	dir := filepath.Dir(p)
	if err := fsperm.SecureMkdirAll(dir); err != nil {
		return fmt.Errorf("create global config dir: %w", err)
	}

	// Never persist plaintext API keys in config.json.
	scrubbed := cfg
	scrubbed.APIKeys = nil
	raw, err := json.MarshalIndent(scrubbed, "", "  ")
	if err != nil {
		return fmt.Errorf("encode config: %w", err)
	}

	// A unique temp file per write, not a fixed "config.json.tmp": two writers
	// sharing that one path meant the first rename moved the file out from
	// under the second, which then failed with ENOENT and took an otherwise
	// healthy config read down with it.
	tmp, err := os.CreateTemp(dir, "config-*.json.tmp")
	if err != nil {
		return fmt.Errorf("create temp config: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename below succeeds

	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp config: %w", err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("secure temp config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp config: %w", err)
	}
	return safeio.Replace(tmpName, p)
}
