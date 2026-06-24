package tui

import (
	"fmt"
	"hash/fnv"
	"os/exec"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"spettro/internal/agent"
	"spettro/internal/config"
)

// maxGoalRetries is the number of times the outer loop will retry a single
// iteration after a hard run error (provider failure, etc.) before stalling.
const maxGoalRetries = 2

// resolveGoalMaxIterations applies the config default: 0 means unlimited.
func resolveGoalMaxIterations(cfg config.UserConfig) int {
	if cfg.GoalMaxIterations <= 0 {
		return 0 // unlimited
	}
	return cfg.GoalMaxIterations
}

// resolveGoalNoProgressLimit applies the config default (3) when unset.
func resolveGoalNoProgressLimit(cfg config.UserConfig) int {
	if cfg.GoalNoProgressLimit <= 0 {
		return 3
	}
	return cfg.GoalNoProgressLimit
}

// resolveGoalContextWindow returns the model's context window for goal-mode
// in-loop compaction, falling back to a provider-specific default so the
// runtime always has a sane window.
func resolveGoalContextWindow(m Model) int {
	w := m.contextWindow()
	if w == 0 {
		w = contextWindowDefault(m.cfg.ActiveProvider)
	}
	return w
}

// handleGoalCommand processes /goal <objective>, /goal stop, /goal status.
func (m Model) handleGoalCommand(input string) (tea.Model, tea.Cmd) {
	fields := strings.Fields(input)
	// sub-commands
	if len(fields) >= 2 {
		switch strings.ToLower(fields[1]) {
		case "stop":
			return m.stopGoal("goal stopped by user")
		case "status":
			return m.showGoalStatus()
		}
	}
	objective := strings.TrimSpace(strings.TrimPrefix(input, fields[0]))
	if objective == "" {
		m.showBanner("usage: /goal <objective>   (e.g. /goal update all dependencies)", "info")
		return m, nil
	}
	if m.thinking {
		m.showBanner("a run is already in progress; stop it first", "error")
		return m, nil
	}
	// Permission warning — an autonomous loop pauses on approvals.
	if m.cfg.Permission != config.PermissionYOLO {
		m.pushSystemMsg(fmt.Sprintf(
			"note: permission is %q, so goal mode will pause for approvals. For fully unattended runs use /permission yolo.",
			m.cfg.Permission))
	}
	m.activeGoal = &goalState{
		Objective:       objective,
		StartedAt:       time.Now(),
		MaxIterations:   resolveGoalMaxIterations(m.cfg),
		NoProgressLimit: resolveGoalNoProgressLimit(m.cfg),
	}
	m.pushSystemMsg("starting goal: " + objective)
	m.publishRemoteState("goal_start")
	return m.dispatchGoalIteration()
}

// dispatchGoalIteration builds the orchestrator spec in goal mode and calls
// into the existing run path. Reuses runAgentApproved with goal fields set.
func (m Model) dispatchGoalIteration() (tea.Model, tea.Cmd) {
	g := m.activeGoal
	if g == nil {
		return m, nil
	}
	g.Iteration++
	spec, ok := m.manifest.AgentByID("coding") // orchestrator
	if !ok {
		m.activeGoal = nil
		m.showBanner("coding agent not found in manifest", "error")
		return m, nil
	}
	// Inject the goal-complete tool ONLY for this run.
	spec.AllowedTools = appendUnique(spec.AllowedTools, "goal-complete")

	// Iteration-boundary system message: one terse line per iteration so the
	// transcript records the autonomous continuation.
	m.pushSystemMsg(fmt.Sprintf("↻ goal iteration %d — continuing autonomously toward: %s",
		g.Iteration, truncateLabel(g.Objective, 80)))

	var task string
	if g.Iteration == 1 {
		task = agent.GoalModePreamble + g.Objective
	} else {
		task = fmt.Sprintf(
			"%s%s\n\n(Continuing autonomously — iteration %d. Review what is already done in the workspace, then keep going. Call goal-complete only when the objective is fully met and verified.)",
			agent.GoalModePreamble, g.Objective, g.Iteration)
	}
	g.LastSignature = m.workspaceSignature() // snapshot before the run (progress detection)
	model, cmd := m.runAgent(spec, task, nil, nil)
	// Override the generic progressNote set by runAgentApproved with a
	// goal-specific message so the activity line reflects the goal.
	if tm, ok := model.(Model); ok {
		tm.progressNote = fmt.Sprintf("Pursuing goal (iteration %d): %s",
			g.Iteration, truncateLabel(g.Objective, 60))
		return tm, cmd
	}
	return model, cmd
}

