package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

// Subagent workspace isolation. When a delegation (agent tool) or swarm
// (ultra tool) call sets isolation="worktree", each subagent runs in its own
// git worktree under <repo>/.spettro/worktrees/, on a branch named after the
// subagent. Concurrent subagents therefore never trip over each other's edits
// in the shared checkout. When a subagent finishes, its branch is merged back
// into the main checkout and both the branch and the worktree are deleted; a
// merge conflict preserves the branch and worktree and reports them so the
// orchestrating agent (or the user) can resolve by hand.

const (
	workspaceDirName      = "worktrees"
	workspaceBranchPrefix = "spettro/"
)

// workspaceMu serializes every git mutation of the main checkout (worktree
// add/remove, branch delete, merge). Parallel agent tool calls and swarm
// merges would otherwise race on the repository index and ref locks.
var workspaceMu sync.Mutex

// agentWorkspace is one isolated worktree+branch pair owned by a subagent.
type agentWorkspace struct {
	repoRoot  string      // toplevel of the main checkout
	path      string      // absolute worktree path under <repoRoot>/.spettro/worktrees/
	branch    string      // spettro/<slug>-<id>
	baseRef   string      // commit the branch forked from, for change detection
	name      string      // subagent instance name, used in commit/merge messages
	subCWD    string      // cwd handed to the subagent (parent's cwd mapped into the worktree)
	committer CommitAgent // writes the commit message for leftover work; nil falls back to a stock message
}

// workspaceMerge is the outcome of folding a workspace back into the main
// checkout. Status is one of: "merged", "no_changes", "conflict" (branch and
// worktree preserved for manual resolution), "preserved" (subagent failed but
// left work behind), or "error".
type workspaceMerge struct {
	Status string
	Branch string
	Path   string
	Detail string
}

// workspaceGit runs git with user hooks and commit signing disabled so local
// configuration cannot break or intercept the automated workspace lifecycle.
func workspaceGit(ctx context.Context, dir string, args ...string) (string, error) {
	base := []string{"-c", "core.hooksPath=/dev/null", "-c", "commit.gpgsign=false"}
	cmd := exec.CommandContext(ctx, "git", append(base, args...)...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// workspaceSlug maps an agent instance name ("code#3") onto a branch/dir-safe
// slug ("code-3").
func workspaceSlug(name string) string {
	var b strings.Builder
	lastDash := true
	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastDash = false
		default:
			if !lastDash {
				b.WriteByte('-')
				lastDash = true
			}
		}
	}
	slug := strings.Trim(b.String(), "-")
	if slug == "" {
		slug = "agent"
	}
	if len(slug) > 32 {
		slug = slug[:32]
	}
	return slug
}

func workspaceID() string {
	var buf [3]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return fmt.Sprintf("%06d", os.Getpid()%1000000)
	}
	return hex.EncodeToString(buf[:])
}

// ensureLocalGitExclude makes sure the repository ignores .spettro/ via
// .git/info/exclude, so worktree churn never pollutes the user's git status
// even when their .gitignore does not cover it. Best-effort: the exclude file
// is untracked repo-local state, never a tracked file we would be editing.
func ensureLocalGitExclude(ctx context.Context, repoRoot string) {
	gitDir, err := workspaceGit(ctx, repoRoot, "rev-parse", "--git-common-dir")
	if err != nil {
		return
	}
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(repoRoot, gitDir)
	}
	path := filepath.Join(gitDir, "info", "exclude")
	data, _ := os.ReadFile(path)
	for line := range strings.SplitSeq(string(data), "\n") {
		if strings.TrimSpace(line) == ".spettro/" {
			return
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.WriteString("\n# spettro subagent workspaces\n.spettro/\n")
}

// newAgentWorkspace creates the worktree+branch pair for one subagent. The
// worktree forks from the current HEAD of the repository containing cwd.
func newAgentWorkspace(ctx context.Context, cwd, name string) (*agentWorkspace, error) {
	root, err := workspaceGit(ctx, cwd, "rev-parse", "--show-toplevel")
	if err != nil {
		return nil, fmt.Errorf("worktree isolation requires a git repository: %s", truncate(root, 300))
	}
	baseRef, err := workspaceGit(ctx, root, "rev-parse", "HEAD")
	if err != nil {
		return nil, fmt.Errorf("worktree isolation requires at least one commit: %s", truncate(baseRef, 300))
	}
	slug := workspaceSlug(name) + "-" + workspaceID()
	w := &agentWorkspace{
		repoRoot: root,
		path:     filepath.Join(root, ".spettro", workspaceDirName, slug),
		branch:   workspaceBranchPrefix + slug,
		baseRef:  baseRef,
		name:     name,
	}
	ensureLocalGitExclude(ctx, root)
	if err := os.MkdirAll(filepath.Dir(w.path), 0o755); err != nil {
		return nil, fmt.Errorf("create worktrees dir: %w", err)
	}
	workspaceMu.Lock()
	out, err := workspaceGit(ctx, root, "worktree", "add", "-b", w.branch, "--", w.path)
	workspaceMu.Unlock()
	if err != nil {
		return nil, fmt.Errorf("create worktree: %s: %w", truncate(out, 500), err)
	}
	// Preserve the parent's position inside the repo: an agent delegated from
	// <root>/pkg/api should start in <worktree>/pkg/api, not the tree root.
	// cwd is resolved first because git reports the toplevel with symlinks
	// already resolved (/private/var/... for a cwd under /var/... on macOS);
	// comparing the two raw would make every path look like an escape and
	// silently drop the subagent at the tree root.
	w.subCWD = w.path
	realCWD, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		realCWD = filepath.Clean(cwd)
	}
	if rel, err := filepath.Rel(root, realCWD); err == nil && !pathEscapesWS(rel) {
		if sub := filepath.Join(w.path, rel); dirExistsWS(sub) {
			w.subCWD = sub
		}
	}
	return w, nil
}

