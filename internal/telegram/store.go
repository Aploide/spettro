package telegram

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"spettro/internal/config"
)

// keyName is the entry used inside Spettro's encrypted secrets store
// (~/.spettro/keys.enc). The Bot API token is held alongside provider API
// keys so a single master secret protects every piece of remote-access
// credential material.
const keyName = "telegram-bot"

// configFile is the plaintext relay configuration written to
// ~/.spettro/telegram.json. It holds non-secret state: allowlist entries,
// auto-start preference, the last seen update id (so polling is idempotent
// across restarts) and the bot's own username (for help text).
const configFile = "telegram.json"

// AllowEntry describes a single allowlisted Telegram chat. Exactly one of
// (Username, ChatID) is populated, mirroring how ParseChatTarget normalises
// user input.
type AllowEntry struct {
	Username string `json:"username,omitempty"`
	ChatID   int64  `json:"chat_id,omitempty"`
}

// String renders the entry in the canonical "@username" / "12345" form.
func (e AllowEntry) String() string {
	return FormatChatTarget(e.Username, e.ChatID)
}

// Matches reports whether the entry refers to the given user/chat.
//
// Username matching is case-insensitive; we expect both sides to be stored
// lowercase.
func (e AllowEntry) Matches(username string, userID, chatID int64) bool {
	switch {
	case e.Username != "":
		return strings.EqualFold(e.Username, strings.TrimPrefix(username, "@"))
	case e.ChatID != 0:
		return e.ChatID == chatID || e.ChatID == userID
	default:
		return false
	}
}

// PersistedConfig is the JSON layout serialised to telegram.json.
type PersistedConfig struct {
	BotUsername  string       `json:"bot_username,omitempty"`
	Allowlist    []AllowEntry `json:"allowlist,omitempty"`
	AutoStart    bool         `json:"auto_start,omitempty"`
	Verbose      bool         `json:"verbose,omitempty"`
	LastUpdateID int64        `json:"last_update_id,omitempty"`
}

// configMu serialises writes to telegram.json across goroutines.
//
// telegram.json is a global file (~/.spettro/telegram.json); concurrent
// callers — the relay's poll loop persisting the last-seen offset and
// the TUI handling /telegram allow — must not race on the file rename.
var configMu sync.Mutex

// configPath returns the absolute path to telegram.json.
func configPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("telegram: resolve home dir: %w", err)
	}
	return filepath.Join(home, ".spettro", configFile), nil
}

// LoadConfig reads telegram.json, returning the zero value if missing.
func LoadConfig() (PersistedConfig, error) {
	p, err := configPath()
	if err != nil {
		return PersistedConfig{}, err
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return PersistedConfig{}, nil
		}
		return PersistedConfig{}, fmt.Errorf("telegram: read config: %w", err)
	}
	var cfg PersistedConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return PersistedConfig{}, fmt.Errorf("telegram: decode config: %w", err)
	}
	cfg = NormaliseConfig(cfg)
	return cfg, nil
}

// SaveConfig atomically writes telegram.json.
func SaveConfig(cfg PersistedConfig) error {
	configMu.Lock()
	defer configMu.Unlock()

	p, err := configPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return fmt.Errorf("telegram: ensure config dir: %w", err)
	}
	cfg = NormaliseConfig(cfg)
	raw, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("telegram: encode config: %w", err)
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return fmt.Errorf("telegram: write temp config: %w", err)
	}
	return os.Rename(tmp, p)
}

// UpdateConfig loads telegram.json, applies mut, and writes the result.
func UpdateConfig(mut func(*PersistedConfig) error) (PersistedConfig, error) {
	cfg, err := LoadConfig()
	if err != nil {
		return PersistedConfig{}, err
	}
	if mut != nil {
		if err := mut(&cfg); err != nil {
			return PersistedConfig{}, err
		}
	}
	if err := SaveConfig(cfg); err != nil {
		return PersistedConfig{}, err
	}
	return cfg, nil
}

// NormaliseConfig deduplicates and sorts the allowlist so it is stable on
// disk and free of accidental empty entries.
func NormaliseConfig(cfg PersistedConfig) PersistedConfig {
	cfg.BotUsername = strings.TrimSpace(strings.TrimPrefix(cfg.BotUsername, "@"))
	out := cfg.Allowlist[:0:0]
	seen := map[string]struct{}{}
	for _, e := range cfg.Allowlist {
		e.Username = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(e.Username, "@")))
		if e.Username == "" && e.ChatID == 0 {
			continue
		}
		key := e.String()
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, e)
	}
	sort.SliceStable(out, func(i, j int) bool {
		// Numerics first, then usernames alphabetically. Avoids surprising
		// reorders when the user reads `/telegram list`.
		switch {
		case out[i].ChatID != 0 && out[j].ChatID == 0:
			return true
		case out[i].ChatID == 0 && out[j].ChatID != 0:
			return false
		case out[i].ChatID != 0 && out[j].ChatID != 0:
			return out[i].ChatID < out[j].ChatID
		default:
			return out[i].Username < out[j].Username
		}
	})
	cfg.Allowlist = out
	if cfg.LastUpdateID < 0 {
		cfg.LastUpdateID = 0
	}
	return cfg
}

// AddAllowEntry inserts entry into the allowlist iff it is not already
// present. Returns the post-update config and a flag for whether the list
// changed.
func AddAllowEntry(cfg PersistedConfig, entry AllowEntry) (PersistedConfig, bool) {
	entry.Username = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(entry.Username, "@")))
	if entry.Username == "" && entry.ChatID == 0 {
		return cfg, false
	}
	for _, e := range cfg.Allowlist {
		if e.Username != "" && e.Username == entry.Username {
			return cfg, false
		}
		if e.ChatID != 0 && e.ChatID == entry.ChatID {
			return cfg, false
		}
	}
	cfg.Allowlist = append(cfg.Allowlist, entry)
	return NormaliseConfig(cfg), true
}

// RemoveAllowEntry deletes entries that match the given identifier (either
// username or chat ID). Returns the updated config and the count of removed
// entries.
func RemoveAllowEntry(cfg PersistedConfig, username string, chatID int64) (PersistedConfig, int) {
	username = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(username, "@")))
	kept := cfg.Allowlist[:0]
	removed := 0
	for _, e := range cfg.Allowlist {
		switch {
		case username != "" && strings.EqualFold(e.Username, username):
			removed++
		case chatID != 0 && e.ChatID == chatID:
			removed++
		default:
			kept = append(kept, e)
		}
	}
	cfg.Allowlist = kept
	return NormaliseConfig(cfg), removed
}

// IsAllowed reports whether (username, userID, chatID) matches any entry in
// the allowlist.
func IsAllowed(cfg PersistedConfig, username string, userID, chatID int64) bool {
	for _, e := range cfg.Allowlist {
		if e.Matches(username, userID, chatID) {
			return true
		}
	}
	return false
}

// SaveToken stores the bot token in the encrypted secrets file. Empty
// tokens clear the entry. This wraps config.SaveAPIKey to keep the secret
// path in one place.
func SaveToken(token string) error {
	return config.SaveAPIKey(keyName, strings.TrimSpace(token))
}

// LoadToken reads the bot token from the encrypted secrets file. Returns
// an empty string if no token has been set.
func LoadToken() (string, error) {
	keys, err := config.LoadAPIKeys()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(keys[keyName]), nil
}
