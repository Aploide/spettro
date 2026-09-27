package checkpoint

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// Taking a checkpoint is split in two so the expensive half can run before
// it is needed:
//
//   - Prepare refreshes the shadow index from the working tree, diffs it
//     against the last checkpoint and, when the tree changed, writes the tree
//     and a commit pinned by its ref. That is every git process a snapshot
//     runs.
//   - Commit records a Prepared snapshot as the checkpoint before a tool
//     call: the list entry and the conversation blob, no git process.
//
// Snapshot is Prepare followed by Commit under one lock, so a synchronous
// snapshot and a prepared one produce the same checkpoint. The agent
// prepares while the model is still generating a step, and a mutating tool
// call then only waits for Commit (see internal/agent/checkpoint_policy.go).

// Prepared is a snapshot of the working tree that is not a checkpoint yet.
//
// It can be committed once, and only while it is still the newest thing
// this Checkpointer did: no other Prepare, Commit or RestoreFiles ran since
// (tracked by Checkpointer.gen), and the checkpoint list still ends where it
// did when it was prepared (another process may have appended). Otherwise
// Commit and CommitTracked return ErrStalePrepared and the caller takes a
// fresh Snapshot.
type Prepared struct {
	gen    uint64
	lastID string // ID of the list's last entry when prepared, "" for none

	// unchanged: the staged tree matched the last checkpoint, so Commit
	// reuses that checkpoint's commit.
	unchanged    bool
	commit, tree string
	filesChanged int
	skipped      []string
}

// ErrStalePrepared reports that a Prepared snapshot can no longer be
// committed; take a fresh Snapshot instead.
var ErrStalePrepared = errors.New("checkpoint: prepared snapshot is stale")

// stageMode says how Prepare refreshes the shadow index.
type stageMode int

const (
	// stageAll is `git add -A`: modified, deleted and new untracked files.
	stageAll stageMode = iota
	// stageTracked is `git add -u`: tracked files only. It skips the
	// untracked-file walk, the dominant cost on large trees, and is only
	// used on top of a full staging moments earlier (CommitTracked).
	stageTracked
)

// preparedCommit is a commit Prepare minted that is not a checkpoint yet.
type preparedCommit struct {
	commit, tree string
}

// unclaimedMarker names the file recording the pending unclaimed commit, so
// the next Open can drop its ref when the process exited before claiming or
// replacing it. (Two sessions on one project share the file; the worst case
// is a leaked ref or an unpinned prepared commit, never a broken list.)
const unclaimedMarker = "prepared-commit"

// Prepare stages the working tree and, when it differs from the last
// checkpoint, commits it without recording a checkpoint. label names the
// commit ("checkpoint before <label>").
func (c *Checkpointer) Prepare(label string) (*Prepared, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.prepareLocked(label, stageAll)
}

// Commit records p as the checkpoint before tool. It runs no git process.
// It returns ErrStalePrepared when p can no longer be used (see Prepared).
func (c *Checkpointer) Commit(p *Prepared, tool, prompt string, conversation []byte) (Checkpoint, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.checkFresh(p); err != nil {
		return Checkpoint{}, err
	}
	return c.commitLocked(p, tool, prompt, conversation)
}

// CommitTracked re-stages the tracked files (`git add -u`) on top of p's full
// staging and records the result as the checkpoint before tool: for when
// files changed after p was prepared. Untracked files created since p was
// prepared are not picked up. It returns ErrStalePrepared like Commit.
func (c *Checkpointer) CommitTracked(p *Prepared, tool, prompt string, conversation []byte) (Checkpoint, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.checkFresh(p); err != nil {
		return Checkpoint{}, err
	}
	fresh, err := c.prepareLocked(tool, stageTracked)
	if err != nil {
		return Checkpoint{}, err
	}
	return c.commitLocked(fresh, tool, prompt, conversation)
}

