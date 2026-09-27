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
// (tracked by Checkpointer.gen), the checkpoint list still ends where it did
// when it was prepared (another process may have appended), and it is
// younger than PreparedMaxAge. Otherwise Commit and CommitTracked return
// ErrStalePrepared and the caller takes a fresh Snapshot.
type Prepared struct {
	gen    uint64
	at     time.Time // when staging began
	lastID string    // ID of the list's last entry when prepared, "" for none

	// unchanged: the staged tree matched the last checkpoint, so Commit
	// reuses that checkpoint's commit.
	unchanged    bool
	commit, tree string
	filesChanged int
	skipped      []string
}

// PreparedMaxAge is how long after its staging began a Prepared snapshot
// can still be committed. It bounds how long a change made by another
// program can go unrecorded (the prepared tree predates it), and it lets
// Open tell a prepared commit that a live session may still claim from one
// an exited process abandoned (see abandonedPreparedAge).
const PreparedMaxAge = 30 * time.Second

// abandonedPreparedAge is how old a pending prepared commit's marker must
// be before Open unpins the commit. Another live session may still claim a
// commit it prepared up to PreparedMaxAge ago, and may reuse a pending
// commit up to PreparedMaxAge old for a new Prepared (see prepareLocked), so
// nothing younger than twice that can still be claimed; the rest is margin.
const abandonedPreparedAge = 4 * PreparedMaxAge

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
	at           time.Time // when it was pinned
}

// pendingDir names the directory holding one marker file per pending
// prepared commit (named by its hash). The owning process removes a marker
// when its commit becomes a checkpoint or is unpinned; Open unpins the
// commits of markers older than abandonedPreparedAge, left behind by a
// process that exited first. One file per commit, rather than one shared
// file, keeps two sessions on one project from unpinning each other's
// commits.
const pendingDir = "prepared"

// Prepare stages the working tree and, when it differs from the last
// checkpoint, commits it without recording a checkpoint. label names the
// commit ("checkpoint before <label>"). A git gc that a recorded checkpoint
// made due runs here first, off the path of the tool call that will claim
// the result.
func (c *Checkpointer) Prepare(label string) (*Prepared, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.runDueGC()
	p, _, err := c.prepareLocked(label, stageAll)
	return p, err
}

// Commit records p as the checkpoint before tool. It runs no git process.
// It returns ErrStalePrepared when p can no longer be used (see Prepared).
func (c *Checkpointer) Commit(p *Prepared, tool, prompt string, conversation []byte) (Checkpoint, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	list, err := c.checkFresh(p)
	if err != nil {
		return Checkpoint{}, err
	}
	return c.commitLocked(p, list, tool, prompt, conversation)
}

