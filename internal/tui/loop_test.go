package tui

import (
	"strings"
	"testing"
	"time"

	"spettro/internal/commands"
)

// hasSystemMsg reports whether any system message in the transcript contains
// the given substring.
func hasSystemMsg(m Model, substr string) bool {
	for _, msg := range m.messages {
		if msg.Role == RoleSystem && strings.Contains(msg.Content, substr) {
			return true
		}
	}
	return false
}

func TestLoopCommandValidation(t *testing.T) {
	cases := []struct {
		input  string
		banner string
	}{
		{"/loop", "usage"},
		{"/loop 5m", "usage"},
		{"/loop nope check things", "invalid interval"},
		{"/loop 5 check things", "invalid interval"},
		{"/loop 1s check things", "below the minimum"},
		{"/loop 30s /loop 30s hi", "cannot itself be /loop"},
	}
	for _, tc := range cases {
		m := NewModelForTesting()
		nm, _ := m.handleCommand(tc.input)
		got := nm.(Model)
		if !strings.Contains(got.banner, tc.banner) {
			t.Errorf("%q: banner = %q, want it to contain %q", tc.input, got.banner, tc.banner)
		}
		if got.activeLoop != nil {
			t.Errorf("%q: no loop must start on a rejected command", tc.input)
		}
	}
}

func TestLoopStartDispatchesFirstIteration(t *testing.T) {
	m := NewModelForTesting()
	nm, cmd := m.handleCommand("/loop 30s check the CI status")
	got := nm.(Model)
	if got.activeLoop == nil {
		t.Fatal("expected an active loop")
	}
	if got.activeLoop.Interval != 30*time.Second {
		t.Fatalf("interval = %v", got.activeLoop.Interval)
	}
	if got.activeLoop.Iteration != 1 {
		t.Fatalf("iteration = %d, want 1 (first firing is immediate)", got.activeLoop.Iteration)
	}
	if cmd == nil {
		t.Fatal("expected a batched run+tick command")
	}
	if !hasSystemMsg(got, "starting loop") || !hasSystemMsg(got, "loop iteration 1") {
		t.Fatalf("missing loop banners in transcript: %+v", got.messages)
	}
	// The plain-text prompt goes through the normal prompt path, so the last
	// user message is the loop's prompt.
	var lastUser string
	for _, msg := range got.messages {
		if msg.Role == RoleUser {
			lastUser = msg.Content
		}
	}
	if lastUser != "check the CI status" {
		t.Fatalf("loop prompt not dispatched as user message, got %q", lastUser)
	}
}

func TestLoopRejectsWhileActiveOrThinking(t *testing.T) {
	m := NewModelForTesting()
	nm, _ := m.handleCommand("/loop 30s first loop")
	got := nm.(Model)
	nm2, _ := got.handleCommand("/loop 30s second loop")
	got2 := nm2.(Model)
	if !strings.Contains(got2.banner, "already active") {
		t.Fatalf("banner = %q, want already-active error", got2.banner)
	}
	if got2.activeLoop.Prompt != "first loop" {
		t.Fatalf("second /loop must not replace the first, got %q", got2.activeLoop.Prompt)
	}

	m2 := NewModelForTesting()
	m2.thinking = true
	nm3, _ := m2.handleCommand("/loop 30s hello")
	got3 := nm3.(Model)
	if got3.activeLoop != nil {
		t.Fatal("must not start a loop while a run is in progress")
	}
}

func TestLoopTickDispatchesWhenIdle(t *testing.T) {
	m := NewModelForTesting()
	nm, _ := m.handleCommand("/loop 30s do the thing")
	got := nm.(Model)
	nm2, cmd := got.update(loopTickMsg{id: got.activeLoop.ID})
	got2 := nm2.(Model)
	if got2.activeLoop.Iteration != 2 {
		t.Fatalf("iteration = %d, want 2", got2.activeLoop.Iteration)
	}
	if cmd == nil {
		t.Fatal("tick must re-arm the timer")
	}
}

