package tui_test

import (
	"strings"
	"testing"
	"time"

	"charm.land/lipgloss/v2"

	"spettro/internal/session"
	"spettro/internal/tui"
)

func TestSidePanelWidth_StaysVisibleWhenEnabledWithoutActivity(t *testing.T) {
	m := tui.NewModelForTesting()
	m.SetDimensionsForTesting(140, 40)
	m.SetSidePanelVisibleForTesting(true)

	if got := m.SidePanelWidthForTesting(); got == 0 {
		t.Fatal("expected activity panel to stay visible when enabled")
	}
}

func TestViewSidePanel_ShowsExpandedContextWithoutToolCallSyntax(t *testing.T) {
	m := tui.NewModelForTesting()
	m.SetDimensionsForTesting(140, 40)
	m.SetSidePanelVisibleForTesting(true)
	m.SetShowToolsForTesting(true)
	body := tui.SanitizeToolOutputForTesting("matched internal/tui/model.go\nTOOL_CALL {\"tool\":\"grep\"}", 24)
	m.AddActivityForTesting(
		"tool",
		"grep",
		"coding",
		`Grep "approval"`,
		`Searches file contents for "approval".`,
		"Searches file contents for \"approval\".\n\n"+body,
		"done",
	)

	view := m.ViewSidePanelForTesting(m.SidePanelWidthForTesting())
	if !strings.Contains(view, `Searches file contents for "approval".`) {
		t.Fatalf("expected expanded tool context in side panel, got: %s", view)
	}
	if strings.Contains(view, "TOOL_CALL") {
		t.Fatalf("expected TOOL_CALL syntax to be hidden, got: %s", view)
	}
}

func TestViewSidePanel_DetailsAreaIsScrollableAndHeightBounded(t *testing.T) {
	m := tui.NewModelForTesting()
	m.SetDimensionsForTesting(140, 36)
	m.SetSidePanelVisibleForTesting(true)
	m.SetShowToolsForTesting(true)
	m.AddActivityForTesting(
		"tool",
		"web-search",
		"ask",
		`Searched web for "daft punk songs"`,
		`Searches web`,
		strings.Repeat("result line that is intentionally long\n", 80),
		"done",
	)
	width := m.SidePanelWidthForTesting()
	initial := m.ViewSidePanelForTesting(width)
	if got := lipgloss.Height(initial); got > 36 {
		t.Fatalf("expected side panel to stay bounded, got %d lines", got)
	}
	if !strings.Contains(initial, "scroll:") {
		t.Fatalf("expected details scroll footer, got: %s", initial)
	}

	m.SetSideDetailScrollForTesting(12)
	scrolled := m.ViewSidePanelForTesting(width)
	if initial == scrolled {
		t.Fatalf("expected details view to change after scrolling")
	}
}

func TestViewSidePanel_IsHeightBounded(t *testing.T) {
	m := tui.NewModelForTesting()
	m.SetDimensionsForTesting(140, 32)
	m.SetSidePanelVisibleForTesting(true)
	m.SetShowToolsForTesting(true)
	m.AddActivityForTesting(
		"message",
		"assistant",
		"coding",
		"Assistant response",
		"Long summary",
		strings.Repeat("line of context\n", 80),
		"done",
	)

	view := m.ViewSidePanelForTesting(m.SidePanelWidthForTesting())
	if got := lipgloss.Height(view); got > 32 {
		t.Fatalf("expected side panel height to stay within terminal, got %d lines", got)
	}
}

func TestViewSidePanel_ShowsGitBranchAndChanges(t *testing.T) {
	m := tui.NewModelForTesting()
	m.SetDimensionsForTesting(140, 40)
	m.SetSidePanelVisibleForTesting(true)
	m.SetGitBranchForTesting("feature/activity-panel")
	m.AddModifiedFileForTesting("internal/tui/model_view.go", 32, 10, false, true, true)
	m.AddModifiedFileForTesting("tests/tui/activity_panel_test.go", 18, 0, false, true, false)
	m.AddModifiedFileForTesting("tmp/new-file.txt", 0, 0, true, false, false)
	m.AddActivityForTesting(
		"tool",
		"grep",
		"coding",
		`Grep "panel"`,
		`Search panel references`,
		"Search panel references",
		"done",
	)

	view := m.ViewSidePanelForTesting(m.SidePanelWidthForTesting())
	for _, want := range []string{
		"⎇",
		"feature/activity-panel",
		"spettro-tui-tests",
		"+50",
		"-10",
	} {
		if !strings.Contains(view, want) {
			t.Fatalf("expected %q in side panel, got: %s", want, view)
		}
	}
	if strings.Contains(view, "internal/tui/model_view.go") {
		t.Fatalf("expected git file list to stay hidden, got: %s", view)
	}
}

