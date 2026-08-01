package tui

import (
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

func longOutput(n int) string {
	var sb strings.Builder
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&sb, "line%d\n", i)
	}
	return strings.TrimRight(sb.String(), "\n")
}

// TestTranscriptExpansionKeys verifies ctrl+o as a plain show/hide toggle and
// ctrl+g as the full-output toggle (which implies details visible; hiding
// details clears it).
func TestTranscriptExpansionKeys(t *testing.T) {
	m := NewModelForTesting()
	ctrlO := tea.KeyPressMsg{Code: 'o', Mod: tea.ModCtrl}
	ctrlG := tea.KeyPressMsg{Code: 'g', Mod: tea.ModCtrl}

	out, _ := m.updateMain(ctrlO)
	m = out.(Model)
	if !m.showTools || m.showFullOutput {
		t.Fatalf("after ctrl+o: showTools=%v full=%v, want true/false", m.showTools, m.showFullOutput)
	}
	out, _ = m.updateMain(ctrlO)
	m = out.(Model)
	if m.showTools {
		t.Fatalf("second ctrl+o should hide details, showTools=%v", m.showTools)
	}

	// ctrl+g from collapsed state turns on both details and full output.
	out, _ = m.updateMain(ctrlG)
	m = out.(Model)
	if !m.showTools || !m.showFullOutput {
		t.Fatalf("after ctrl+g: showTools=%v full=%v, want true/true", m.showTools, m.showFullOutput)
	}
	out, _ = m.updateMain(ctrlG)
	m = out.(Model)
	if !m.showTools || m.showFullOutput {
		t.Fatalf("second ctrl+g should keep details but trim, got showTools=%v full=%v", m.showTools, m.showFullOutput)
	}

	// Hiding details clears the full-output flag.
	out, _ = m.updateMain(ctrlG)
	m = out.(Model)
	out, _ = m.updateMain(ctrlO)
	m = out.(Model)
	if m.showTools || m.showFullOutput {
		t.Fatalf("ctrl+o hide should clear full, got showTools=%v full=%v", m.showTools, m.showFullOutput)
	}
}

// TestTranscriptFullOutputExpansion verifies that a long tool output is
// trimmed in the default expanded state and shown in full once the full-output
// state is active.
func TestTranscriptFullOutputExpansion(t *testing.T) {
	tools := []ToolItem{{Name: "shell", Status: "done", Args: `{"command":"ls"}`, Output: longOutput(30)}}

	collapsed := renderToolGroups(tools, false, false, colorHeaderBg)
	if strings.Contains(collapsed, "line1\n") || strings.Contains(collapsed, "line30") {
		t.Fatalf("collapsed transcript should not show tool output:\n%s", collapsed)
	}

	trimmed := renderToolGroups(tools, true, false, colorHeaderBg)
	if !strings.Contains(trimmed, "line1") {
		t.Fatalf("expanded transcript should show output start:\n%s", trimmed)
	}
	if strings.Contains(trimmed, "line30") {
		t.Fatalf("expanded transcript should trim long output:\n%s", trimmed)
	}
	if !strings.Contains(trimmed, "more lines") {
		t.Fatalf("trimmed output should mention hidden lines:\n%s", trimmed)
	}

	full := renderToolGroups(tools, true, true, colorHeaderBg)
	if !strings.Contains(full, "line30") {
		t.Fatalf("full state should show the entire output:\n%s", full)
	}
	if strings.Contains(full, "more lines") {
		t.Fatalf("full state should not truncate:\n%s", full)
	}
}

// TestTranscriptGroupFullOutputExpansion covers the grouped-tools path, which
// uses a tighter per-tool cap when trimmed.
func TestTranscriptGroupFullOutputExpansion(t *testing.T) {
	tools := []ToolItem{
		{Name: "shell", Status: "done", Args: `{"command":"a"}`, Output: longOutput(12)},
		{Name: "shell", Status: "done", Args: `{"command":"b"}`, Output: longOutput(12)},
	}

	trimmed := renderToolGroups(tools, true, false, colorHeaderBg)
	if strings.Contains(trimmed, "line12") {
		t.Fatalf("grouped trimmed output should cap at 8 lines:\n%s", trimmed)
	}

	full := renderToolGroups(tools, true, true, colorHeaderBg)
	if strings.Count(full, "line12") != 2 {
		t.Fatalf("grouped full state should show both outputs in full:\n%s", full)
	}
}
