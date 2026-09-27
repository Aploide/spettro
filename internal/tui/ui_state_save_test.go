package tui

import (
	"testing"

	"spettro/internal/config"
)

// The mode and panel toggle are saved by a background command, never on
// the Update goroutine, and two saves that run out of order still leave the
// latest state on disk.
func TestUIStateSaveKeepsTheLatestState(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // config.Update writes $HOME/.spettro/config.json
	m := footerModel(120, 40)

	m.mode, m.showSidePanel = "plan", true
	m.persistUIState()
	first := m.uiStateSaveCmd()
	if first == nil {
		t.Fatal("no background save was scheduled")
	}
	if m.uiStateSaveCmd() != nil {
		t.Fatal("one request scheduled two saves")
	}
	m.mode, m.showSidePanel = "ask", false
	m.persistUIState()
	second := m.uiStateSaveCmd()

	second()
	first() // runs last, but must still write the latest state
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LastAgentID != "ask" || cfg.ShowSidePanel {
		t.Fatalf("saved mode %q panel %v, want the latest (ask, false)", cfg.LastAgentID, cfg.ShowSidePanel)
	}
	if m.cfg.LastAgentID != "ask" {
		t.Fatalf("the in-memory config was not updated: %q", m.cfg.LastAgentID)
	}
}
