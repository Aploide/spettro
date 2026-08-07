package tui

import (
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"spettro/internal/agent"
)

// loopState tracks an active /loop schedule: the saved prompt re-runs every
// Interval until /loop stop. nil means no loop is active.
type loopState struct {
	// ID matches loopTickMsg.id; ticks scheduled by a stopped or replaced
	// loop carry a stale ID and are ignored.
	ID        int
	Interval  time.Duration
	Prompt    string
	Iteration int
	Skipped   int // firings skipped because a run was still in progress
	StartedAt time.Time
	NextAt    time.Time
}

// loopTickMsg fires when the active loop's interval elapses.
type loopTickMsg struct{ id int }

func loopTickCmd(id int, d time.Duration) tea.Cmd {
	return tea.Tick(d, func(time.Time) tea.Msg { return loopTickMsg{id: id} })
}

// handleLoopCommand processes /loop <interval> <prompt>, /loop stop, and
// /loop status. The prompt may itself be a slash command (custom commands
// included), mirroring Claude Code's /loop.
func (m Model) handleLoopCommand(input string) (tea.Model, tea.Cmd) {
	fields := strings.Fields(input)
	if len(fields) == 2 {
		switch strings.ToLower(fields[1]) {
		case "stop":
			if m.activeLoop == nil {
				m.showBanner("no active loop", "info")
				return m, nil
			}
			l := m.activeLoop
			m.activeLoop = nil
			m.pushSystemMsg(fmt.Sprintf("loop stopped after %d iteration(s): %s",
				l.Iteration, truncateLabel(l.Prompt, 80)))
			m.showBanner("loop stopped", "info")
			m.refreshViewport()
			return m, nil
		case "status":
			if m.activeLoop == nil {
				m.showBanner("no active loop", "info")
				return m, nil
			}
			l := m.activeLoop
			m.pushSystemMsg(fmt.Sprintf("loop: %s\nevery: %s\niterations: %d (skipped: %d)\nnext run: in %s\nelapsed: %s",
				l.Prompt, l.Interval, l.Iteration, l.Skipped,
				time.Until(l.NextAt).Round(time.Second), time.Since(l.StartedAt).Round(time.Second)))
			m.refreshViewport()
			return m, nil
		}
	}
	rest := strings.TrimSpace(strings.TrimPrefix(input, fields[0]))
	intervalArg, prompt, ok := agent.SplitLoopArgs(rest)
	if !ok {
		m.showBanner("usage: /loop <interval> <prompt>  (e.g. /loop 5m check CI status) — /loop stop | status", "info")
		return m, nil
	}
	interval, err := agent.ParseLoopInterval(intervalArg)
	if err != nil {
		m.showBanner("loop: "+err.Error(), "error")
		return m, nil
	}
	if strings.HasPrefix(prompt, "/loop") {
		m.showBanner("loop: the looped prompt cannot itself be /loop", "error")
		return m, nil
	}
	if m.activeLoop != nil {
		m.showBanner("a loop is already active — stop it first with /loop stop", "error")
		return m, nil
	}
	if m.thinking {
		m.showBanner("a run is already in progress; stop it first", "error")
		return m, nil
	}
	m.loopSeq++
	m.activeLoop = &loopState{
		ID:        m.loopSeq,
		Interval:  interval,
		Prompt:    prompt,
		StartedAt: time.Now(),
		NextAt:    time.Now().Add(interval),
	}
	m.pushSystemMsg(fmt.Sprintf("starting loop — running %q every %s (stop with /loop stop)", prompt, interval))
	newModel, runCmd := m.dispatchLoopIteration()
	nm, isModel := newModel.(Model)
	if !isModel || nm.activeLoop == nil {
		return newModel, runCmd
	}
	return nm, tea.Batch(runCmd, loopTickCmd(nm.activeLoop.ID, interval))
}

// dispatchLoopIteration runs one firing of the active loop. The saved text is
// routed exactly like typed input: slash commands go through handleCommand
// (so custom commands and built-ins work), anything else becomes a normal
// prompt run.
func (m Model) dispatchLoopIteration() (tea.Model, tea.Cmd) {
	l := m.activeLoop
	if l == nil {
		return m, nil
	}
	l.Iteration++
	m.pushSystemMsg(fmt.Sprintf("↻ loop iteration %d — %s", l.Iteration, truncateLabel(l.Prompt, 120)))
	if strings.HasPrefix(l.Prompt, "/") {
		return m.handleCommand(l.Prompt)
	}
	return m.handlePrompt(l.Prompt)
}

// handleLoopTick is the loopTickMsg handler: re-arm the timer, then either
// dispatch the next iteration or skip this firing when a run (loop-started or
// user-started) is still in progress — skipping instead of queueing keeps a
// slow run from piling up a backlog of identical prompts.
func (m Model) handleLoopTick(msg loopTickMsg) (tea.Model, tea.Cmd) {
	l := m.activeLoop
	if l == nil || msg.id != l.ID {
		return m, nil // stale tick from a stopped or replaced loop
	}
	l.NextAt = time.Now().Add(l.Interval)
	next := loopTickCmd(l.ID, l.Interval)
	if m.thinking {
		l.Skipped++
		m.pushSystemMsg(fmt.Sprintf("loop: firing skipped — a run is still in progress (next in %s)", l.Interval))
		m.refreshViewport()
		return m, next
	}
	newModel, runCmd := m.dispatchLoopIteration()
	return newModel, tea.Batch(runCmd, next)
}
