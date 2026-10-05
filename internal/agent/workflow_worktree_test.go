package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"spettro/internal/workflow"
)

// The engine only brackets a call when the Runner implements CallScoper; if
// this assertion ever stops holding, worktree isolation silently stops
// happening and agents edit the shared checkout instead.
var _ workflow.CallScoper = (*workflowRunner)(nil)

func TestWorkflowRunnerCreatesAndMergesWorktree(t *testing.T) {
	repo := testGitRepo(t)
	rt := &toolRuntime{cwd: repo}
	runner := &workflowRunner{rt: rt}
	ctx := context.Background()

	req := workflow.Request{Index: 1, Instance: "general-purpose#1", Isolation: "worktree"}
	if err := runner.BeginCall(ctx, req); err != nil {
		t.Fatalf("BeginCall: %v", err)
	}
	ws := runner.workspaceFor(1)
	if ws == nil {
		t.Fatal("isolation:worktree did not produce a workspace")
	}
	if ws.subCWD == repo {
		t.Fatalf("the agent would run in the shared checkout, not its worktree: %s", ws.subCWD)
	}

	// An edit made with a repository-relative path from inside the worktree
	// must land in the worktree, not the main checkout.
	if err := os.WriteFile(filepath.Join(ws.subCWD, "added.txt"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(repo, "added.txt")); !os.IsNotExist(err) {
		t.Fatal("the edit leaked into the main checkout before the merge")
	}

	runner.EndCall(ctx, req, nil)
	if runner.workspaceFor(1) != nil {
		t.Fatal("the workspace was not released")
	}
	if _, err := os.Stat(filepath.Join(repo, "added.txt")); err != nil {
		t.Fatalf("the worktree was not merged back: %v", err)
	}
	if branches := gitOut(t, repo, "branch", "--list", "spettro/*"); branches != "" {
		t.Fatalf("a clean merge must delete its branch, got:\n%s", branches)
	}
	if len(runner.mergeNotes()) != 0 {
		t.Fatalf("a clean merge must not be reported as a problem: %v", runner.mergeNotes())
	}
}

func TestWorkflowRunnerWithoutIsolationTouchesNothing(t *testing.T) {
	repo := testGitRepo(t)
	runner := &workflowRunner{rt: &toolRuntime{cwd: repo}}
	req := workflow.Request{Index: 1, Instance: "general-purpose#1"}
	if err := runner.BeginCall(context.Background(), req); err != nil {
		t.Fatalf("BeginCall: %v", err)
	}
	if runner.workspaceFor(1) != nil {
		t.Fatal("a call without isolation must not get a worktree")
	}
	runner.EndCall(context.Background(), req, nil)
}

// TestWorkflowRunnerReportsPreservedWorktree: a member that edited its
// worktree and then failed keeps that work on a branch. The branch must be
// reported like a merge conflict is, or nobody learns it exists; a failed
// member that changed nothing leaves nothing to report.
func TestWorkflowRunnerReportsPreservedWorktree(t *testing.T) {
	repo := testGitRepo(t)
	runner := &workflowRunner{rt: &toolRuntime{cwd: repo}}
	ctx := context.Background()

	failed := workflow.Request{Index: 1, Instance: "general-purpose#1", Isolation: "worktree"}
	if err := runner.BeginCall(ctx, failed); err != nil {
		t.Fatalf("BeginCall: %v", err)
	}
	if err := os.WriteFile(filepath.Join(runner.workspaceFor(1).subCWD, "half.txt"), []byte("wip\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runner.EndCall(ctx, failed, errors.New("provider unavailable"))
	notes := runner.mergeNotes()
	if len(notes) != 1 || !strings.Contains(notes[0], "general-purpose#1") || !isPreservedNote(notes[0]) {
		t.Fatalf("a failed member's kept worktree must be reported, got %v", notes)
	}
	if _, err := os.Stat(filepath.Join(repo, "half.txt")); !os.IsNotExist(err) {
		t.Fatal("a failed member's work must not be merged")
	}

	clean := workflow.Request{Index: 2, Instance: "general-purpose#2", Isolation: "worktree"}
	if err := runner.BeginCall(ctx, clean); err != nil {
		t.Fatalf("BeginCall: %v", err)
	}
	runner.EndCall(ctx, clean, errors.New("provider unavailable"))
	if got := runner.mergeNotes(); len(got) != 1 {
		t.Fatalf("a failed member that changed nothing must add no note, got %v", got)
	}
}

// TestRenderSeparatesPreservedFromConflicts: a conflict is finished work to
// merge by hand; a failed member's branch is unfinished work to inspect. The
// result must not tell the model to merge the latter.
func TestRenderSeparatesPreservedFromConflicts(t *testing.T) {
	preserved := "general-purpose#3: " + preservedNoteMarker + ` — branch "spettro/x" kept at /tmp/x — subagent failed`
	conflict := `general-purpose#4: workspace merge conflict — branch "spettro/y" kept at /tmp/y`
	out := renderWorkflowResult("wf_1", "/tmp/run", "inline", workflow.Meta{Name: "m"}, workflow.Result{}, []string{preserved})
	if strings.Contains(out, "merge it, fix conflicts") || !strings.Contains(out, "Do not merge it blindly") {
		t.Fatalf("preserved only:\n%s", out)
	}
	out = renderWorkflowResult("wf_1", "/tmp/run", "inline", workflow.Meta{Name: "m"}, workflow.Result{}, []string{conflict})
	if !strings.Contains(out, "merge it, fix conflicts") || strings.Contains(out, "Do not merge it blindly") {
		t.Fatalf("conflict only:\n%s", out)
	}
}
