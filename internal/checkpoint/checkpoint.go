// Package checkpoint snapshots the project working tree into a shadow git
// repository before any file-modifying tool runs, so the user can rewind
// files and/or conversation to any earlier step via /rewind.
//
// The shadow repository lives under Spettro's data dir
// (~/.spettro/history/<project_hash>/repo.git) and is completely separate
// from the user's own .git. Because commits are made with the project as the
// work tree, the project's .gitignore files are honoured and nested .git
// directories are never tracked.
//
// Storage design: when the project has its own .git, the shadow repo borrows
// its objects via objects/info/alternates instead of copying them, so the
// shadow store only holds uncommitted deltas. Checkpoint commits are minted
// parentless and pinned by per-checkpoint refs (refs/checkpoints/<hash>), so
// retention is just "delete the ref, let gc collect the objects". Files above
// a size cap are excluded from snapshots and recorded on the checkpoint so
// /rewind can warn about them.
package checkpoint

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// emptyTree is git's well-known hash of the empty tree, used as a diff base
// before the first commit exists.
const emptyTree = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"

// Options tunes shadow-repo storage behaviour. Zero values mean "use the
// default"; Disabled turns checkpointing off entirely.
type Options struct {
	Disabled      bool
	MaxFileMB     int // files larger than this are not snapshotted (default 20)
	RetentionDays int // checkpoints older than this are pruned on Open (default 14)
	MaxGB         int // shadow-store size cap enforced on Open (default 5)
	WarnGB        int // big-repo warning threshold when alternates are unavailable (default 2)
	GCEvery       int // run git gc --auto every N snapshots (default 20)
}

func (o Options) withDefaults() Options {
	if o.MaxFileMB <= 0 {
		o.MaxFileMB = 20
	}
	if o.RetentionDays <= 0 {
		o.RetentionDays = 14
	}
	if o.MaxGB <= 0 {
		o.MaxGB = 5
	}
	if o.WarnGB <= 0 {
		o.WarnGB = 2
	}
	if o.GCEvery <= 0 {
		o.GCEvery = 20
	}
	return o
}

// Checkpoint is one snapshot: a shadow commit plus the conversation state
// that was current when the pending tool call triggered it.
type Checkpoint struct {
	ID           string    `json:"id"` // shadow-repo commit hash
	At           time.Time `json:"at"`
	Tool         string    `json:"tool"`   // tool call that triggered the snapshot
	Prompt       string    `json:"prompt"` // user prompt of the run, truncated
	FilesChanged int       `json:"files_changed"`
	Tree         string    `json:"tree,omitempty"` // tree hash, for no-change dedupe
	Conv         string    `json:"conv,omitempty"` // conversation blob key when != ID
	// SkippedLarge lists files that exceeded the size cap and were not
	// snapshotted; /rewind surfaces them so a restore isn't silently partial.
	SkippedLarge []string `json:"skipped_large,omitempty"`
}

// ConvKey returns the key of the conversation blob stored with this
// checkpoint. Deduplicated (no-change) entries share a commit with an earlier
// checkpoint but keep their own conversation snapshot.
func (cp Checkpoint) ConvKey() string {
	if cp.Conv != "" {
		return cp.Conv
	}
	return cp.ID
}

type Checkpointer struct {
	mu         sync.Mutex
	project    string // user's project root (work tree)
	dir        string // ~/.spettro/history/<hash>
	gitDir     string // <dir>/repo.git
	opts       Options
	disabled   bool   // git missing or init failed; snapshots become no-ops
	alternates bool   // borrowing objects from the project's own .git
	warning    string // one-time big-repo warning, empty if not applicable
}

// Dir returns the per-project history directory under globalDir.
func Dir(globalDir, projectPath string) string {
	sum := sha256.Sum256([]byte(projectPath))
	return filepath.Join(globalDir, "history", fmt.Sprintf("%x", sum[:8]))
}

// Open prepares the shadow repository for projectPath with default options.
func Open(globalDir, projectPath string) (*Checkpointer, error) {
	return OpenWith(globalDir, projectPath, Options{})
}

