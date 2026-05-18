package telegram_test

import (
	"os"
	"path/filepath"
	"testing"

	"spettro/internal/telegram"
)

// withHomeDir redirects HOME to a temporary directory so persistence tests
// can read and write ~/.spettro/telegram.json + keys.enc without touching
// the developer's real config.
func withHomeDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir) // Windows fallback used by os.UserHomeDir
	t.Setenv("SPETTRO_MASTER_KEY", "test-master-key-do-not-leak")
	return dir
}

// TestLoadConfig_MissingReturnsZero is the no-config-yet case.
func TestLoadConfig_MissingReturnsZero(t *testing.T) {
	withHomeDir(t)
	cfg, err := telegram.LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if len(cfg.Allowlist) != 0 || cfg.AutoStart || cfg.BotUsername != "" || cfg.LastUpdateID != 0 {
		t.Fatalf("expected zero config, got %#v", cfg)
	}
}

// TestSaveConfig_RoundTrip writes a fully populated config and reads it
// back, asserting normalisation is stable.
func TestSaveConfig_RoundTrip(t *testing.T) {
	dir := withHomeDir(t)

	input := telegram.PersistedConfig{
		BotUsername: "@MyBot",
		Allowlist: []telegram.AllowEntry{
			{Username: "Carlo"},
			{Username: "@carlo"}, // dup, should drop
			{ChatID: -100},
			{Username: ""}, // garbage, should drop
		},
		AutoStart:    true,
		LastUpdateID: 17,
	}
	if err := telegram.SaveConfig(input); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}

	want := filepath.Join(dir, ".spettro", "telegram.json")
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("config file not created: %v", err)
	}

	got, err := telegram.LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if got.BotUsername != "MyBot" {
		t.Fatalf("bot username not normalised: %q", got.BotUsername)
	}
	if !got.AutoStart || got.LastUpdateID != 17 {
		t.Fatalf("autostart/offset lost: %#v", got)
	}
	if len(got.Allowlist) != 2 {
		t.Fatalf("expected 2 dedup entries, got %d: %#v", len(got.Allowlist), got.Allowlist)
	}
	// Numeric IDs come first per NormaliseConfig.
	if got.Allowlist[0].ChatID != -100 {
		t.Fatalf("ordering wrong: %#v", got.Allowlist)
	}
}

// TestSaveToken_Encrypted persists and recovers the BotFather token via
// the shared secrets file.
func TestSaveToken_Encrypted(t *testing.T) {
	dir := withHomeDir(t)

	const tok = "111:abcDEF"
	if err := telegram.SaveToken(tok); err != nil {
		t.Fatalf("SaveToken: %v", err)
	}
	keysFile := filepath.Join(dir, ".spettro", "keys.enc")
	raw, err := os.ReadFile(keysFile)
	if err != nil {
		t.Fatalf("read keys.enc: %v", err)
	}
	if contains(raw, []byte(tok)) {
		t.Fatal("token leaked in plaintext inside keys.enc")
	}
	got, err := telegram.LoadToken()
	if err != nil {
		t.Fatalf("LoadToken: %v", err)
	}
	if got != tok {
		t.Fatalf("LoadToken mismatch: %q", got)
	}
}

func contains(haystack, needle []byte) bool {
	if len(needle) == 0 {
		return true
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if equal(haystack[i:i+len(needle)], needle) {
			return true
		}
	}
	return false
}

func equal(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestNormaliseConfig_DropsEmpty makes sure the de-duper is robust.
func TestNormaliseConfig_DropsEmpty(t *testing.T) {
	cfg := telegram.PersistedConfig{
		Allowlist: []telegram.AllowEntry{
			{},
			{Username: "   "},
			{Username: "alice"},
			{ChatID: 0},
			{ChatID: 1},
		},
		LastUpdateID: -3,
	}
	cfg = telegram.NormaliseConfig(cfg)
	if len(cfg.Allowlist) != 2 {
		t.Fatalf("expected 2 entries, got %d (%#v)", len(cfg.Allowlist), cfg.Allowlist)
	}
	if cfg.LastUpdateID != 0 {
		t.Fatalf("negative offset should be zeroed, got %d", cfg.LastUpdateID)
	}
}

// TestUpdateConfig_AppliesMutation rewrites part of the persisted state
// and confirms the on-disk file reflects the change.
func TestUpdateConfig_AppliesMutation(t *testing.T) {
	withHomeDir(t)
	if _, err := telegram.UpdateConfig(func(cfg *telegram.PersistedConfig) error {
		cfg.Allowlist = append(cfg.Allowlist, telegram.AllowEntry{Username: "carlo"})
		cfg.AutoStart = true
		return nil
	}); err != nil {
		t.Fatalf("UpdateConfig: %v", err)
	}
	cfg, err := telegram.LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if !cfg.AutoStart {
		t.Fatal("AutoStart did not persist")
	}
	if len(cfg.Allowlist) != 1 || cfg.Allowlist[0].Username != "carlo" {
		t.Fatalf("allowlist did not persist: %#v", cfg.Allowlist)
	}
}
