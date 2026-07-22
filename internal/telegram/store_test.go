package telegram

import (
	"testing"
)

func TestAllowEntryMatches(t *testing.T) {
	byName := AllowEntry{Username: "carlo"}
	if !byName.Matches("Carlo", 1, 2) || !byName.Matches("@carlo", 1, 2) {
		t.Error("username matching must be case-insensitive and @-tolerant")
	}
	if byName.Matches("other", 1, 2) {
		t.Error("different username must not match")
	}
	byID := AllowEntry{ChatID: 42}
	if !byID.Matches("x", 42, 0) || !byID.Matches("x", 0, 42) {
		t.Error("chat ID must match either user or chat ID")
	}
	if byID.Matches("x", 1, 2) {
		t.Error("wrong IDs must not match")
	}
	if (AllowEntry{}).Matches("any", 1, 1) {
		t.Error("empty entry must never match")
	}
}

func TestNormaliseConfig(t *testing.T) {
	cfg := NormaliseConfig(PersistedConfig{
		BotUsername: "@MyBot ",
		Allowlist: []AllowEntry{
			{Username: "Zeta"},
			{Username: "@zeta"}, // dup after normalisation
			{Username: "alpha"},
			{ChatID: 99},
			{ChatID: 5},
			{}, // empty entry dropped
		},
		LastUpdateID: -3,
	})
	if cfg.BotUsername != "MyBot" {
		t.Errorf("BotUsername = %q", cfg.BotUsername)
	}
	if cfg.LastUpdateID != 0 {
		t.Errorf("negative LastUpdateID not clamped: %d", cfg.LastUpdateID)
	}
	want := []string{"5", "99", "@alpha", "@zeta"}
	if len(cfg.Allowlist) != len(want) {
		t.Fatalf("allowlist = %+v", cfg.Allowlist)
	}
	for i, e := range cfg.Allowlist {
		if e.String() != want[i] {
			t.Errorf("allowlist[%d] = %q, want %q", i, e.String(), want[i])
		}
	}
}

func TestAddRemoveAllowEntry(t *testing.T) {
	cfg := PersistedConfig{}
	cfg, added := AddAllowEntry(cfg, AllowEntry{Username: "@Carlo"})
	if !added || len(cfg.Allowlist) != 1 || cfg.Allowlist[0].Username != "carlo" {
		t.Fatalf("add failed: %+v", cfg.Allowlist)
	}
	if _, added := AddAllowEntry(cfg, AllowEntry{Username: "CARLO"}); added {
		t.Error("duplicate username must not be added")
	}
	if _, added := AddAllowEntry(cfg, AllowEntry{}); added {
		t.Error("empty entry must not be added")
	}
	cfg, _ = AddAllowEntry(cfg, AllowEntry{ChatID: 7})
	if _, added := AddAllowEntry(cfg, AllowEntry{ChatID: 7}); added {
		t.Error("duplicate chat ID must not be added")
	}

	if !IsAllowed(cfg, "@carlo", 0, 0) || !IsAllowed(cfg, "", 7, 0) {
		t.Error("IsAllowed must match listed entries")
	}
	if IsAllowed(cfg, "stranger", 1, 2) {
		t.Error("unlisted user must be denied")
	}

	cfg, removed := RemoveAllowEntry(cfg, "carlo", 0)
	if removed != 1 || len(cfg.Allowlist) != 1 {
		t.Errorf("remove by name: removed=%d list=%+v", removed, cfg.Allowlist)
	}
	cfg, removed = RemoveAllowEntry(cfg, "", 7)
	if removed != 1 || len(cfg.Allowlist) != 0 {
		t.Errorf("remove by ID: removed=%d list=%+v", removed, cfg.Allowlist)
	}
	if _, removed := RemoveAllowEntry(cfg, "ghost", 0); removed != 0 {
		t.Error("removing missing entry must remove nothing")
	}
}

func TestSaveLoadConfigRoundTrip(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Allowlist) != 0 {
		t.Errorf("fresh config not empty: %+v", cfg)
	}

	cfg.BotUsername = "bot"
	cfg.Allowlist = []AllowEntry{{Username: "carlo"}, {ChatID: 3}}
	cfg.AutoStart = true
	cfg.LastUpdateID = 12
	if err := SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	got, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if got.BotUsername != "bot" || !got.AutoStart || got.LastUpdateID != 12 || len(got.Allowlist) != 2 {
		t.Errorf("round-trip = %+v", got)
	}

	got, err = UpdateConfig(func(c *PersistedConfig) error {
		c.LastUpdateID = 99
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.LastUpdateID != 99 {
		t.Errorf("UpdateConfig result = %+v", got)
	}
	reloaded, _ := LoadConfig()
	if reloaded.LastUpdateID != 99 {
		t.Errorf("UpdateConfig not persisted: %+v", reloaded)
	}
}