// CommitTracked re-stages the tracked files (`git add -u`) on top of p's full
// staging and records the result as the checkpoint before tool: for when
// files changed after p was prepared. Untracked files created since p was
// prepared are not picked up. It returns ErrStalePrepared like Commit.
func (c *Checkpointer) CommitTracked(p *Prepared, tool, prompt string, conversation []byte) (Checkpoint, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, err := c.checkFresh(p); err != nil {
		return Checkpoint{}, err
	}
	fresh, list, err := c.prepareLocked(tool, stageTracked)
	if err != nil {
		return Checkpoint{}, err
	}
	return c.commitLocked(fresh, list, tool, prompt, conversation)
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
//     transaction for both refs (or none, when a young prepared, unclaimed
//     commit of the same tree exists).
//
// Every GCEvery-th checkpoint also runs git gc --auto: after the snapshot,
// on the caller's path, when nothing prepared ahead picked it up first.
func (c *Checkpointer) Snapshot(tool, prompt string, conversation []byte) (Checkpoint, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	p, list, err := c.prepareLocked(tool, stageAll)
	if err != nil {
		return Checkpoint{}, err
	}
	cp, err := c.commitLocked(p, list, tool, prompt, conversation)
	if err == nil {
		c.runDueGC()
	}
	return cp, err
}

// checkFresh returns the checkpoint list p is committed onto, or
// ErrStalePrepared unless p can still be committed. The list is read once
// here and handed to commitLocked, so a concurrent writer cannot change it
// between the check and its use.
func (c *Checkpointer) checkFresh(p *Prepared) ([]Checkpoint, error) {
	if c.disabled {
		return nil, fmt.Errorf("checkpointing disabled")
	}
	if p == nil || p.gen != c.gen || time.Since(p.at) >= PreparedMaxAge {
		return nil, ErrStalePrepared
	}
	list, err := c.list()
	if err != nil || lastID(list) != p.lastID {
		// Another process rewrote the list (or is rewriting it right now):
		// the prepared diff base may no longer be the last checkpoint.
		return nil, ErrStalePrepared
	}
	return list, nil
}

// runDueGC runs git gc --auto when a recorded checkpoint made it due.
func (c *Checkpointer) runDueGC() {
	if !c.gcDue || c.disabled {
		return
	}
	c.gcDue = false
	_, _ = c.git("gc", "--auto", "--quiet")
}

// gcDueAt reports whether a list of n checkpoints makes git gc --auto due.
func (c *Checkpointer) gcDueAt(n int) bool {
	return n > 0 && n%c.opts.GCEvery == 0
}

// prepareLocked is Prepare with c.mu held. It also returns the checkpoint
// list it diffed against, for commitLocked.
func (c *Checkpointer) prepareLocked(label string, mode stageMode) (*Prepared, []Checkpoint, error) {
	if c.disabled {
		return nil, nil, fmt.Errorf("checkpointing disabled")
	}
	c.gen++
	p := &Prepared{gen: c.gen, at: time.Now()}
	stage := []string{"add", "-A", "."}
	if mode == stageTracked {
		stage = []string{"add", "-u", "."}
	}
	if out, err := c.git(stage...); err != nil {
		return nil, nil, fmt.Errorf("checkpoint add: %v: %s", err, out)
	}
	list, _ := c.list()
	p.lastID = lastID(list)
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
		c.dropUnclaimed(list)
		return p, list, nil
	}

	tree, err := c.git("write-tree")
	if err != nil {
		return nil, nil, fmt.Errorf("checkpoint write-tree: %v: %s", err, tree)
	}
	p.tree = tree
	p.filesChanged = len(changes)
	if c.unclaimed.commit != "" && c.unclaimed.tree == tree && time.Since(c.unclaimed.at) < PreparedMaxAge {
		// An earlier prepared commit of this very tree is still pinned, and
		// young enough that no other session can have unpinned it yet
		// (abandonedPreparedAge).
		p.commit = c.unclaimed.commit
		return p, list, nil
	}
	hash, err := c.git("commit-tree", tree, "-m", fmt.Sprintf("checkpoint before %s", label))
	if err != nil {
		return nil, nil, fmt.Errorf("checkpoint commit: %v: %s", err, hash)
	}
	refs := fmt.Sprintf("update refs/checkpoints/%s %s\nupdate HEAD %s\n", hash, hash, hash)
	if c.unclaimed.commit != "" && c.unclaimed.commit != hash && !recorded(list, c.unclaimed.commit) {
		// The earlier prepared commit was never claimed: unpin it in the
		// same transaction (deleting a ref that is already gone succeeds).
		refs += fmt.Sprintf("delete refs/checkpoints/%s\n", c.unclaimed.commit)
	}
	if out, err := c.gitStdin(refs, "update-ref", "--stdin"); err != nil {
		return nil, nil, fmt.Errorf("checkpoint ref: %v: %s", err, out)
	}
	c.clearUnclaimed()
	c.setUnclaimed(preparedCommit{commit: hash, tree: tree, at: time.Now()})
	p.commit = hash
	return p, list, nil
}

