package tui

import (
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"spettro/internal/agent"
	"spettro/internal/jobs"
	"spettro/internal/pty"
)

// armTimers returns the timers the model needs after an update, marking
// them armed so each is scheduled once:
//
//   - the 50 ms animation tick, while needsAnimation holds;
//   - the 1 s clock tick, while needsClock holds;
//   - a one-shot timer for a banner that clears itself (bannerClearAt).
//
// Nothing else wakes an idle TUI but the input cursor's blink.
func (m *Model) armTimers() tea.Cmd {
	var cmds []tea.Cmd
	if !m.tickArmed && m.needsAnimation() {
		m.tickArmed = true
		cmds = append(cmds, tick())
	}
	if !m.clockArmed && m.needsClock() {
		m.clockArmed = true
		cmds = append(cmds, clockTick())
	}
	if m.banner != "" && !m.bannerClearAt.IsZero() && !m.bannerClearAt.Equal(m.bannerTimerAt) {
		at := m.bannerClearAt
		m.bannerTimerAt = at
		cmds = append(cmds, tea.Tick(max(time.Until(at), 0), func(time.Time) tea.Msg { return bannerExpiredMsg{at: at} }))
	}
	return tea.Batch(cmds...)
}

// needsAnimation reports whether anything on screen advances on eyeFrame,
// which only the animation tick moves forward. Each case is covered by
// TestAnimatedElementsKeepTicking.
func (m Model) needsAnimation() bool {
	switch {
	case m.eyeIntroStarted && m.eyeIntroFrame < eyeIntroFrames:
		return true
	case m.thinking:
		// Working indicator (glare, symbol, elapsed time), running tool
		// rows, live pty tails.
		return true
	case m.showOnboarding, m.showLogin:
		// Key verification bar and sign-in spinners.
		return true
	case m.spettroPlanName() == "max":
		// The rainbow MAX label in the header.
		return true
	case m.activeGoal != nil:
		// The goal's elapsed time in the status bar.
		return true
	case m.hasRunningDelegation():
		return true
	case m.hasInProgressTodo():
		return true
	case inputMayGlow(m.ta.Value(), m.ultracode):
		return true
	}
	return false
}

// The idle blink of the eyes logo runs on its own slow tick chain rather
// than the animation tick: the intro sits on the open frame, then a brief
// run of fast frames (idleEyesStep each) plays the blink down and up, and
// the chain rests on the open eyes for idleEyesRest before blinking again.
// The chain re-arms itself until the first user message stops it.
const (
	idleEyesStep   = 60 * time.Millisecond
	idleEyesRest   = 4 * time.Second
	idleEyesFrames = 6 // blink frames; frame 0 is the open-eyes rest
)

// idleEyesTickMsg advances the idle blink by one frame.
type idleEyesTickMsg struct{}

func idleEyesTick(delay time.Duration) tea.Cmd {
	return tea.Tick(delay, func(time.Time) tea.Msg { return idleEyesTickMsg{} })
}

// clockTickInterval is how often the clock tick redraws the status bar.
// The values it shows (a /loop's countdown, rounded to the second, and the
// background job and pty counters) never need more.
const clockTickInterval = time.Second

// clockTickMsg redraws the chrome for a value that changes with the wall
// clock or with work outside the TUI rather than with eyeFrame. It is not
// transcriptOnly, so its Update re-renders the memoized status bar.
type clockTickMsg time.Time

func clockTick() tea.Cmd {
	return tea.Tick(clockTickInterval, func(t time.Time) tea.Msg { return clockTickMsg(t) })
}

// needsClock reports whether the status bar shows something that changes
// while the TUI receives no message at all: an idle /loop counting down to
// its next iteration, or a background job or pty session, which can end at
// any moment and must then leave the counter. Each case is covered by
// TestClockElementsKeepTicking.
func (m Model) needsClock() bool {
	return m.activeLoop != nil || jobs.Default().RunningCount() > 0 || pty.Default().RunningCount() > 0
}

// hasRunningDelegation reports whether a sub-agent or a workflow phase is
// running: their rows glare.
func (m Model) hasRunningDelegation() bool {
	for _, a := range m.parallelAgents {
		if a.Status == "running" {
			return true
		}
	}
	return m.workflow != nil && m.workflow.Status == "running"
}

// hasInProgressTodo reports whether a task row glares (todoRow).
func (m Model) hasInProgressTodo() bool {
	for _, td := range m.todos {
		if td.Status == "in_progress" || td.Status == "running" {
			return true
		}
	}
	return false
}

// inputMayGlow mirrors highlightWorkflowInput's gate: input holding none of
// these words cannot light up the keyword, so it needs no frames. With the
// ultracode toggle on, a "+" may also be a budget directive about to glow.
func inputMayGlow(input string, ultracode bool) bool {
	if input == "" {
		return false
	}
	if ultracode && strings.Contains(input, "+") {
		return true
	}
	lower := strings.ToLower(input)
	return strings.Contains(lower, agent.WorkflowKeyword) || strings.Contains(lower, "workflow") || strings.Contains(lower, "agent")
}

// budgetDirectivesLive reports whether a "+500k" in the input would be
// honoured: workflows have to be on for the message, by its own words or by
// the session's ultracode toggle. The "+" check keeps the regex pass off the
// common path, since the input is rendered on every frame.
func (m Model) budgetDirectivesLive() bool {
	if m.ultracode {
		return true
	}
	value := m.ta.Value()
	return strings.Contains(value, "+") && agent.WorkflowRequested(value)
}
