package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"spettro/internal/platform"
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
	// Without exec there is no git status to hash, and the cwd-mtime fallback
	// below is actively wrong here: editing a file two directories down does
	// not touch the root's mtime, so every iteration would fingerprint
	// identically and goal mode would declare "no detectable progress" while
	// the agent was in fact working. Walk the tree instead.
	if !platform.CanExec() {
		return walkSignature(cwd)
	}
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

// walkSignatureMaxFiles bounds the pure-Go workspace walk. Progress detection
// only needs the fingerprint to change when the agent edits something, and it
// runs once per goal iteration, so a partial-but-deterministic hash of a huge
// tree is a better trade than a slow complete one. Entries are visited in
// directory order, which os.ReadDir sorts by name, so the truncation point is
// stable across iterations.
const walkSignatureMaxFiles = 20000

// walkSignatureSkipDirs are directory names never worth hashing: they are
// large, machine-generated, and change for reasons unrelated to agent
// progress.
var walkSignatureSkipDirs = map[string]struct{}{
	".git": {}, "node_modules": {}, "vendor": {}, ".venv": {}, "venv": {},
	"target": {}, "dist": {}, "build": {}, ".next": {}, ".build": {},
	"DerivedData": {}, "Pods": {}, ".spettro": {},
}

// walkSignature fingerprints a working tree by hashing each file's path, size
// and modification time. It is the exec-free equivalent of hashing
// `git status --porcelain`: content is not read, so the cost is one stat per
// file, and any edit, creation or deletion moves the hash.
func walkSignature(cwd string) string {
	h := sha256.New()
	n := 0
	var walk func(dir, rel string)
	walk = func(dir, rel string) {
		if n >= walkSignatureMaxFiles {
			return
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, e := range entries {
			if n >= walkSignatureMaxFiles {
				return
			}
			name := e.Name()
			childRel := name
			if rel != "" {
				childRel = rel + "/" + name
			}
			if e.IsDir() {
				if _, skip := walkSignatureSkipDirs[name]; skip {
					continue
				}
				walk(filepath.Join(dir, name), childRel)
				continue
			}
			info, err := e.Info()
			if err != nil {
				continue
			}
			n++
			fmt.Fprintf(h, "%s\x00%d\x00%d\n", childRel, info.Size(), info.ModTime().UnixNano())
		}
	}
	walk(cwd, "")
	if n == 0 {
		// Unreadable or empty tree: an empty signature means "unknown", which
		// the caller already treats as no change rather than as progress.
		return ""
	}
	return hex.EncodeToString(h.Sum(nil))
}