func TestViewSidePanel_HidesTextActivities(t *testing.T) {
	m := tui.NewModelForTesting()
	m.SetDimensionsForTesting(140, 40)
	m.SetSidePanelVisibleForTesting(true)
	m.SetShowToolsForTesting(true)
	m.AddActivityForTesting(
		"message",
		"assistant",
		"coding",
		"Assistant response",
		"This should be hidden",
		"This should not appear in activity.",
		"done",
	)
	m.AddActivityForTesting(
		"tool",
		"shell-exec",
		"coding",
		"$ go test ./...",
		"Runs tests",
		"Runs tests",
		"done",
	)

	view := m.ViewSidePanelForTesting(m.SidePanelWidthForTesting())
	if strings.Contains(view, "Assistant response") {
		t.Fatalf("expected text activity to be hidden, got: %s", view)
	}
	if !strings.Contains(view, "$ go test ./...") {
		t.Fatalf("expected tool activity to remain visible, got: %s", view)
	}
}

func TestViewSidePanel_ShowsCommandActivities(t *testing.T) {
	m := tui.NewModelForTesting()
	m.SetDimensionsForTesting(140, 40)
	m.SetSidePanelVisibleForTesting(true)
	m.SetShowToolsForTesting(true)
	m.AddActivityForTesting(
		"command",
		"/permission ask-first",
		"tui",
		"/permission ask-first",
		"command",
		"/permission ask-first",
		"done",
	)

	view := m.ViewSidePanelForTesting(m.SidePanelWidthForTesting())
	if !strings.Contains(view, "/permission ask-first") {
		t.Fatalf("expected command activity in side panel, got: %s", view)
	}
}

func TestViewSidePanel_GroupsEntriesUnderAgentHeaders(t *testing.T) {
	m := tui.NewModelForTesting()
	m.SetDimensionsForTesting(140, 40)
	m.SetSidePanelVisibleForTesting(true)
	m.SetShowToolsForTesting(true)
	m.AddActivityForTesting(
		"tool",
		"shell-exec",
		"review",
		"$ go test ./...",
		"Runs tests",
		"Runs tests",
		"done",
	)
	m.AddActivityForTesting(
		"tool",
		"grep",
		"coding",
		`Grep "TODO"`,
		"Search",
		"Search",
		"done",
	)

	view := m.ViewSidePanelForTesting(m.SidePanelWidthForTesting())
	for _, want := range []string{"review", "coding", "go test ./...", "Grep"} {
		if !strings.Contains(view, want) {
			t.Fatalf("expected hierarchical activity row %q, got: %s", want, view)
		}
	}
	if !strings.Contains(view, "└") {
		t.Fatalf("expected indented hierarchy marker in side panel, got: %s", view)
	}
}

func TestRebuildActivitiesFromEventsForTesting_RestoresToolAndCommand(t *testing.T) {
	m := tui.NewModelForTesting()
	m.SetDimensionsForTesting(140, 40)
	m.SetSidePanelVisibleForTesting(true)
	m.SetShowToolsForTesting(true)
	events := []session.AgentEvent{
		{
			At:      time.Now().Add(-time.Minute),
			Kind:    "command",
			AgentID: "tui",
			Task:    "/models",
			Status:  "done",
			Summary: "/models",
		},
		{
			At:         time.Now(),
			Kind:       "tool",
			AgentID:    "coding",
			Status:     "error",
			ToolName:   "shell-exec",
			ToolArgs:   `{"command":"go test ./..."}`,
			ToolOutput: "error: test failed",
		},
	}
	m.RebuildActivitiesFromEventsForTesting(events)
	view := m.ViewSidePanelForTesting(m.SidePanelWidthForTesting())
	for _, want := range []string{"/models", "$ go test ./...", "error: test failed"} {
		if !strings.Contains(view, want) {
			t.Fatalf("expected %q in resumed activity view, got: %s", want, view)
		}
	}
}