// Snapshot commits the current working tree and stores the given conversation
// blob alongside it. tool describes the pending tool call; prompt is the
// user prompt of the current run.
//
// Commits are parentless and pinned by refs/checkpoints/<hash>, so retention
// can drop any checkpoint independently. The hot path runs as few git
// processes as it can, because on a warm index a process spawn costs more
// than the work each one does:
//
//   - `add -A` refreshes the shadow index, which persists between snapshots,
//     so only files whose stat data changed are rehashed.
//   - One diff-index against the previous checkpoint answers "did anything
//     change", yields FilesChanged, and finds files over the size cap.
//   - An unchanged tree mints no commit: the entry reuses the previous
//     commit, and if the conversation is unchanged too the previous entry is
//     returned as is rather than adding a duplicate.
//   - A changed tree costs write-tree, commit-tree and one update-ref --stdin
//     transaction for both refs (or none, when a prepared, unclaimed commit
//     of the same tree exists).
func (c *Checkpointer) Snapshot(tool, prompt string, conversation []byte) (Checkpoint, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	p, err := c.prepareLocked(tool, stageAll)
	if err != nil {
		return Checkpoint{}, err
	}
	return c.commitLocked(p, tool, prompt, conversation)
}

// checkFresh returns ErrStalePrepared unless p can still be committed.
func (c *Checkpointer) checkFresh(p *Prepared) error {
	if c.disabled {
		return fmt.Errorf("checkpointing disabled")
	}
	if p == nil || p.gen != c.gen {
		return ErrStalePrepared
	}
	list, _ := c.list()
	if lastID(list) != p.lastID {
		return ErrStalePrepared
	}
	return nil
}

// prepareLocked is Prepare with c.mu held.
func (c *Checkpointer) prepareLocked(label string, mode stageMode) (*Prepared, error) {
	if c.disabled {
		return nil, fmt.Errorf("checkpointing disabled")
	}
	c.gen++
	if c.gcDue {
		c.gcDue = false
		_, _ = c.git("gc", "--auto", "--quiet")
	}
	stage := []string{"add", "-A", "."}
	if mode == stageTracked {
		stage = []string{"add", "-u", "."}
	}
	if out, err := c.git(stage...); err != nil {
		return nil, fmt.Errorf("checkpoint add: %v: %s", err, out)
	}
	list, _ := c.list()
	p := &Prepared{gen: c.gen, lastID: lastID(list)}
	base := emptyTree
	if p.lastID != "" {
		base = p.lastID
	}
	changes, diffErr := c.stagedChanges(base)
	if diffErr != nil && base != emptyTree {
		// The previous commit is unreadable (its borrowed objects were
		// pruned): diff against nothing so the snapshot is still taken.
		base = emptyTree
		changes, diffErr = c.stagedChanges(base)
	}
	if diffErr == nil {
		p.skipped, changes = c.excludeOversized(changes)
	}
	if p.lastID != "" && base == p.lastID && diffErr == nil && len(changes) == 0 {
		// No-change fast path: the index matches the previous commit.
		p.unchanged = true
		c.dropUnclaimed()
		return p, nil
	}

	tree, err := c.git("write-tree")
	if err != nil {
		return nil, fmt.Errorf("checkpoint write-tree: %v: %s", err, tree)
	}
	p.tree = tree
	p.filesChanged = len(changes)
	if c.unclaimed.commit != "" && c.unclaimed.tree == tree {
		// An earlier prepared commit of this very tree is still pinned.
		p.commit = c.unclaimed.commit
		return p, nil
	}
	hash, err := c.git("commit-tree", tree, "-m", fmt.Sprintf("checkpoint before %s", label))
	if err != nil {
		return nil, fmt.Errorf("checkpoint commit: %v: %s", err, hash)
	}
	refs := fmt.Sprintf("update refs/checkpoints/%s %s\nupdate HEAD %s\n", hash, hash, hash)
	if c.unclaimed.commit != "" {
		// The earlier prepared commit was never claimed: unpin it in the
		// same transaction.
		refs += fmt.Sprintf("delete refs/checkpoints/%s\n", c.unclaimed.commit)
	}
	if out, err := c.gitStdin(refs, "update-ref", "--stdin"); err != nil {
		return nil, fmt.Errorf("checkpoint ref: %v: %s", err, out)
	}
	c.setUnclaimed(preparedCommit{commit: hash, tree: tree})
	p.commit = hash
	return p, nil
}

