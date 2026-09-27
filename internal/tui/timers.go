package tui

import (
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"spettro/internal/agent"
)

// armTimers returns the timers the model needs after an update, marking
// them armed so each is scheduled once:
//
//   - the 50 ms animation tick, while needsAnimation holds;
//   - a one-shot timer for a banner that clears itself (bannerClearAt).
//
// Nothing else wakes an idle TUI but the input cursor's blink.
func (m *Model) armTimers() tea.Cmd {
	var cmds []tea.Cmd
	if !m.tickArmed && m.needsAnimation() {
		m.tickArmed = true
		cmds = append(cmds, tick())
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
	case inputMayGlow(m.ta.Value()):
		return true
	}
	return false
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

// inputMayGlow mirrors highlightUltracode's gate: input holding none of
// these words cannot light up the keyword, so it needs no frames.
func inputMayGlow(input string) bool {
	if input == "" {
		return false
	}
	lower := strings.ToLower(input)
	return strings.Contains(lower, agent.WorkflowKeyword) || strings.Contains(lower, "workflow") || strings.Contains(lower, "agent")
}
