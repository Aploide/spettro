package agent

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// testGitRepo creates a repository with one commit and a local identity so
// merge commits work without relying on the host's git config.
func testGitRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	// Resolved, because git reports the toplevel with symlinks resolved and
	// t.TempDir sits under a symlink on macOS (/var -> /private/var). Without
	// this the test compares two spellings of the same directory.
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-b", "main")
	run("config", "user.name", "test")
	run("config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "-A")
	run("commit", "-m", "init")
	return dir
}

func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestWorkspaceMergeCycle(t *testing.T) {
	ctx := context.Background()
	repo := testGitRepo(t)

	ws, err := newAgentWorkspace(ctx, repo, "code#1")
	if err != nil {
		t.Fatalf("newAgentWorkspace: %v", err)
	}
	if !strings.HasPrefix(ws.branch, "spettro/code-1-") {
		t.Fatalf("branch = %q, want spettro/code-1-* prefix", ws.branch)
	}
	wantDir := filepath.Join(repo, ".spettro", "worktrees")
	if filepath.Dir(ws.path) != wantDir {
		t.Fatalf("worktree at %q, want under %q", ws.path, wantDir)
	}

	// Subagent work: uncommitted edit inside the worktree.
	if err := os.WriteFile(filepath.Join(ws.path, "feature.txt"), []byte("done\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	res := ws.finalize(ctx)
	if res.Status != "merged" {
		t.Fatalf("finalize status = %q (%s), want merged", res.Status, res.Detail)
	}
	if _, err := os.Stat(filepath.Join(repo, "feature.txt")); err != nil {
		t.Fatalf("merged file missing in main checkout: %v", err)
	}
	if _, err := os.Stat(ws.path); !os.IsNotExist(err) {
		t.Fatalf("worktree %s not removed", ws.path)
	}
	if branches := gitOut(t, repo, "branch", "--list", ws.branch); branches != "" {
		t.Fatalf("branch %s not deleted: %q", ws.branch, branches)
	}
	if !strings.Contains(gitOut(t, repo, "log", "-1", "--pretty=%s"), "Merge branch") {
		t.Fatal("HEAD is not the merge commit")
	}
}

func TestWorkspaceNoChanges(t *testing.T) {
	ctx := context.Background()
	repo := testGitRepo(t)

	ws, err := newAgentWorkspace(ctx, repo, "review")
	if err != nil {
		t.Fatalf("newAgentWorkspace: %v", err)
	}
	res := ws.finalize(ctx)
	if res.Status != "no_changes" {
		t.Fatalf("finalize status = %q, want no_changes", res.Status)
	}
	if _, err := os.Stat(ws.path); !os.IsNotExist(err) {
		t.Fatalf("worktree %s not removed", ws.path)
	}
	if head := gitOut(t, repo, "log", "-1", "--pretty=%s"); head != "init" {
		t.Fatalf("main HEAD moved: %q", head)
	}
}

func TestWorkspaceConflictKeepsBranch(t *testing.T) {
	ctx := context.Background()
	repo := testGitRepo(t)

	ws, err := newAgentWorkspace(ctx, repo, "code")
	if err != nil {
		t.Fatalf("newAgentWorkspace: %v", err)
	}
	// Same file diverges in the worktree and on main.
	if err := os.WriteFile(filepath.Join(ws.path, "README.md"), []byte("subagent\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("mainline\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitOut(t, repo, "commit", "-am", "mainline edit")

	res := ws.finalize(ctx)
	if res.Status != "conflict" {
		t.Fatalf("finalize status = %q (%s), want conflict", res.Status, res.Detail)
	}
	// Merge aborted: main checkout clean, branch and worktree preserved.
	if status := gitOut(t, repo, "status", "--porcelain"); status != "" {
		t.Fatalf("main checkout dirty after aborted merge:\n%s", status)
	}
	if branches := gitOut(t, repo, "branch", "--list", ws.branch); branches == "" {
		t.Fatalf("conflicted branch %s was deleted", ws.branch)
	}
	if _, err := os.Stat(ws.path); err != nil {
		t.Fatalf("conflicted worktree removed: %v", err)
	}
}

func TestWorkspaceAbandon(t *testing.T) {
	ctx := context.Background()
	repo := testGitRepo(t)

	// Clean workspace: abandon deletes it silently.
	ws, err := newAgentWorkspace(ctx, repo, "a")
	if err != nil {
		t.Fatal(err)
	}
	if kept := ws.abandon(ctx); kept != nil {
		t.Fatalf("abandon of clean workspace kept it: %+v", kept)
	}
	if _, err := os.Stat(ws.path); !os.IsNotExist(err) {
		t.Fatal("clean workspace not removed")
	}

	// Workspace with work in it: abandon preserves it.
	ws2, err := newAgentWorkspace(ctx, repo, "b")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws2.path, "wip.txt"), []byte("wip\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	kept := ws2.abandon(ctx)
	if kept == nil || kept.Status != "preserved" {
		t.Fatalf("abandon of dirty workspace = %+v, want preserved", kept)
	}
	if _, err := os.Stat(ws2.path); err != nil {
		t.Fatalf("dirty workspace removed: %v", err)
	}
}

func TestWorkspaceSubdirCWD(t *testing.T) {
	ctx := context.Background()
	repo := testGitRepo(t)
	sub := filepath.Join(repo, "pkg", "api")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "api.go"), []byte("package api\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitOut(t, repo, "add", "-A")
	gitOut(t, repo, "commit", "-m", "add pkg")

	ws, err := newAgentWorkspace(ctx, sub, "code")
	if err != nil {
		t.Fatalf("newAgentWorkspace: %v", err)
	}
	defer ws.cleanup(ctx)
	if want := filepath.Join(ws.path, "pkg", "api"); ws.subCWD != want {
		t.Fatalf("subCWD = %q, want %q", ws.subCWD, want)
	}
}

func TestWorkspaceExcludesSpettroDir(t *testing.T) {
	ctx := context.Background()
	repo := testGitRepo(t)

	ws, err := newAgentWorkspace(ctx, repo, "code")
	if err != nil {
		t.Fatal(err)
	}
	defer ws.cleanup(ctx)
	// The worktree lives under .spettro/ yet must not show up in git status.
	if status := gitOut(t, repo, "status", "--porcelain"); status != "" {
		t.Fatalf("worktree pollutes main checkout status:\n%s", status)
	}
}

// fakeCommitter stands in for LLMCommitter: commits everything with a fixed
// message, or fails without touching the tree.
type fakeCommitter struct {
	msg  string
	fail bool
}

func (f fakeCommitter) Commit(ctx context.Context, cwd string) (string, error) {
	if f.fail {
		return "", os.ErrDeadlineExceeded
	}
	for _, args := range [][]string{
		{"add", "-A"},
		{"commit", "-m", f.msg},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = cwd
		if _, err := cmd.CombinedOutput(); err != nil {
			return "", err
		}
	}
	return f.msg, nil
}

func TestWorkspaceCommitterWritesMessage(t *testing.T) {
	ctx := context.Background()
	repo := testGitRepo(t)

	ws, err := newAgentWorkspace(ctx, repo, "code")
	if err != nil {
		t.Fatal(err)
	}
	ws.committer = fakeCommitter{msg: "feat: add feature file"}
	if err := os.WriteFile(filepath.Join(ws.path, "feature.txt"), []byte("done\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if res := ws.finalize(ctx); res.Status != "merged" {
		t.Fatalf("finalize status = %q (%s), want merged", res.Status, res.Detail)
	}
	subjects := gitOut(t, repo, "log", "--pretty=%s")
	if !strings.Contains(subjects, "feat: add feature file") {
		t.Fatalf("committer message missing from history:\n%s", subjects)
	}
	if strings.Contains(subjects, "spettro: subagent") {
		t.Fatalf("stock fallback message used despite working committer:\n%s", subjects)
	}
}

func TestWorkspaceCommitterFallback(t *testing.T) {
	ctx := context.Background()
	repo := testGitRepo(t)

	ws, err := newAgentWorkspace(ctx, repo, "code")
	if err != nil {
		t.Fatal(err)
	}
	ws.committer = fakeCommitter{fail: true}
	if err := os.WriteFile(filepath.Join(ws.path, "feature.txt"), []byte("done\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if res := ws.finalize(ctx); res.Status != "merged" {
		t.Fatalf("finalize status = %q (%s), want merged", res.Status, res.Detail)
	}
	if !strings.Contains(gitOut(t, repo, "log", "--pretty=%s"), "spettro: subagent code work") {
		t.Fatal("fallback commit missing after committer failure")
	}
	// The fallback commit is authored by Spettro itself, under its real
	// identity.
	if !strings.Contains(gitOut(t, repo, "log", "--pretty=%an <%ae>"), "Spettro <spettro@eyed.to>") {
		t.Fatalf("fallback commit not authored as Spettro <spettro@eyed.to>:\n%s",
			gitOut(t, repo, "log", "--pretty=%an <%ae>"))
	}
}

func TestWorkspaceSlug(t *testing.T) {
	cases := map[string]string{
		"code#3":     "code-3",
		"Code Agent": "code-agent",
		"##":         "agent",
		"":           "agent",
	}
	for in, want := range cases {
		if got := workspaceSlug(in); got != want {
			t.Errorf("workspaceSlug(%q) = %q, want %q", in, got, want)
		}
	}
}
