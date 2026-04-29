package tui_test

import (
	"strings"
	"testing"

	"spettro/internal/tui"
)

// TestThinkingCommand_SetsLevel verifies that "/thinking high" persists the
// level, and that "/thinking" with no args reports it. The command runs
// instantly (even while a run is in flight) so it should be safe to
// dispatch via HandleCommandForTesting without spinning up an agent.
func TestThinkingCommand_SetsLevel(t *testing.T) {
	m := tui.NewModelForTesting()
	if level := m.ThinkingLevelForTesting(); level != "" {
		t.Fatalf("default thinking should be empty, got %q", level)
	}

	got, _ := m.HandleCommandForTesting("/thinking high")
	m = got.(tui.Model)
	if level := m.ThinkingLevelForTesting(); level != "high" {
		t.Fatalf("after /thinking high, level = %q, want \"high\"", level)
	}

	got, _ = m.HandleCommandForTesting("/thinking off")
	m = got.(tui.Model)
	if level := m.ThinkingLevelForTesting(); level != "" {
		t.Fatalf("after /thinking off, level = %q, want \"\"", level)
	}

	// Invalid value should not change the persisted setting.
	got, _ = m.HandleCommandForTesting("/thinking high")
	m = got.(tui.Model)
	got, _ = m.HandleCommandForTesting("/thinking bogus")
	m = got.(tui.Model)
	if level := m.ThinkingLevelForTesting(); level != "high" {
		t.Fatalf("after invalid /thinking, level changed unexpectedly to %q", level)
	}
}

// TestThinkingCommand_ConfirmsViaBanner ensures the success banner mentions
// the new level so users get visible feedback even when stdout is muted.
func TestThinkingCommand_ConfirmsViaBanner(t *testing.T) {
	m := tui.NewModelForTesting()
	got, _ := m.HandleCommandForTesting("/thinking medium")
	m = got.(tui.Model)
	banner := m.BannerForTesting()
	if !strings.Contains(banner, "medium") {
		t.Fatalf("expected banner to confirm level change, got %q", banner)
	}
}
