package tui_test

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"spettro/internal/agent"
	"spettro/internal/config"
	"spettro/internal/provider"
	"spettro/internal/storage"
	"spettro/internal/tui"
)

func TestUpdateMain_EnterWhileThinkingOffersSteerOrQueue(t *testing.T) {
	m := tui.NewModelForTesting()
	m.SetThinkingForTesting(true)
	m.SetTextareaValueForTesting("please review the latest change")

	gotModel, _ := m.UpdateMainForTesting(tea.KeyPressMsg{Code: tea.KeyEnter})
	got := gotModel.(tui.Model)

	if !got.ShowSteerChoiceForTesting() {
		t.Fatal("expected the steer/queue picker to open while a run is active")
	}
	if got.SteerPendingForTesting() != "please review the latest change" {
		t.Fatalf("unexpected pending steer input: %q", got.SteerPendingForTesting())
	}
	if strings.TrimSpace(got.TextareaValueForTesting()) != "" {
		t.Fatalf("expected textarea to reset while picker is open, got %q", got.TextareaValueForTesting())
	}

	// Choosing "Queue for after the run" (option 2) falls back to the old
	// queueing behavior.
	gotModel, _ = got.UpdateMainForTesting(tea.KeyPressMsg{Code: tea.KeyDown})
	got = gotModel.(tui.Model)
	gotModel, _ = got.UpdateMainForTesting(tea.KeyPressMsg{Code: tea.KeyEnter})
	got = gotModel.(tui.Model)

	if got.ShowSteerChoiceForTesting() {
		t.Fatal("picker should close after a choice")
	}
	if got.PendingPromptCountForTesting() != 1 {
		t.Fatalf("expected one queued prompt, got %d", got.PendingPromptCountForTesting())
	}
	msgs := got.MessagesForTesting()
	if len(msgs) == 0 || !strings.Contains(msgs[len(msgs)-1].Content, "queued request") {
		t.Fatalf("expected queued request system message, got %+v", msgs)
	}
}

func TestUpdateMain_SteerNowPushesToSteeringQueue(t *testing.T) {
	m := tui.NewModelForTesting()
	m.SetThinkingForTesting(true)
	m.SetSteeringQueueForTesting(agent.NewSteeringQueue())
	m.SetTextareaValueForTesting("use table-driven tests instead")

	gotModel, _ := m.UpdateMainForTesting(tea.KeyPressMsg{Code: tea.KeyEnter})
	got := gotModel.(tui.Model)
	// Option 1 ("Steer now") is preselected.
	gotModel, _ = got.UpdateMainForTesting(tea.KeyPressMsg{Code: tea.KeyEnter})
	got = gotModel.(tui.Model)

	if got.ShowSteerChoiceForTesting() {
		t.Fatal("picker should close after steering")
	}
	q := got.SteeringQueueForTesting()
	if q == nil || q.Len() != 1 {
		t.Fatalf("expected one steering message in the queue, got %v", q.Len())
	}
	if msgs := q.Drain(); msgs[0] != "use table-driven tests instead" {
		t.Fatalf("unexpected steering message: %q", msgs[0])
	}
	if got.PendingPromptCountForTesting() != 0 {
		t.Fatal("steer-now must not also queue the prompt")
	}
	msgs := got.MessagesForTesting()
	if len(msgs) < 2 || msgs[len(msgs)-2].Role != tui.RoleUser ||
		!strings.Contains(msgs[len(msgs)-1].Content, "delivered at the agent's next step") {
		t.Fatalf("expected user message + delivery notice in transcript, got %+v", msgs)
	}
}

func TestUpdateMain_SteerChoiceEscRestoresInput(t *testing.T) {
	m := tui.NewModelForTesting()
	m.SetThinkingForTesting(true)
	m.SetTextareaValueForTesting("half-typed thought")

	gotModel, _ := m.UpdateMainForTesting(tea.KeyPressMsg{Code: tea.KeyEnter})
	got := gotModel.(tui.Model)
	gotModel, _ = got.UpdateMainForTesting(tea.KeyPressMsg{Code: tea.KeyEsc})
	got = gotModel.(tui.Model)

	if got.ShowSteerChoiceForTesting() {
		t.Fatal("esc should close the picker")
	}
	if got.TextareaValueForTesting() != "half-typed thought" {
		t.Fatalf("esc should restore the typed input, got %q", got.TextareaValueForTesting())
	}
	if got.PendingPromptCountForTesting() != 0 {
		t.Fatal("esc must not queue anything")
	}
}