// advanceGoal decides whether the goal is done, stalled, or should continue,
// and returns the tea.Cmd that dispatches the next iteration (or nil to stop).
// Called from the agentDoneMsg handler after a successful (no-error) run.
func (m *Model) advanceGoal(msg agentDoneMsg) tea.Cmd {
	g := m.activeGoal
	if g == nil {
		return nil
	}

	// 1. Completion: the agent called goal-complete (carried on agentDoneMsg).
	if msg.goalComplete {
		summary := msg.goalSummary
		if summary == "" {
			summary = "objective reported complete"
		}
		m.pushSystemMsg("✅ goal complete: " + summary)
		m.showBanner("goal complete", "success")
		m.publishRemoteState("goal_done")
		m.maybeNotify(nil)
		m.activeGoal = nil
		return nil
	}

	// 2. Iteration cap (safety; 0 = unlimited).
	if g.MaxIterations > 0 && g.Iteration >= g.MaxIterations {
		m.pushSystemMsg(fmt.Sprintf("⏹ goal stopped: reached max iterations (%d). Run /goal again to keep going.", g.MaxIterations))
		m.showBanner("goal hit iteration cap", "info")
		m.activeGoal = nil
		return nil
	}

	// 3. No-progress guard.
	sig := m.workspaceSignature()
	if sig == g.LastSignature {
		g.NoProgress++
	} else {
		g.NoProgress = 0
	}
	if g.NoProgress >= g.NoProgressLimit {
		m.pushSystemMsg(fmt.Sprintf("⏹ goal stalled: %d iterations with no detectable progress. Last response:\n%s",
			g.NoProgress, truncateLabel(msg.content, 500)))
		m.showBanner("goal stalled — no progress", "error")
		m.publishRemoteState("goal_stalled")
		m.activeGoal = nil
		return nil
	}

	// 4. Continue. Compact between iterations if the window is tight, exactly
	//    like the normal flow, THEN dispatch. If compaction is needed, run it
	//    and let compactDoneMsg re-enter the loop (see resumeGoalAfterCompact).
	//    Otherwise dispatch immediately.
	if cmd := m.autoCompactIfNeeded(); cmd != nil {
		m.goalResumeAfterCompact = true
		return cmd
	}
	_, cmd := m.dispatchGoalIteration()
	return cmd
}

// advanceGoalOnError handles a hard run error (provider failure, etc.) during
// a goal run. It retries the same iteration a bounded number of times, else
// stalls with a clear error message.
func (m *Model) advanceGoalOnError(msg agentDoneMsg) tea.Cmd {
	g := m.activeGoal
	if g == nil {
		return nil
	}
	g.Retries++
	if g.Retries > maxGoalRetries {
		m.pushSystemMsg(fmt.Sprintf("⏹ goal stopped after %d consecutive errors: %s", g.Retries, msg.err.Error()))
		m.showBanner("goal stopped — repeated errors", "error")
		m.publishRemoteState("goal_error")
		m.activeGoal = nil
		return nil
	}
	m.pushSystemMsg(fmt.Sprintf("⚠ goal iteration %d failed (retry %d/%d): %s",
		g.Iteration, g.Retries, maxGoalRetries, msg.err.Error()))
	// Re-dispatch the same iteration (don't increment Iteration again).
	g.Iteration-- // dispatchGoalIteration will increment it back
	_, cmd := m.dispatchGoalIteration()
	return cmd
}

// stopGoal abandons the active goal and cancels any in-flight run.
func (m Model) stopGoal(reason string) (tea.Model, tea.Cmd) {
	if m.activeGoal == nil {
		m.showBanner("no active goal", "info")
		return m, nil
	}
	m.activeGoal = nil
	if m.cancelAgent != nil {
		m.cancelAgent() // interrupt the in-flight run
	}
	m.showBanner(reason, "info")
	return m, nil
}

// showGoalStatus prints the current goal's iteration / no-progress / elapsed.
func (m Model) showGoalStatus() (tea.Model, tea.Cmd) {
	if m.activeGoal == nil {
		m.showBanner("no active goal", "info")
		return m, nil
	}
	g := m.activeGoal
	m.pushSystemMsg(fmt.Sprintf("goal: %s\niteration: %d\nno-progress: %d/%d\nelapsed: %s",
		g.Objective, g.Iteration, g.NoProgress, g.NoProgressLimit, time.Since(g.StartedAt).Round(time.Second)))
	return m, nil
}

// workspaceSignature returns a cheap fingerprint of the workspace state to
// detect "did anything change this iteration." It hashes the git status
// --porcelain output (set of modified files). Identical signature across
// consecutive iterations => no progress. Conservative: false "progress" just
// lets the loop run longer; false "stall" is the bad case, so we prefer to
// under-report stalls.
func (m Model) workspaceSignature() string {
	cmd := exec.Command("git", "status", "--porcelain")
	cmd.Dir = m.cwd
	out, err := cmd.Output()
	if err != nil {
		// Not a git repo or git unavailable — fall back to a time-based
		// signature so the no-progress guard never falsely trips.
		return fmt.Sprintf("t:%d", time.Now().UnixNano())
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	sort.Strings(lines)
	h := fnv.New64a()
	for _, line := range lines {
		line = strings.TrimRight(line, "\r")
		if line != "" {
			h.Write([]byte(line))
			h.Write([]byte("\n"))
		}
	}
	return fmt.Sprintf("git:%x", h.Sum64())
}

// appendUnique appends s to slice only if it is not already present.
func appendUnique(slice []string, s string) []string {
	for _, v := range slice {
		if v == s {
			return slice
		}
	}
	return append(slice, s)
}