// commitLocked is Commit with c.mu held. list is the checkpoint list p was
// checked against (checkFresh) or prepared from (prepareLocked); it is not
// re-read, so the entry appended is always the one p describes.
func (c *Checkpointer) commitLocked(p *Prepared, list []Checkpoint, tool, prompt string, conversation []byte) (Checkpoint, error) {
	if lastID(list) != p.lastID {
		return Checkpoint{}, ErrStalePrepared
	}
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
		// p.unchanged implies p.lastID != "", so list is not empty.
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
	if c.gcDueAt(len(list)) {
		c.gcDue = true
	}
	return cp, nil
}

// pendingMarkerPath is the marker file of the pending prepared commit hash.
func (c *Checkpointer) pendingMarkerPath(hash string) string {
	return filepath.Join(c.dir, pendingDir, hash)
}

// setUnclaimed records pc as the pending unclaimed commit, with its marker.
func (c *Checkpointer) setUnclaimed(pc preparedCommit) {
	c.unclaimed = pc
	path := c.pendingMarkerPath(pc.commit)
	if err := os.WriteFile(path, nil, 0o600); errors.Is(err, os.ErrNotExist) {
		if os.MkdirAll(filepath.Dir(path), 0o700) == nil {
			_ = os.WriteFile(path, nil, 0o600)
		}
	}
}

// clearUnclaimed forgets the unclaimed commit (it became a checkpoint, or
// its ref was deleted) and removes its marker.
func (c *Checkpointer) clearUnclaimed() {
	if c.unclaimed.commit != "" {
		_ = os.Remove(c.pendingMarkerPath(c.unclaimed.commit))
	}
	c.unclaimed = preparedCommit{}
}

// dropUnclaimed unpins the unclaimed commit, if any. list is the current
// checkpoint list: a commit that is also a recorded checkpoint (the same
// tree committed with the same message within one second hashes the same)
// keeps its ref.
func (c *Checkpointer) dropUnclaimed(list []Checkpoint) {
	if c.unclaimed.commit == "" {
		return
	}
	if !recorded(list, c.unclaimed.commit) {
		_, _ = c.git("update-ref", "-d", "refs/checkpoints/"+c.unclaimed.commit)
	}
	c.clearUnclaimed()
}

// recorded reports whether hash is the commit of a checkpoint in list.
func recorded(list []Checkpoint, hash string) bool {
	return slices.ContainsFunc(list, func(cp Checkpoint) bool { return cp.ID == hash })
}

// dropAbandonedPrepared unpins the prepared commits whose markers are older
// than abandonedPreparedAge: their process exited before claiming or
// replacing them. Younger ones may belong to a live session on the same
// project and are left alone. Runs on Open.
func (c *Checkpointer) dropAbandonedPrepared() {
	entries, err := os.ReadDir(filepath.Join(c.dir, pendingDir))
	if err != nil {
		return
	}
	var abandoned []string
	for _, e := range entries {
		info, err := e.Info()
		if err != nil || !isCommitHash(e.Name()) || time.Since(info.ModTime()) < abandonedPreparedAge {
			continue
		}
		abandoned = append(abandoned, e.Name())
	}
	if len(abandoned) == 0 {
		return
	}
	list, _ := c.list()
	var refs strings.Builder
	for _, hash := range abandoned {
		// A commit that did become a checkpoint (its owner exited between
		// recording it and removing the marker) keeps its ref.
		if !recorded(list, hash) {
			fmt.Fprintf(&refs, "delete refs/checkpoints/%s\n", hash)
		}
	}
	if refs.Len() > 0 {
		if _, err := c.gitStdin(refs.String(), "update-ref", "--stdin"); err != nil {
			return // keep the markers; the next Open retries
		}
	}
	for _, hash := range abandoned {
		_ = os.Remove(c.pendingMarkerPath(hash))
	}
}

// isCommitHash reports whether s looks like a full SHA-1 or SHA-256 object
// name, so a stray file in the marker directory can never name an arbitrary
// ref.
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
