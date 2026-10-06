package tui

import (
	"strings"
	"testing"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"spettro/internal/agent"
)

// The indicator is the row directly above the input box while a run is in
// flight, and it is not drawn at all when the agent is idle.
func TestWorkingIndicatorSitsAboveTheInputBox(t *testing.T) {
	m := footerModel(120, 40)
	m.messages = append(m.messages, ChatMessage{Role: RoleUser, Content: "hello"})
	m.thinking = true
	m.agentStartAt = time.Now().Add(-12 * time.Second)
	m.workingVerb = "Percolating"
	m.liveRunTokens = 2900
	m = m.recalcLayout()
	m.refreshViewport()

	lines := strings.Split(ansi.Strip(m.View().Content), "\n")
	row := -1
	for i, l := range lines {
		if strings.Contains(l, "Percolating…") {
			row = i
			break
		}
	}
	if row < 0 {
		t.Fatalf("no indicator in the frame:\n%s", strings.Join(lines, "\n"))
	}
	if !strings.Contains(lines[row], "(0m 12s · ↓ 2.9k tokens)") {
		t.Fatalf("indicator is missing its elapsed/token tail: %q", lines[row])
	}
	if !strings.HasPrefix(strings.TrimSpace(lines[row+1]), "┌") {
		t.Fatalf("the indicator must sit immediately above the input box, got %q", lines[row+1])
	}

	m.thinking = false
	m = m.recalcLayout()
	if strings.Contains(ansi.Strip(m.View().Content), "Percolating…") {
		t.Fatalf("the indicator must disappear when the agent stops working")
	}
}

// The symbol animates off the frame counter while the verb stays put: the
// word is drawn once per run, not once per frame.
func TestWorkingIndicatorVerbIsStableWithinATurn(t *testing.T) {
	m := footerModel(120, 40)
	m.thinking = true
	m.beginRunIndicator()

	verb := m.workingVerb
	var symbols []string
	for frame := range 24 {
		m.eyeFrame = frame
		line := ansi.Strip(m.viewWorkingIndicator(80))
		if !strings.Contains(line, verb+"…") {
			t.Fatalf("frame %d re-rolled the verb: %q (want %q)", frame, line, verb)
		}
		symbols = append(symbols, strings.Fields(line)[0])
	}
	if symbols[0] == symbols[len(symbols)-1] {
		t.Fatalf("the symbol should have cycled across 24 frames, stuck on %q", symbols[0])
	}

	// A second run picks a different word, so back-to-back turns never look
	// like the indicator froze.
	m.beginRunIndicator()
	if m.workingVerb == verb {
		t.Fatalf("consecutive runs reused the verb %q", verb)
	}
}

// The verb's sweep follows the mode accent, so plan mode reads purple where
// coding mode does not.
func TestWorkingIndicatorFollowsTheModeAccent(t *testing.T) {
	m := footerModel(120, 40)
	m.thinking = true
	m.agentStartAt = time.Now()
	m.workingVerb = "Conjuring"
	m.eyeFrame = 6

	coding := m.viewWorkingIndicator(80)
	m.mode = "plan"
	if plan := m.viewWorkingIndicator(80); plan == coding {
		t.Fatalf("plan mode must sweep in its own accent colour")
	}
}

func TestFormatRunElapsed(t *testing.T) {
	for _, tc := range []struct {
		d    time.Duration
		want string
	}{
		{12 * time.Second, "0m 12s"},
		{158 * time.Second, "2m 38s"},
		{time.Hour + 2*time.Minute + 3*time.Second, "1h 02m 03s"},
		{-time.Second, "0m 00s"},
	} {
		if got := formatRunElapsed(tc.d); got != tc.want {
			t.Fatalf("formatRunElapsed(%s) = %q, want %q", tc.d, got, tc.want)
		}
	}
}

// The indicator competes for the same rows as the question form and the
// command overlay, so the frame has to stay inside the terminal at the common
// short heights whether the agent is working or idle.
func TestFrameStaysInBoundsWhileWorking(t *testing.T) {
	sizes := []struct{ w, h int }{{80, 24}, {100, 30}, {120, 24}, {80, 22}, {100, 20}}
	for _, size := range sizes {
		for _, working := range []bool{false, true} {
			for _, extra := range []string{"plain", "question", "cmdoverlay"} {
				m := footerModel(size.w, size.h)
				m.messages = append(m.messages, ChatMessage{Role: RoleUser, Content: "hello"})
				switch extra {
				case "question":
					m.SetPendingAskUserFormForTesting(agent.AskUserForm{Questions: []agent.AskUserQuestion{{
						Header:   "Pick",
						Question: "Which of these should the agent do first?",
						Options: []agent.AskUserOption{
							{Label: "Refactor the parser"}, {Label: "Write the tests"},
							{Label: "Ship it"}, {Label: "Ask again later"},
						},
					}}})
				case "cmdoverlay":
					m.cmdItems = m.filterCommands("")
				}
				if working {
					m.thinking = true
					m.agentStartAt = time.Now().Add(-98 * time.Second)
					m.workingVerb = "Materialising"
					m.liveRunTokens = 2900
				}
				m = m.recalcLayout()
				m.refreshViewport()

				if got := lipgloss.Height(m.View().Content); got > size.h {
					t.Fatalf("%dx%d %s working=%v: frame is %d rows", size.w, size.h, extra, working, got)
				}
			}
		}
	}
}