// commitLocked is Commit with c.mu held and p known to be fresh.
func (c *Checkpointer) commitLocked(p *Prepared, tool, prompt string, conversation []byte) (Checkpoint, error) {
	list, _ := c.list()
	c.gen++
	if len(prompt) > 200 {
		prompt = prompt[:200] + "…"
	}
	cp := Checkpoint{
		At:           time.Now(),
		Tool:         tool,
		Prompt:       prompt,
		SkippedLarge: p.skipped,
	}
	if p.unchanged {
		prev := list[len(list)-1]
		if c.sameConversation(prev, conversation) {
			return prev, nil
		}
		cp.ID = prev.ID
		cp.Tree = prev.Tree
		cp.Conv = fmt.Sprintf("%s-%d", prev.ID[:min(12, len(prev.ID))], len(list))
	} else {
		cp.ID = p.commit
		cp.Tree = p.tree
		cp.FilesChanged = p.filesChanged
	}

	if err := c.storeConversation(&cp, conversation); err != nil {
		return Checkpoint{}, err
	}
	list = append(list, cp)
	if err := c.writeList(list); err != nil {
		return Checkpoint{}, err
	}
	if cp.ID == c.unclaimed.commit {
		c.clearUnclaimed()
	}
	if len(list)%c.opts.GCEvery == 0 {
		c.gcDue = true
	}
	return cp, nil
}

func (c *Checkpointer) unclaimedMarkerPath() string {
	return filepath.Join(c.dir, unclaimedMarker)
}

// setUnclaimed records pc as the pending unclaimed commit.
func (c *Checkpointer) setUnclaimed(pc preparedCommit) {
	c.unclaimed = pc
	_ = os.WriteFile(c.unclaimedMarkerPath(), []byte(pc.commit+"\n"), 0o600)
}

// clearUnclaimed forgets the unclaimed commit (it became a checkpoint, or
// its ref was deleted).
func (c *Checkpointer) clearUnclaimed() {
	c.unclaimed = preparedCommit{}
	_ = os.Remove(c.unclaimedMarkerPath())
}

// dropUnclaimed unpins the unclaimed commit, if any.
func (c *Checkpointer) dropUnclaimed() {
	if c.unclaimed.commit == "" {
		return
	}
	_, _ = c.git("update-ref", "-d", "refs/checkpoints/"+c.unclaimed.commit)
	c.clearUnclaimed()
}

// dropUnclaimedFromLastSession unpins the prepared commit an earlier process
// left unclaimed (see unclaimedMarker). Runs on Open.
func (c *Checkpointer) dropUnclaimedFromLastSession() {
	data, err := os.ReadFile(c.unclaimedMarkerPath())
	if err != nil {
		return
	}
	defer os.Remove(c.unclaimedMarkerPath())
	hash := strings.TrimSpace(string(data))
	if !isCommitHash(hash) {
		return
	}
	list, _ := c.list()
	if slices.ContainsFunc(list, func(cp Checkpoint) bool { return cp.ID == hash }) {
		return
	}
	_, _ = c.git("update-ref", "-d", "refs/checkpoints/"+hash)
}

// isCommitHash reports whether s looks like a full SHA-1 or SHA-256 object
// name, so a damaged marker can never name an arbitrary ref.
func isCommitHash(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !('0' <= s[i] && s[i] <= '9' || 'a' <= s[i] && s[i] <= 'f') {
			return false
		}
	}
	return true
}
