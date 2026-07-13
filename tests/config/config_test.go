package config_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"spettro/internal/config"
)

func TestLoadOrCreateRoundTrip(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	cfg, err := config.LoadOrCreate()
	if err != nil {
		t.Fatalf("load/create: %v", err)
	}

	cfg.ActiveProvider = "anthropic"
	cfg.ActiveModel = "claude-3-7-sonnet"
	cfg.LastAgentID = "coding"
	cfg.ShowSidePanel = true
	cfg.GoalShellTimeoutSec = 900
	cfg.GoalMaxIterations = 10
	cfg.GoalNoProgressLimit = 5
	if err := config.Save(cfg); err != nil {
		t.Fatalf("save: %v", err)
	}

	reloaded, err := config.LoadOrCreate()
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.ActiveProvider != "anthropic" {
		t.Fatalf("expected anthropic provider, got %s", reloaded.ActiveProvider)
	}
	if reloaded.ActiveModel != "claude-3-7-sonnet" {
		t.Fatalf("expected saved model, got %s", reloaded.ActiveModel)
	}
	if reloaded.Permission != config.PermissionAskFirst {
		t.Fatalf("expected saved permission, got %s", reloaded.Permission)
	}
	if reloaded.LastAgentID != "coding" {
		t.Fatalf("expected saved last agent, got %s", reloaded.LastAgentID)
	}
	if !reloaded.ShowSidePanel {
		t.Fatal("expected side panel preference to persist")
	}
	if reloaded.GoalShellTimeoutSec != 900 {
		t.Fatalf("expected goal shell timeout 900, got %d", reloaded.GoalShellTimeoutSec)
	}
	if reloaded.GoalMaxIterations != 10 {
		t.Fatalf("expected goal max iterations 10, got %d", reloaded.GoalMaxIterations)
	}
	if reloaded.GoalNoProgressLimit != 5 {
		t.Fatalf("expected goal no progress limit 5, got %d", reloaded.GoalNoProgressLimit)
	}

	p := filepath.Join(tmpHome, ".spettro", "config.json")
	info, err := os.Stat(p)
	if err != nil {
		t.Fatalf("stat config: %v", err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("expected private permissions, got %o", info.Mode().Perm())
	}

	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if strings.Contains(string(raw), "secret") {
		t.Fatal("plaintext key leaked into config file")
	}
}

func TestLoadOrCreateNormalizesMissingCoreFields(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	p := filepath.Join(tmpHome, ".spettro", "config.json")
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	raw := []byte(`{"token_budget": 99, "permission": "invalid"}`)
	if err := os.WriteFile(p, raw, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := config.LoadOrCreate()
	if err != nil {
		t.Fatalf("load/create: %v", err)
	}
	// Active provider/model deliberately stay empty until real credentials
	// exist; a hardcoded default would surface a model the user cannot run.
	if cfg.ActiveProvider != "" {
		t.Fatalf("expected empty provider, got %q", cfg.ActiveProvider)
	}
	if cfg.ActiveModel != "" {
		t.Fatalf("expected empty model, got %q", cfg.ActiveModel)
	}
	if cfg.Permission != config.Default().Permission {
		t.Fatalf("expected default permission %q, got %q", config.Default().Permission, cfg.Permission)
	}

	updatedRaw, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read normalized config: %v", err)
	}
	var persisted map[string]any
	if err := json.Unmarshal(updatedRaw, &persisted); err != nil {
		t.Fatalf("decode normalized config: %v", err)
	}
	if prov, _ := persisted["active_provider"].(string); prov != "" {
		t.Fatalf("expected empty active_provider in persisted config: %s", string(updatedRaw))
	}
	if mod, _ := persisted["active_model"].(string); mod != "" {
		t.Fatalf("expected empty active_model in persisted config: %s", string(updatedRaw))
	}
	if got, _ := persisted["permission"].(string); got != string(config.Default().Permission) {
		t.Fatalf("expected normalized permission %q, got %q", config.Default().Permission, got)
	}
}
