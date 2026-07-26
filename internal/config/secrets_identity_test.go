package config

import (
	"os"
	"os/user"
	"path/filepath"
	"testing"
)

// TestSecretsIdentityUsesHomeDir pins the property iOS depends on: the home
// directory secrets are bound to is whatever $HOME says, with no passwd lookup
// involved. On iOS $HOME is the app container, so this is what puts
// ~/.spettro inside the sandbox.
func TestSecretsIdentityUsesHomeDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	gotUser, gotHome, err := secretsIdentity()
	if err != nil {
		t.Fatalf("secretsIdentity: %v", err)
	}
	if gotHome != home {
		t.Errorf("home = %q, want %q", gotHome, home)
	}
	if gotUser == "" {
		t.Error("username is empty; legacySecrets would hash a blank name")
	}

	p, err := keysPath()
	if err != nil {
		t.Fatalf("keysPath: %v", err)
	}
	if want := filepath.Join(home, ".spettro", "keys.enc"); p != want {
		t.Errorf("keysPath = %q, want %q", p, want)
	}
}

// TestSecretsIdentityFallsBackWhenNoPasswdEntry covers the iOS Simulator case
// directly. There the app runs as the host Mac's uid, which the simulator's
// /etc/passwd does not contain, so user.Current fails. Before the fallback
// existed that error aborted every config read — no keys.enc needed to be
// present for the engine to refuse to start.
//
// The real call cannot be made to fail on a healthy desktop, so this asserts
// the fallback's own contract: a stable synthetic name and the real home.
func TestSecretsIdentityFallsBackWhenNoPasswdEntry(t *testing.T) {
	if fallbackUsername == "" {
		t.Fatal("fallbackUsername is empty")
	}
	if fallbackUsername != "spettro" {
		t.Errorf("fallbackUsername = %q; changing it silently orphans keys.enc "+
			"files that legacySecrets would otherwise migrate", fallbackUsername)
	}

	// Whatever identity we end up with, a fresh container must be able to
	// create a master key and round-trip a secret through it. This is the
	// path a first launch on a phone takes.
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SPETTRO_MASTER_KEY", "")

	if err := SaveAPIKey("anthropic", "sk-test-value"); err != nil {
		t.Fatalf("SaveAPIKey in a fresh container: %v", err)
	}
	keys, err := LoadAPIKeys()
	if err != nil {
		t.Fatalf("LoadAPIKeys: %v", err)
	}
	if keys["anthropic"] != "sk-test-value" {
		t.Errorf("round-trip returned %q", keys["anthropic"])
	}
	if _, err := os.Stat(filepath.Join(home, ".spettro", "master.key")); err != nil {
		t.Errorf("master.key was not created: %v", err)
	}
}

// TestSecretsIdentityPrefersRealUsername makes sure the fallback stays a
// fallback: on a host with a working passwd entry the real name is used, so
// legacy keys.enc files written by older desktop versions still migrate.
func TestSecretsIdentityPrefersRealUsername(t *testing.T) {
	current, err := user.Current()
	if err != nil {
		t.Skipf("no passwd entry on this host: %v", err)
	}
	t.Setenv("HOME", t.TempDir())

	gotUser, _, err := secretsIdentity()
	if err != nil {
		t.Fatalf("secretsIdentity: %v", err)
	}
	if gotUser != current.Username {
		t.Errorf("username = %q, want the real %q", gotUser, current.Username)
	}
}