func TestUpdateMain_EscWhileThinkingPreservesProgressAndAsksInstead(t *testing.T) {
	m := tui.NewModelForTesting()
	m.SetThinkingForTesting(true)
	m.SetActiveAgentForTesting("coding")
	m.SetLiveToolsForTesting([]tui.ToolItem{{
		Name:   "file-read",
		Status: "success",
		Args:   `{"path":"internal/tui/model.go"}`,
	}}, &tui.ToolItem{
		Name:   "grep",
		Status: "running",
		Args:   `{"pattern":"approval"}`,
	})

	gotModel, _ := m.UpdateMainForTesting(tea.KeyPressMsg{Code: tea.KeyEsc})
	got := gotModel.(tui.Model)

	if !got.AwaitingInsteadForTesting() {
		t.Fatal("expected esc interrupt to wait for replacement instruction")
	}
	if got.BannerForTesting() != "what should I do instead?" {
		t.Fatalf("unexpected banner: %q", got.BannerForTesting())
	}
	msgs := got.MessagesForTesting()
	if len(msgs) == 0 {
		t.Fatal("expected interrupt summary message")
	}
	last := msgs[len(msgs)-1].Content
	if !strings.Contains(last, "Progress kept:") || !strings.Contains(last, "Read internal/tui/model.go") {
		t.Fatalf("expected preserved progress summary, got %q", last)
	}
}

func TestUpdateShellApproval_DenyInterruptsAndAsksInstead(t *testing.T) {
	m := tui.NewModelForTesting()
	m.SetThinkingForTesting(true)
	m.SetPendingShellApprovalForTesting(2)

	gotModel, _ := m.UpdateShellApprovalForTesting(tea.KeyPressMsg{Code: tea.KeyEnter})
	got := gotModel.(tui.Model)

	if !got.AwaitingInsteadForTesting() {
		t.Fatal("expected denial to enter replacement-instruction mode")
	}
	if got.HasPendingShellApprovalForTesting() {
		t.Fatal("expected pending shell approval to resolve")
	}
	if got.BannerForTesting() != "what should I do instead?" {
		t.Fatalf("unexpected banner: %q", got.BannerForTesting())
	}
}

func TestRenderMessages_KeepsCommentsAndToolEventsInOrder(t *testing.T) {
	m := tui.NewModelForTesting()
	m.SetDimensionsForTesting(120, 40)
	m.AddMessageForTesting(tui.ChatMessage{
		Role:    tui.RoleAssistant,
		Kind:    "comment",
		Content: "Let me read the key files first.",
		At:      time.Now(),
	})
	m.AddMessageForTesting(tui.ChatMessage{
		Role: tui.RoleAssistant,
		Kind: "tool-stream",
		Tools: []tui.ToolItem{{
			Name:   "file-read",
			Status: "success",
			Args:   `{"path":"internal/tui/model.go"}`,
		}},
		At: time.Now(),
	})
	m.AddMessageForTesting(tui.ChatMessage{
		Role:    tui.RoleAssistant,
		Kind:    "comment",
		Content: "Now let me read the rendering section.",
		At:      time.Now(),
	})
	m.AddMessageForTesting(tui.ChatMessage{
		Role: tui.RoleAssistant,
		Kind: "tool-stream",
		Tools: []tui.ToolItem{{
			Name:   "file-read",
			Status: "running",
			Args:   `{"path":"internal/tui/model_state.go"}`,
		}},
		At: time.Now(),
	})

	// lipgloss v2 always emits ANSI styling (downsampling happens in the
	// bubbletea renderer, not at Render time), so strip it before matching
	// plain substrings.
	rendered := ansi.Strip(m.RenderMessagesForTesting())
	wantOrder := []string{
		"Let me read the key files first.",
		"Read internal/tui/model.go",
		"Now let me read the rendering section.",
		"Reading internal/tui/model_state.go",
	}
	last := -1
	for _, want := range wantOrder {
		idx := strings.Index(rendered, want)
		if idx == -1 {
			t.Fatalf("expected %q in rendered output, got:\n%s", want, rendered)
		}
		if idx <= last {
			t.Fatalf("expected %q after previous stream item, got:\n%s", want, rendered)
		}
		last = idx
	}
	if strings.Contains(rendered, "● Let me read the key files first.") {
		t.Fatalf("expected comment text without dot prefix, got:\n%s", rendered)
	}
	if !strings.Contains(rendered, "● Read internal/tui/model.go") {
		t.Fatalf("expected tool entry to keep dot prefix, got:\n%s", rendered)
	}
}

func TestNew_RestoresLastAgentAndPanelState(t *testing.T) {
	cwd := t.TempDir()
	store, err := storage.New(cwd)
	if err != nil {
		t.Fatalf("storage.New: %v", err)
	}
	pm := provider.NewManager()
	cfg := config.Default()
	cfg.LastAgentID = "coding"
	cfg.ShowSidePanel = true

	m := tui.New(cwd, cfg, store, pm, nil)
	if m.ModeForTesting() != "coding" {
		t.Fatalf("expected restored mode coding, got %s", m.ModeForTesting())
	}
	if !m.SidePanelVisibleForTesting() {
		t.Fatal("expected restored side panel state to be visible")
	}
}