// OpenWith prepares (creating if needed) the shadow repository for
// projectPath. Besides init, Open is where all storage maintenance happens so
// the per-snapshot hot path stays fast: alternates wiring, retention pruning
// and the big-repo size check all run here.
func OpenWith(globalDir, projectPath string, opts Options) (*Checkpointer, error) {
	dir := Dir(globalDir, projectPath)
	c := &Checkpointer{
		project: projectPath,
		dir:     dir,
		gitDir:  filepath.Join(dir, "repo.git"),
		opts:    opts.withDefaults(),
	}
	if opts.Disabled {
		c.disabled = true
		return c, fmt.Errorf("checkpointing disabled by config")
	}
	if _, err := exec.LookPath("git"); err != nil {
		c.disabled = true
		return c, fmt.Errorf("git not found: checkpointing disabled")
	}
	if err := os.MkdirAll(filepath.Join(dir, "conv"), 0o700); err != nil {
		return nil, err
	}
	if _, err := os.Stat(filepath.Join(c.gitDir, "HEAD")); err != nil {
		// init cannot take --git-dir/--work-tree, so run it bare.
		cmd := exec.Command("git", "init", "--quiet", "--bare", c.gitDir)
		if out, err := cmd.CombinedOutput(); err != nil {
			c.disabled = true
			return c, fmt.Errorf("shadow repo init: %v: %s", err, strings.TrimSpace(string(out)))
		}
		// Cut `add -A` latency on large trees.
		_, _ = c.git("config", "core.untrackedCache", "true")
		_, _ = c.git("config", "index.version", "4")
	}
	c.setupAlternates()
	c.writeDefaultExcludes()
	c.enforceRetention()
	c.computeWarning()
	return c, nil
}

// setupAlternates points the shadow object store at the project's own
// objects directory so already-committed content is borrowed, not copied.
// Trade-off (documented in docs/checkpointing.md): if the user later prunes
// those objects (`git gc --prune`), affected checkpoints can no longer be
// restored; RestoreFiles detects that and fails with a clear error rather
// than crashing.
func (c *Checkpointer) setupAlternates() {
	cmd := exec.Command("git", "rev-parse", "--git-common-dir")
	cmd.Dir = c.project
	out, err := cmd.Output()
	if err != nil {
		return // project is not a git repo; nothing to borrow
	}
	commonDir := strings.TrimSpace(string(out))
	if !filepath.IsAbs(commonDir) {
		commonDir = filepath.Join(c.project, commonDir)
	}
	objects := filepath.Join(commonDir, "objects")
	if st, err := os.Stat(objects); err != nil || !st.IsDir() {
		return
	}
	infoDir := filepath.Join(c.gitDir, "objects", "info")
	if err := os.MkdirAll(infoDir, 0o700); err != nil {
		return
	}
	if err := os.WriteFile(filepath.Join(infoDir, "alternates"), []byte(objects+"\n"), 0o600); err != nil {
		return
	}
	c.alternates = true
}

// writeDefaultExcludes seeds the shadow repo's own exclude file (honoured in
// addition to the project's gitignores) with classic heavyweight artifacts.
// Snapshot appends over-cap file paths below the marker line, so the seed is
// only written when the file does not exist yet.
func (c *Checkpointer) writeDefaultExcludes() {
	path := filepath.Join(c.gitDir, "info", "exclude")
	if _, err := os.Stat(path); err == nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return
	}
	defaults := strings.Join([]string{
		"# spettro checkpoint excludes (defaults + files over checkpoint_max_file_mb)",
		"*.iso",
		"*.img",
		"*.qcow2",
		"*.vmdk",
		"*.safetensors",
		"*.gguf",
		"*.ckpt",
		"*.onnx",
		"*.pt",
		"*.pth",
		"",
	}, "\n")
	_ = os.WriteFile(path, []byte(defaults), 0o600)
}

// computeWarning prepares the one-time big-repo banner: without alternates a
// first snapshot copies the whole tree into ~/.spettro, so above WarnGB the
// user should know before it happens. The estimate walks the project and
// stops as soon as the threshold is crossed.
func (c *Checkpointer) computeWarning() {
	if c.alternates {
		return
	}
	marker := filepath.Join(c.dir, "size-warned")
	if _, err := os.Stat(marker); err == nil {
		return
	}
	limit := int64(c.opts.WarnGB) * 1 << 30
	var total int64
	_ = filepath.WalkDir(c.project, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", ".venv", "target":
				return filepath.SkipDir
			}
			return nil
		}
		if info, err := d.Info(); err == nil {
			total += info.Size()
		}
		if total > limit {
			return filepath.SkipAll
		}
		return nil
	})
	if total <= limit {
		return
	}
	c.warning = fmt.Sprintf(
		"checkpointing will store a copy of this project (>%d GB) under ~/.spettro — see /checkpoints, disable with checkpointing_disabled",
		c.opts.WarnGB)
	_ = os.WriteFile(marker, []byte(time.Now().Format(time.RFC3339)+"\n"), 0o600)
}

// Warning returns the one-time big-repo notice to show the user, or "".
func (c *Checkpointer) Warning() string { return c.warning }