// pathEscapesWS reports whether a relative path leaves its base, or is the
// base itself. A plain strings.HasPrefix(rel, "..") would also reject a real
// directory named "..foo", so the separator is part of the test.
func pathEscapesWS(rel string) bool {
	return rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func dirExistsWS(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.IsDir()
}

// newSubagentWorkspace builds the workspace for a subagent spawned by this
// runtime, wiring in the LLM commit-message writer so leftover subagent work
// is committed with a real Conventional Commits subject instead of a stock
// message.
func (r *toolRuntime) newSubagentWorkspace(ctx context.Context, name string) (*agentWorkspace, error) {
	ws, err := newAgentWorkspace(ctx, r.cwd, name)
	if err != nil {
		return nil, err
	}
	if r.providerMgr != nil {
		ws.committer = LLMCommitter{
			ProviderManager: r.providerMgr,
			ProviderName:    r.providerName,
			ModelName:       r.modelName,
		}
	}
	return ws, nil
}

// commitPending commits any uncommitted subagent work on the workspace
// branch. The commit message comes from the configured committer (the LLM
// commit-message writer), so workspace commits read like any other commit in
// the project's history; if that fails (provider error, oversized diff,
// hooks/gpg trouble) a hardened stock-message commit keeps the merge going.
// Identity on the fallback is pinned: it is an intermediate automation commit
// that gets folded into the user's history through the merge commit.
func (w *agentWorkspace) commitPending(ctx context.Context) (string, error) {
	dirty, err := isGitDirty(ctx, w.path)
	if err != nil || !dirty {
		return "", err
	}
	if w.committer != nil {
		if _, err := w.committer.Commit(ctx, w.path); err == nil {
			return "", nil
		}
	}
	if out, err := workspaceGit(ctx, w.path, "add", "-A"); err != nil {
		return "git add: " + truncate(out, 500), err
	}
	msg := fmt.Sprintf("spettro: subagent %s work", w.name)
	out, err := workspaceGit(ctx, w.path,
		"-c", "user.name="+spettroCommitName, "-c", "user.email="+spettroCommitEmail,
		"commit", "-m", msg, "--trailer", coAuthor)
	if err != nil {
		return "git commit: " + truncate(out, 500), err
	}
	return "", nil
}

// hasCommits reports whether the workspace branch has any commits past the
// fork point.
func (w *agentWorkspace) hasCommits(ctx context.Context) bool {
	count, err := workspaceGit(ctx, w.path, "rev-list", "--count", w.baseRef+".."+w.branch)
	return err == nil && strings.TrimSpace(count) != "0"
}

// cleanup removes the worktree and deletes the branch. Safe to call on a
// partially torn-down workspace.
func (w *agentWorkspace) cleanup(ctx context.Context) {
	workspaceMu.Lock()
	defer workspaceMu.Unlock()
	_, _ = workspaceGit(ctx, w.repoRoot, "worktree", "remove", "--force", "--", w.path)
	_, _ = workspaceGit(ctx, w.repoRoot, "branch", "-D", "--", w.branch)
}

// finalize folds a finished subagent's workspace back into the main checkout:
// commit leftover work, merge the branch into the current HEAD, then delete
// branch and worktree. On conflict the merge is aborted and the branch and
// worktree are preserved for manual resolution.
func (w *agentWorkspace) finalize(ctx context.Context) workspaceMerge {
	res := workspaceMerge{Branch: w.branch, Path: w.path}
	if detail, err := w.commitPending(ctx); err != nil {
		res.Status = "error"
		res.Detail = detail
		if res.Detail == "" {
			res.Detail = err.Error()
		}
		return res
	}
	if !w.hasCommits(ctx) {
		w.cleanup(ctx)
		return workspaceMerge{Status: "no_changes", Branch: w.branch}
	}
	workspaceMu.Lock()
	msg := fmt.Sprintf("Merge branch '%s' (spettro subagent %s)", w.branch, w.name)
	out, err := workspaceGit(ctx, w.repoRoot, "merge", "--no-ff", "-m", msg, "--", w.branch)
	if err != nil {
		_, _ = workspaceGit(ctx, w.repoRoot, "merge", "--abort")
		workspaceMu.Unlock()
		res.Status = "error"
		if strings.Contains(out, "CONFLICT") {
			res.Status = "conflict"
		}
		res.Detail = truncate(out, 800)
		return res
	}
	workspaceMu.Unlock()
	w.cleanup(ctx)
	return workspaceMerge{Status: "merged", Branch: w.branch}
}

// abandon handles a subagent that failed: throwaway workspaces (no commits,
// clean tree) are deleted; anything with work in it is preserved and reported
// so nothing a subagent produced is silently lost.
func (w *agentWorkspace) abandon(ctx context.Context) *workspaceMerge {
	dirty, err := isGitDirty(ctx, w.path)
	if err == nil && !dirty && !w.hasCommits(ctx) {
		w.cleanup(ctx)
		return nil
	}
	return &workspaceMerge{
		Status: "preserved",
		Branch: w.branch,
		Path:   w.path,
		Detail: "subagent failed; its work is preserved on the branch and worktree",
	}
}
