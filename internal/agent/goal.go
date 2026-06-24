package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"
)

// GoalDecision represents the outcome of evaluating a goal iteration.
type GoalDecision int

const (
	GoalDecisionContinue GoalDecision = iota
	GoalDecisionComplete
	GoalDecisionStalled
	GoalDecisionMaxIterations
)

// GoalState tracks the progress of a goal-mode run across iterations.
type GoalState struct {
	Objective       string
	Iteration       int
	NoProgress      int
	StartedAt       time.Time
	LastSignature   string
	MaxIterations   int // 0 means unlimited
	NoProgressLimit int
}

// ShouldContinueGoal evaluates whether a goal-mode run should continue after
// an iteration completes. It implements the same logic used by the TUI's
// advanceGoal: check for completion, iteration cap, and no-progress stall.
//
// This function is shared between the TUI and headless goal runners.
func ShouldContinueGoal(state *GoalState, result RunResult, cwd string) (decision GoalDecision, reason string) {
	// 1. Goal complete: the agent called goal-complete
	if result.GoalComplete {
		summary := result.GoalSummary
		if summary == "" {
			summary = "objective reported complete"
		}
		return GoalDecisionComplete, summary
	}

	// 2. Iteration cap (safety)
	if state.MaxIterations > 0 && state.Iteration >= state.MaxIterations {
		return GoalDecisionMaxIterations, "reached max iterations"
	}

	// 3. No-progress guard
	sig := WorkspaceSignature(cwd)
	if sig == state.LastSignature {
		state.NoProgress++
	} else {
		state.NoProgress = 0
	}
	state.LastSignature = sig

	if state.NoProgress >= state.NoProgressLimit {
		return GoalDecisionStalled, "no detectable progress"
	}

	return GoalDecisionContinue, ""
}

// WorkspaceSignature computes a fingerprint of the current workspace state
// for progress detection. It hashes git status --porcelain output. If not in
// a git repo, falls back to modification time of the working directory.
//
// This is the shared implementation used by both TUI and headless goal runners.
func WorkspaceSignature(cwd string) string {
	cmd := exec.Command("git", "status", "--porcelain")
	cmd.Dir = cwd
	out, err := cmd.Output()
	if err != nil {
		// Not a git repo: fall back to directory mtime
		info, err := os.Stat(cwd)
		if err != nil {
			return ""
		}
		return info.ModTime().Format(time.RFC3339Nano)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	sort.Strings(lines)
	h := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return hex.EncodeToString(h[:])
}