func TestLoopTickSkipsWhileThinking(t *testing.T) {
	m := NewModelForTesting()
	nm, _ := m.handleCommand("/loop 30s do the thing")
	got := nm.(Model)
	got.thinking = true
	nm2, cmd := got.update(loopTickMsg{id: got.activeLoop.ID})
	got2 := nm2.(Model)
	if got2.activeLoop.Iteration != 1 {
		t.Fatalf("iteration = %d, want 1 (firing skipped)", got2.activeLoop.Iteration)
	}
	if got2.activeLoop.Skipped != 1 {
		t.Fatalf("skipped = %d, want 1", got2.activeLoop.Skipped)
	}
	if cmd == nil {
		t.Fatal("a skipped firing must still re-arm the timer")
	}
	if !hasSystemMsg(got2, "firing skipped") {
		t.Fatal("expected a skipped-firing note in the transcript")
	}
}

func TestLoopStaleTickIgnored(t *testing.T) {
	m := NewModelForTesting()
	nm, _ := m.handleCommand("/loop 30s do the thing")
	got := nm.(Model)
	staleID := got.activeLoop.ID

	// Stop, then start a new loop: the old loop's timer is still armed and
	// will fire with the stale ID.
	nm2, _ := got.handleCommand("/loop stop")
	got2 := nm2.(Model)
	if got2.activeLoop != nil {
		t.Fatal("stop must clear the loop")
	}
	nm3, _ := got2.handleCommand("/loop 45s another thing")
	got3 := nm3.(Model)
	if got3.activeLoop.ID == staleID {
		t.Fatal("a new loop must get a new ID")
	}

	nm4, cmd := got3.update(loopTickMsg{id: staleID})
	got4 := nm4.(Model)
	if got4.activeLoop.Iteration != 1 {
		t.Fatalf("stale tick must not dispatch, iteration = %d", got4.activeLoop.Iteration)
	}
	if cmd != nil {
		t.Fatal("stale tick must not re-arm the timer")
	}
}

func TestLoopStopAndStatus(t *testing.T) {
	m := NewModelForTesting()
	nm, _ := m.handleCommand("/loop status")
	got := nm.(Model)
	if !strings.Contains(got.banner, "no active loop") {
		t.Fatalf("banner = %q", got.banner)
	}

	nm2, _ := got.handleCommand("/loop 30s watch the build")
	got2 := nm2.(Model)
	nm3, _ := got2.handleCommand("/loop status")
	got3 := nm3.(Model)
	if !hasSystemMsg(got3, "iterations: 1") {
		t.Fatal("status must report the iteration count")
	}

	nm4, _ := got3.handleCommand("/loop stop")
	got4 := nm4.(Model)
	if got4.activeLoop != nil {
		t.Fatal("stop must clear the loop")
	}
	if !hasSystemMsg(got4, "loop stopped after 1 iteration(s)") {
		t.Fatal("stop must log a summary")
	}
}

// The looped prompt may itself be a slash command (mirrors Claude Code).
func TestLoopDispatchesSlashCommandPrompt(t *testing.T) {
	m := NewModelForTesting()
	m.customCommands = []commands.Command{
		{Name: "st", Description: "status", Prompt: "Summarize the workspace status.", Scope: "project"},
	}
	nm, _ := m.handleCommand("/loop 30s /st")
	got := nm.(Model)
	if got.activeLoop == nil || got.activeLoop.Iteration != 1 {
		t.Fatal("expected the slash-command loop to start and fire once")
	}
	msgs := got.MessagesForTesting()
	var lastUser string
	for _, msg := range msgs {
		if msg.Role == RoleUser {
			lastUser = msg.Content
		}
	}
	if lastUser != "Summarize the workspace status." {
		t.Fatalf("custom command not expanded on dispatch, got %q", lastUser)
	}
}

func TestLoopInstantSubcommands(t *testing.T) {
	if !isInstantCommand("/loop stop") || !isInstantCommand("/loop status") {
		t.Fatal("/loop stop and /loop status must be instant")
	}
	if isInstantCommand("/loop") || isInstantCommand("/loop 5m check things") {
		t.Fatal("starting a loop must not be instant")
	}
}

func TestInterruptClearsLoop(t *testing.T) {
	m := NewModelForTesting()
	nm, _ := m.handleCommand("/loop 30s do the thing")
	got := nm.(Model)
	got.thinking = true
	got.stopAgent()
	if got.activeLoop != nil {
		t.Fatal("user interrupt must stop the loop")
	}
	if !hasSystemMsg(got, "loop stopped by user interrupt") {
		t.Fatal("interrupt must log that the loop stopped")
	}
}