func TestUpdateMain_CtrlCShowsExitHint(t *testing.T) {
	m := tui.NewModelForTesting()

	gotModel, _ := m.UpdateMainForTesting(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	got := gotModel.(tui.Model)

	if got.BannerForTesting() != "press again ctrl C to exit" {
		t.Fatalf("unexpected ctrl+c banner: %q", got.BannerForTesting())
	}
}

func TestQuitWarningMsg_ClearsExitHintAfterTimeout(t *testing.T) {
	m := tui.NewModelForTesting()

	gotModel, _ := m.UpdateForTesting(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	got := gotModel.(tui.Model)
	if got.BannerForTesting() != "press again ctrl C to exit" {
		t.Fatalf("unexpected ctrl+c banner: %q", got.BannerForTesting())
	}

	clearedModel, _ := got.TriggerQuitWarningTimeoutForTesting()
	cleared := clearedModel.(tui.Model)
	if cleared.BannerForTesting() != "" {
		t.Fatalf("expected ctrl+c hint to clear after timeout, got %q", cleared.BannerForTesting())
	}
}

func TestToolProgressMsg_CommentShowsInChatNotActivity(t *testing.T) {
	m := tui.NewModelForTesting()
	m.SetThinkingForTesting(true)

	gotModel, _ := m.UpdateForTesting(tui.ToolProgressMsgForTesting("comment", "success", `{"message":"Inspecting remaining files."}`, "Inspecting remaining files."))
	got := gotModel.(tui.Model)

	msgs := got.MessagesForTesting()
	if len(msgs) == 0 || msgs[len(msgs)-1].Kind != "comment" || !strings.Contains(msgs[len(msgs)-1].Content, "Inspecting remaining files.") {
		t.Fatalf("expected comment tool output to appear in chat, got %+v", msgs)
	}
	if got.ActivityCountForTesting() != 0 {
		t.Fatalf("expected comment tool to skip activity feed, got %d items", got.ActivityCountForTesting())
	}
}

func TestToolProgressMsg_RunningToolDoesNotInjectHardcodedComment(t *testing.T) {
	m := tui.NewModelForTesting()
	m.SetThinkingForTesting(true)

	gotModel, _ := m.UpdateForTesting(tui.ToolProgressMsgForTesting("shell-exec", "running", `{"command":"git status --porcelain"}`, ""))
	got := gotModel.(tui.Model)

	msgs := got.MessagesForTesting()
	if len(msgs) != 1 {
		t.Fatalf("expected only tool stream message, got %+v", msgs)
	}
	if msgs[0].Kind != "tool-stream" {
		t.Fatalf("expected tool-stream message, got %+v", msgs[0])
	}
	if strings.Contains(msgs[0].Content, "Okay, let me") {
		t.Fatalf("expected no hardcoded narration, got %+v", msgs[0])
	}
}

func TestToolProgressMsg_FailedCommentStaysHidden(t *testing.T) {
	m := tui.NewModelForTesting()
	m.SetThinkingForTesting(true)

	gotModel, _ := m.UpdateForTesting(tui.ToolProgressMsgForTesting("comment", "error", `{"message":"I am narrating."}`, "error: comment failed"))
	got := gotModel.(tui.Model)

	if len(got.MessagesForTesting()) != 0 {
		t.Fatalf("expected failed comment to stay hidden, got %+v", got.MessagesForTesting())
	}
	if got.ActivityCountForTesting() != 0 {
		t.Fatalf("expected failed comment to skip activity feed, got %d items", got.ActivityCountForTesting())
	}
}

func TestRenderMessages_LongCommentWrapsToPaneWidth(t *testing.T) {
	m := tui.NewModelForTesting()
	m.SetDimensionsForTesting(60, 20)
	m.AddMessageForTesting(tui.ChatMessage{
		Role:    tui.RoleAssistant,
		Kind:    "comment",
		Content: "This is a very long progress comment that should wrap across multiple lines instead of overflowing in one single long row.",
		At:      time.Now(),
	})

	rendered := m.RenderMessagesForTesting()
	if !strings.Contains(rendered, "This is a very long progress comment") {
		t.Fatalf("expected rendered comment content, got:\n%s", rendered)
	}
	if lipgloss.Height(rendered) < 2 {
		t.Fatalf("expected wrapped output with multiple lines, got:\n%s", rendered)
	}
}

func TestRenderMessages_LongUserMessageWrapsToPaneWidth(t *testing.T) {
	m := tui.NewModelForTesting()
	m.SetDimensionsForTesting(60, 20)
	m.AddMessageForTesting(tui.ChatMessage{
		Role:    tui.RoleUser,
		Content: "This is a very long user message that should wrap cleanly in the chat pane instead of overflowing on a single line.",
		At:      time.Now(),
	})

	rendered := m.RenderMessagesForTesting()
	if !strings.Contains(rendered, "This is a very long user message") {
		t.Fatalf("expected rendered user content, got:\n%s", rendered)
	}
	if lipgloss.Height(rendered) < 2 {
		t.Fatalf("expected wrapped user output with multiple lines, got:\n%s", rendered)
	}
}
