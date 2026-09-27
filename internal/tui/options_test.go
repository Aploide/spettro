package tui

import (
	"os"
	"path/filepath"
	"testing"

	"spettro/internal/config"
	"spettro/internal/provider"
	"spettro/internal/storage"
)

// writeBrokenManifest puts a spettro.agents.toml that does not parse in cwd,
// so a Model that loaded it would fall back to the empty manifest.
func writeBrokenManifest(t *testing.T, cwd string) {
	t.Helper()
	path := filepath.Join(cwd, config.AgentManifestFilename)
	if err := os.WriteFile(path, []byte("version = [not toml"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// WithManifest is the manifest New uses: it does not read the project's
// spettro.agents.toml again (the one on disk here would not even parse).
func TestNewWithManifestDoesNotLoadTheManifest(t *testing.T) {
	cwd := t.TempDir()
	writeBrokenManifest(t, cwd)
	store := &storage.Store{ProjectDir: filepath.Join(cwd, ".spettro"), GlobalDir: t.TempDir()}

	manifest := config.DefaultAgentManifest()
	manifest.DefaultAgent = "coding"
	m := New(cwd, config.Default(), store, provider.NewManager(), nil, WithManifest(manifest))

	if m.mode != "coding" {
		t.Fatalf("mode = %q, want the passed manifest's default agent %q", m.mode, "coding")
	}
	if len(m.manifest.Agents) != len(manifest.Agents) {
		t.Fatalf("model has %d agents, want the passed manifest's %d", len(m.manifest.Agents), len(manifest.Agents))
	}
}

// Without WithManifest, New still loads the project's manifest itself.
func TestNewWithoutManifestLoadsIt(t *testing.T) {
	cwd := t.TempDir()
	writeBrokenManifest(t, cwd)
	store := &storage.Store{ProjectDir: filepath.Join(cwd, ".spettro"), GlobalDir: t.TempDir()}

	m := New(cwd, config.Default(), store, provider.NewManager(), nil)

	// The broken file loads as the empty manifest: no agents at all.
	if len(m.manifest.Agents) != 0 {
		t.Fatalf("model has %d agents, want the empty manifest a broken file loads as", len(m.manifest.Agents))
	}
}
