package tui_test

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"spettro/internal/tui"
)

func TestIsInstantCommand_Classification(t *testing.T) {
	cases := []struct {
		input string
		want  bool
	}{
		// Pure config / display / dialog commands → instant.
		{"/help", true},
		{"/permission", true},
		{"/permission yolo", true},
		{"/permissions", true},
		{"/permissions debug on", true},
		{"/budget", true},
		{"/budget 4000", true},
		{"/connect", true},
		{"/models", true},
		{"/models openai:gpt-4", true},
		{"/skills", true},
		{"/skill", true},
		{"/skill list", true},
		{"/hooks", true},
		{"/tasks", true},
		{"/tasks add finish docs", true},
		{"/mcp", true},
		{"/mcp list", true},
		{"/mode", true},
		{"/next", true},
		{"/remote", true},
		{"/remote stop", true},
		{"/exit", true},
		{"/quit", true},
		// /plan and /compact have mixed sub-commands.
		{"/plan", true},
		{"/plan refactor module", false},
		{"/compact", false},
		{"/compact focus on tests", false},
		{"/compact auto", true},
		{"/compact auto on", true},
		{"/compact policy", true},
		// Commands that drive an LLM run or destroy state → not instant.
		{"/init", false},
		{"/clear", false},
		{"/resume", false},
		{"/approve", false},
		// Non-slash inputs (prompts) are never "instant" commands.
		{"hello world", false},
		{"", false},
		// Case-insensitive prefix matching for safety.
		{"/HELP", true},
		{"  /help  ", true},
	}
	for _, tc := range cases {
		if got := tui.IsInstantCommandForTesting(tc.input); got != tc.want {
			t.Errorf("isInstantCommand(%q) = %v, want %v", tc.input, got, tc.want)
		}
	}
}

func TestUpdateMain_InstantCommandRunsWhileThinking(t *testing.T) {
	m := tui.NewModelForTesting()
	m.SetThinkingForTesting(true)
	m.SetTextareaValueForTesting("/help")

	gotModel, _ := m.UpdateMainForTesting(tea.KeyPressMsg{Code: tea.KeyEnter})
	got := gotModel.(tui.Model)

	msgs := got.MessagesForTesting()
	if len(msgs) == 0 || !strings.Contains(msgs[len(msgs)-1].Content, "commands:") {
		t.Fatalf("expected /help to execute even while thinking; got messages: %+v", msgs)
	}
	if got.BannerForTesting() == "commands cannot be queued while an agent is running" {
		t.Fatalf("expected no blocking banner for instant command, got %q", got.BannerForTesting())
	}
	if strings.TrimSpace(got.TextareaValueForTesting()) != "" {
		t.Fatalf("expected textarea to reset after instant command, got %q", got.TextareaValueForTesting())
	}
}

func TestUpdateMain_InstantCommandSkipsInputHistory(t *testing.T) {
	m := tui.NewModelForTesting()
	m.SetTextareaValueForTesting("/permission yolo")

	gotModel, _ := m.UpdateMainForTesting(tea.KeyPressMsg{Code: tea.KeyEnter})
	got := gotModel.(tui.Model)

	for _, entry := range got.InputHistoryForTesting() {
		if strings.HasPrefix(entry, "/") {
			t.Fatalf("expected instant slash commands to be excluded from input history; got %q", entry)
		}
	}
}

func TestUpdateMain_InstantCommandDoesNotPushIntoSharedHistory(t *testing.T) {
	m := tui.NewModelForTesting()
	m.SetInputHistoryForTesting([]string{"first prompt", "second prompt"})
	m.SetTextareaValueForTesting("/help")

	gotModel, _ := m.UpdateMainForTesting(tea.KeyPressMsg{Code: tea.KeyEnter})
	got := gotModel.(tui.Model)

	history := got.InputHistoryForTesting()
	want := []string{"first prompt", "second prompt"}
	if len(history) != len(want) {
		t.Fatalf("expected history unchanged, got %v", history)
	}
	for i := range want {
		if history[i] != want[i] {
			t.Fatalf("expected history[%d] = %q, got %q", i, want[i], history[i])
		}
	}
}

func TestUpdateMain_NonInstantCommandBlockedWhileThinking(t *testing.T) {
	m := tui.NewModelForTesting()
	m.SetThinkingForTesting(true)
	m.SetTextareaValueForTesting("/clear")

	beforeMsgCount := len(m.MessagesForTesting())

	gotModel, _ := m.UpdateMainForTesting(tea.KeyPressMsg{Code: tea.KeyEnter})
	got := gotModel.(tui.Model)

	if got.BannerForTesting() != "commands cannot be queued while an agent is running" {
		t.Fatalf("expected blocking banner for non-instant command while thinking, got %q", got.BannerForTesting())
	}
	if len(got.MessagesForTesting()) != beforeMsgCount {
		t.Fatalf("expected no new messages from blocked command, got %+v", got.MessagesForTesting())
	}
}

func TestUpdateMain_PromptsStillEnterInputHistory(t *testing.T) {
	m := tui.NewModelForTesting()
	m.SetTextareaValueForTesting("explain this code")

	gotModel, _ := m.UpdateMainForTesting(tea.KeyPressMsg{Code: tea.KeyEnter})
	got := gotModel.(tui.Model)

	history := got.InputHistoryForTesting()
	if len(history) == 0 || history[len(history)-1] != "explain this code" {
		t.Fatalf("expected prompt to be pushed into input history, got %v", history)
	}
}

func TestUpdateMain_AutocompleteInstantCommandRunsWhileThinking(t *testing.T) {
	m := tui.NewModelForTesting()
	m.SetThinkingForTesting(true)
	m.SetTextareaValueForTesting("/help")
	m.SetCommandItemsForTesting([]string{"/help"})

	gotModel, _ := m.UpdateMainForTesting(tea.KeyPressMsg{Code: tea.KeyEnter})
	got := gotModel.(tui.Model)

	msgs := got.MessagesForTesting()
	if len(msgs) == 0 || !strings.Contains(msgs[len(msgs)-1].Content, "commands:") {
		t.Fatalf("expected autocomplete-driven instant command to run while thinking; got %+v", msgs)
	}
	if got.BannerForTesting() == "commands cannot be queued while an agent is running" {
		t.Fatalf("expected no blocking banner for instant command via autocomplete")
	}
}

func TestUpdateMain_AutocompleteNonInstantCommandBlockedWhileThinking(t *testing.T) {
	m := tui.NewModelForTesting()
	m.SetThinkingForTesting(true)
	m.SetTextareaValueForTesting("/clear")
	m.SetCommandItemsForTesting([]string{"/clear"})

	beforeMsgCount := len(m.MessagesForTesting())

	gotModel, _ := m.UpdateMainForTesting(tea.KeyPressMsg{Code: tea.KeyEnter})
	got := gotModel.(tui.Model)

	if got.BannerForTesting() != "commands cannot be queued while an agent is running" {
		t.Fatalf("expected blocking banner for autocomplete-driven non-instant command while thinking, got %q", got.BannerForTesting())
	}
	if len(got.MessagesForTesting()) != beforeMsgCount {
		t.Fatalf("expected no new messages when autocomplete-driven command is blocked, got %+v", got.MessagesForTesting())
	}
}
