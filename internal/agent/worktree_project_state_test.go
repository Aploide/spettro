package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// worktreeCWD lays out <main>/.spettro/worktrees/<slug>, the directory an
// isolated sub-agent runs in, and returns the main checkout and that dir.
func worktreeCWD(t *testing.T) (main, wt string) {
	t.Helper()
	main = t.TempDir()
	wt = filepath.Join(main, ".spettro", workspaceDirName, "sub-1")
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatal(err)
	}
	return main, wt
}

// .spettro/ is never checked out into an agent worktree, so a sub-agent
// running there reads the project's hooks from the main checkout: a
// project PreToolUse deny applies to it like to the parent.
func TestWorktreeSubagentKeepsProjectHooks(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	main, wt := worktreeCWD(t)
	hooksJSON := `{"hooks":[{"id":"no-writes","event":"PreToolUse","matcher":"file-write","command":"echo '{\"decision\":\"deny\",\"reason\":\"project says no\"}'"}]}`
	if err := os.WriteFile(filepath.Join(main, ".spettro", "hooks.json"), []byte(hooksJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	pm, url, _ := newLoopServer(t,
		loopReply{toolName: "file-write", toolArgs: `{"path":"new.txt","content":"x"}`},
		loopReply{content: "done"},
	)
	cfg := loopCfg(t, pm, url)
	cfg.CWD = wt
	res, err := runToolLoop(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(wt, "new.txt")); err == nil {
		t.Fatal("the project's deny hook did not apply inside the worktree")
	}
	if !toolResultContains(res.messages, "project says no") {
		t.Fatal("the hook's reason was not reported")
	}
}

// The allow-always command list lives in the main checkout's .spettro/
// too: it is read from there, and a choice made inside the worktree is
// saved there, not into a directory that is deleted after the merge.
func TestWorktreeSubagentUsesMainAllowList(t *testing.T) {
	main, wt := worktreeCWD(t)
	if err := saveAllowedCommandSet(projectStateDir(wt), map[string]struct{}{"echo hi": {}}); err != nil {
		t.Fatal(err)
	}
	set, err := loadAllowedCommandSet(main)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := set["echo hi"]; !ok {
		t.Fatalf("allow-list saved from the worktree did not reach the main checkout: %v", set)
	}
	if got := projectStateDir(filepath.Join(wt, "pkg")); got != filepath.Join(main, "pkg") {
		t.Fatalf("projectStateDir(worktree/pkg) = %q", got)
	}
	if got := projectStateDir(main); got != main {
		t.Fatalf("projectStateDir(main) = %q", got)
	}
}
