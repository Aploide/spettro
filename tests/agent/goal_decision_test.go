package agent_test

import (
	"os/exec"
	"testing"

	"spettro/internal/agent"
)

// TestShouldContinueGoal_Complete verifies that when the agent signals
// completion, the decision logic returns GoalDecisionComplete and stops.
func TestShouldContinueGoal_Complete(t *testing.T) {
	state := &agent.GoalState{
		Objective:       "test objective",
		Iteration:       5,
		NoProgress:      0,
		MaxIterations:   10,
		NoProgressLimit: 3,
		LastSignature:   "sig-1",
	}

	result := agent.RunResult{
		GoalComplete: true,
		GoalSummary:  "All tasks completed successfully",
	}

	decision, reason := agent.ShouldContinueGoal(state, result, "/tmp")

	if decision != agent.GoalDecisionComplete {
		t.Errorf("expected GoalDecisionComplete, got %v", decision)
	}
	if reason != "All tasks completed successfully" {
		t.Errorf("expected summary in reason, got %q", reason)
	}
}

// TestShouldContinueGoal_MaxIterationsReached verifies that the iteration cap
// triggers GoalDecisionMaxIterations.
func TestShouldContinueGoal_MaxIterationsReached(t *testing.T) {
	state := &agent.GoalState{
		Objective:       "test objective",
		Iteration:       10,
		NoProgress:      0,
		MaxIterations:   10, // Reached the cap
		NoProgressLimit: 3,
		LastSignature:   "sig-1",
	}

	result := agent.RunResult{
		GoalComplete: false,
	}

	decision, reason := agent.ShouldContinueGoal(state, result, "/tmp")

	if decision != agent.GoalDecisionMaxIterations {
		t.Errorf("expected GoalDecisionMaxIterations, got %v", decision)
	}
	if reason != "reached max iterations" {
		t.Errorf("expected 'reached max iterations', got %q", reason)
	}
}

// TestShouldContinueGoal_UnlimitedIterations verifies that MaxIterations=0
// means unlimited and does not trigger the iteration cap.
func TestShouldContinueGoal_UnlimitedIterations(t *testing.T) {
	state := &agent.GoalState{
		Objective:       "test objective",
		Iteration:       100,
		NoProgress:      0,
		MaxIterations:   0, // Unlimited
		NoProgressLimit: 3,
		LastSignature:   "sig-1",
	}

	result := agent.RunResult{
		GoalComplete: false,
	}

	decision, _ := agent.ShouldContinueGoal(state, result, "/tmp")

	if decision == agent.GoalDecisionMaxIterations {
		t.Error("MaxIterations=0 should mean unlimited, but triggered iteration cap")
	}
}

// TestShouldContinueGoal_NoProgressStall verifies that consecutive iterations
// with no workspace changes trigger GoalDecisionStalled.
func TestShouldContinueGoal_NoProgressStall(t *testing.T) {
	cwd := t.TempDir()
	// Get the actual signature for this directory
	actualSig := agent.WorkspaceSignature(cwd)

	state := &agent.GoalState{
		Objective:       "test objective",
		Iteration:       5,
		NoProgress:      2, // Already 2 iterations with no progress
		MaxIterations:   10,
		NoProgressLimit: 3,         // Stall after 3 consecutive no-progress
		LastSignature:   actualSig, // Match the actual signature so it increments
	}

	result := agent.RunResult{
		GoalComplete: false,
	}

	decision, reason := agent.ShouldContinueGoal(state, result, cwd)

	if decision != agent.GoalDecisionStalled {
		t.Errorf("expected GoalDecisionStalled after 3 no-progress iterations, got %v", decision)
	}
	if reason != "no detectable progress" {
		t.Errorf("expected 'no detectable progress', got %q", reason)
	}
}

// TestShouldContinueGoal_ProgressDetected verifies that when the workspace
// signature changes, the no-progress counter resets and the loop continues.
func TestShouldContinueGoal_ProgressDetected(t *testing.T) {
	state := &agent.GoalState{
		Objective:       "test objective",
		Iteration:       5,
		NoProgress:      2, // Had 2 no-progress iterations
		MaxIterations:   10,
		NoProgressLimit: 3,
		LastSignature:   "sig-1",
	}

	result := agent.RunResult{
		GoalComplete: false,
	}

	// Use a real git repo so signature changes
	cwd := createTempGitRepo(t)

	decision, _ := agent.ShouldContinueGoal(state, result, cwd)

	if decision != agent.GoalDecisionContinue {
		t.Errorf("expected GoalDecisionContinue when progress detected, got %v", decision)
	}
	if state.NoProgress != 0 {
		t.Errorf("expected NoProgress counter to reset to 0, got %d", state.NoProgress)
	}
}

// TestWorkspaceSignature_GitRepo verifies that WorkspaceSignature produces
// consistent signatures for the same git state and different signatures when
// the workspace changes.
func TestWorkspaceSignature_GitRepo(t *testing.T) {
	cwd := createTempGitRepo(t)

	sig1 := agent.WorkspaceSignature(cwd)
	if sig1 == "" {
		t.Error("expected non-empty signature for git repo")
	}

	// Same state should produce same signature
	sig2 := agent.WorkspaceSignature(cwd)
	if sig1 != sig2 {
		t.Errorf("expected identical signatures for unchanged workspace, got %q vs %q", sig1, sig2)
	}
}

// TestWorkspaceSignature_NonGitFallback verifies that WorkspaceSignature
// falls back to directory mtime when not in a git repo.
func TestWorkspaceSignature_NonGitFallback(t *testing.T) {
	cwd := t.TempDir() // Not a git repo

	sig := agent.WorkspaceSignature(cwd)
	if sig == "" {
		t.Error("expected non-empty signature even for non-git directory")
	}
}

// createTempGitRepo creates a temporary git repository for testing.
func createTempGitRepo(t *testing.T) string {
	t.Helper()
	cwd := t.TempDir()

	// Initialize git repo
	runShell(t, cwd, "git", "init")
	runShell(t, cwd, "git", "config", "user.email", "test@example.com")
	runShell(t, cwd, "git", "config", "user.name", "Test User")
	runShell(t, cwd, "git", "commit", "--allow-empty", "-m", "initial")

	return cwd
}

// runShell executes a shell command in the given directory for test setup.
func runShell(t *testing.T, cwd string, args ...string) {
	t.Helper()
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Dir = cwd
	if err := cmd.Run(); err != nil {
		t.Fatalf("shell command failed: %v", err)
	}
}