// git runs a git command against the shadow repo with the project as the
// work tree. Identity and global config are pinned so user config and hooks
// can never interfere.
func (c *Checkpointer) git(args ...string) (string, error) {
	full := append([]string{
		"--git-dir=" + c.gitDir,
		"--work-tree=" + c.project,
		"-c", "core.bare=false",
		"-c", "user.name=spettro",
		"-c", "user.email=spettro@localhost",
		"-c", "commit.gpgsign=false",
		"-c", "core.hooksPath=/dev/null",
		// No reflogs: retention deletes per-checkpoint refs so gc can
		// collect the objects; a reflog would keep them reachable forever.
		"-c", "core.logAllRefUpdates=false",
	}, args...)
	cmd := exec.Command("git", full...)
	cmd.Dir = c.project
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
	)
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// excludeOversized drops staged files above the size cap from the index and
// appends them to the shadow exclude file so future `add -A` calls skip them
// without rehashing. Returns the relative paths that were skipped.
func (c *Checkpointer) excludeOversized(base string) []string {
	out, err := c.git("diff", "--cached", "--name-only", "--diff-filter=AM", "-z", base)
	if err != nil || out == "" {
		return nil
	}
	limit := int64(c.opts.MaxFileMB) << 20
	var skipped []string
	for _, rel := range strings.Split(strings.Trim(out, "\x00"), "\x00") {
		if rel == "" {
			continue
		}
		st, err := os.Stat(filepath.Join(c.project, rel))
		if err != nil || st.Size() <= limit {
			continue
		}
		if _, err := c.git("rm", "--cached", "--quiet", "--", rel); err == nil {
			skipped = append(skipped, rel)
		}
	}
	if len(skipped) > 0 {
		f, err := os.OpenFile(filepath.Join(c.gitDir, "info", "exclude"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err == nil {
			for _, rel := range skipped {
				fmt.Fprintf(f, "/%s\n", rel)
			}
			f.Close()
		}
	}
	return skipped
}

// Snapshot commits the current working tree and stores the given conversation
// blob alongside it. label describes the pending tool call; prompt is the
// user prompt of the current run.
//
// Commits are parentless and pinned by refs/checkpoints/<hash>, so retention
// can drop any checkpoint independently. An unchanged tree mints no new
// commit: the list entry points at the previous commit and only the
// conversation blob is stored.
func (c *Checkpointer) Snapshot(tool, prompt string, conversation []byte) (Checkpoint, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.disabled {
		return Checkpoint{}, fmt.Errorf("checkpointing disabled")
	}
	if out, err := c.git("add", "-A", "."); err != nil {
		return Checkpoint{}, fmt.Errorf("checkpoint add: %v: %s", err, out)
	}
	list, _ := c.list()
	var prev *Checkpoint
	if len(list) > 0 {
		prev = &list[len(list)-1]
	}
	base := emptyTree
	if prev != nil {
		base = prev.ID
	}
	skipped := c.excludeOversized(base)

	tree, err := c.git("write-tree")
	if err != nil {
		return Checkpoint{}, fmt.Errorf("checkpoint write-tree: %v: %s", err, tree)
	}
	if len(prompt) > 200 {
		prompt = prompt[:200] + "…"
	}
	cp := Checkpoint{
		At:           time.Now(),
		Tool:         tool,
		Prompt:       prompt,
		Tree:         tree,
		SkippedLarge: skipped,
	}

	prevTree := ""
	if prev != nil {
		prevTree = prev.Tree
		if prevTree == "" {
			prevTree, _ = c.git("rev-parse", prev.ID+"^{tree}")
		}
	}
	if prev != nil && prevTree == tree {
		// No-change fast path: reuse the previous commit, store only the
		// conversation under a distinct key.
		cp.ID = prev.ID
		cp.Conv = fmt.Sprintf("%s-%d", prev.ID[:12], len(list))
	} else {
		msg := fmt.Sprintf("checkpoint before %s", tool)
		hash, err := c.git("commit-tree", tree, "-m", msg)
		if err != nil {
			return Checkpoint{}, fmt.Errorf("checkpoint commit: %v: %s", err, hash)
		}
		if out, err := c.git("update-ref", "refs/checkpoints/"+hash, hash); err != nil {
			return Checkpoint{}, fmt.Errorf("checkpoint ref: %v: %s", err, out)
		}
		if out, err := c.git("update-ref", "HEAD", hash); err != nil {
			return Checkpoint{}, fmt.Errorf("checkpoint head: %v: %s", err, out)
		}
		cp.ID = hash
		diffArgs := []string{"diff-tree", "--name-only", "-r", tree}
		if prevTree != "" {
			diffArgs = []string{"diff-tree", "--name-only", "-r", prevTree, tree}
		}
		if out, err := c.git(diffArgs...); err == nil && out != "" {
			cp.FilesChanged = len(strings.Split(out, "\n"))
		}
	}

	if len(conversation) > 0 {
		if err := os.WriteFile(c.convPath(cp.ConvKey()), conversation, 0o600); err != nil {
			return Checkpoint{}, err
		}
	}
	list = append(list, cp)
	if err := c.writeList(list); err != nil {
		return Checkpoint{}, err
	}
	if len(list)%c.opts.GCEvery == 0 {
		_, _ = c.git("gc", "--auto", "--quiet")
	}
	return cp, nil
}

// List returns all checkpoints, oldest first.
func (c *Checkpointer) List() ([]Checkpoint, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.list()
}

func (c *Checkpointer) list() ([]Checkpoint, error) {
	data, err := os.ReadFile(filepath.Join(c.dir, "checkpoints.json"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []Checkpoint
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *Checkpointer) writeList(list []Checkpoint) error {
	raw, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(c.dir, "checkpoints.json"), raw, 0o600)
}

func (c *Checkpointer) convPath(id string) string {
	return filepath.Join(c.dir, "conv", id+".json")
}

// Conversation returns the conversation blob stored with a checkpoint (keyed
// by Checkpoint.ConvKey()), or nil if none was recorded.
func (c *Checkpointer) Conversation(key string) ([]byte, error) {
	data, err := os.ReadFile(c.convPath(key))
	if os.IsNotExist(err) {
		return nil, nil
	}
	return data, err
}

// RestoreFiles resets the project working tree to the given checkpoint:
// tracked files are restored and files created after the checkpoint are
// removed (gitignored files are left alone).
func (c *Checkpointer) RestoreFiles(id string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.disabled {
		return fmt.Errorf("checkpointing disabled")
	}
	// With alternates, a user-side `git gc --prune` can delete borrowed
	// objects this checkpoint still needs; verify every object is present
	// and fail with a clear message instead of a raw git error mid-reset.
	if _, err := c.git("rev-list", "--objects", "--missing=error", id, "--"); err != nil {
		return fmt.Errorf("checkpoint %s is no longer restorable (its objects were pruned, possibly by git gc in the project repo)", id[:min(12, len(id))])
	}
	if out, err := c.git("reset", "--quiet", "--hard", id); err != nil {
		return fmt.Errorf("restore reset: %v: %s", err, out)
	}
	if out, err := c.git("clean", "-qfd"); err != nil {
		return fmt.Errorf("restore clean: %v: %s", err, out)
	}
	return nil
}

// enforceRetention drops checkpoints older than RetentionDays (and, if the
// store still exceeds MaxGB, the oldest half of what remains), deletes their
// conversation blobs and pinning refs, and lets gc collect the objects.
// Runs on Open only, keeping the per-snapshot path fast.
func (c *Checkpointer) enforceRetention() {
	list, err := c.list()
	if err != nil || len(list) == 0 {
		return
	}
	horizon := time.Now().AddDate(0, 0, -c.opts.RetentionDays)
	cut := 0
	for cut < len(list) && list[cut].At.Before(horizon) {
		cut++
	}
	if cut == 0 && dirSize(c.dir) > int64(c.opts.MaxGB)<<30 {
		cut = len(list) / 2
	}
	if cut == 0 {
		return
	}
	dropped, kept := list[:cut], list[cut:]
	keptIDs := make(map[string]bool, len(kept))
	for _, cp := range kept {
		keptIDs[cp.ID] = true
	}
	for _, cp := range dropped {
		_ = os.Remove(c.convPath(cp.ConvKey()))
		if !keptIDs[cp.ID] {
			_, _ = c.git("update-ref", "-d", "refs/checkpoints/"+cp.ID)
		}
	}
	if err := c.writeList(kept); err != nil {
		return
	}
	// Clear any reflogs from repos created before logging was pinned off,
	// then collect the now-unreachable checkpoint objects.
	_, _ = c.git("reflog", "expire", "--expire=now", "--all")
	_, _ = c.git("gc", "--quiet", "--prune=now")
}

// ChangesSince returns how many files differ between checkpoint id and the
// current working tree (including files created since, honouring ignores).
// The /rewind picker uses it to show what restoring the latest checkpoint
// would actually undo.
func (c *Checkpointer) ChangesSince(id string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.disabled {
		return 0
	}
	// Same staging step as Snapshot: refresh the shadow index to the live
	// tree, then diff it against the checkpoint's commit.
	if _, err := c.git("add", "-A", "."); err != nil {
		return 0
	}
	out, err := c.git("diff", "--cached", "--name-only", id)
	if err != nil || out == "" {
		return 0
	}
	return len(strings.Split(out, "\n"))
}

// Size returns the disk usage in bytes of this project's checkpoint store
// (shadow repo, conversation blobs and index).
func (c *Checkpointer) Size() int64 { return dirSize(c.dir) }

// TotalSize returns the disk usage in bytes of all checkpoint stores under
// globalDir (every project's history).
func TotalSize(globalDir string) int64 {
	return dirSize(filepath.Join(globalDir, "history"))
}

func dirSize(root string) int64 {
	var total int64
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if info, err := d.Info(); err == nil {
			total += info.Size()
		}
		return nil
	})
	return total
}
