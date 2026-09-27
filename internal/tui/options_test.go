package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

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

// Found in the qa-r9 first-frame captures: Bubble Tea renders the model once
// before it delivers the first WindowSizeMsg, and on a busy machine that
// render reached the screen as a blank page reading "loading…". With the
// size passed to New the very first View is the real frame, and the
// WindowSizeMsg that follows with the same size adds nothing.
func TestInitialSizeMakesTheFirstViewReal(t *testing.T) {
	cwd := t.TempDir()
	store := &storage.Store{ProjectDir: filepath.Join(cwd, ".spettro"), GlobalDir: t.TempDir()}

	m := New(cwd, config.Default(), store, provider.NewManager(), nil)
	if plain := ansi.Strip(m.View().Content); !strings.Contains(plain, "loading…") {
		t.Fatalf("without a size the first view should be the placeholder:\n%s", plain)
	}

	m = New(cwd, config.Default(), store, provider.NewManager(), nil, WithInitialSize(80, 24))
	frame := m.View().Content
	assertFrameFits(t, "first frame", frame, 80, 24)
	plain := ansi.Strip(frame)
	if strings.Contains(plain, "loading…") || !strings.Contains(plain, "╭") {
		t.Fatalf("the first view is not the real frame:\n%s", plain)
	}
	before := len(m.messages)
	next, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	if got := len(next.(Model).messages); got != before {
		t.Fatalf("the same size again added %d messages", got-before)
	}
}
