// Package checkpoint snapshots the project working tree into a shadow git
// repository before any file-modifying tool runs, so the user can rewind
// files and/or conversation to any earlier step via /rewind.
//
// The shadow repository lives under Spettro's data dir
// (~/.spettro/history/<project_hash>/repo.git) and is completely separate
// from the user's own .git. Because commits are made with the project as the
// work tree, the project's .gitignore files are honoured and nested .git
// directories are never tracked.
package checkpoint

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Checkpoint is one snapshot: a shadow commit plus the conversation state
// that was current when the pending tool call triggered it.
type Checkpoint struct {
	ID           string    `json:"id"` // shadow-repo commit hash
	At           time.Time `json:"at"`
	Tool         string    `json:"tool"`    // tool call that triggered the snapshot
	Prompt       string    `json:"prompt"`  // user prompt of the run, truncated
	FilesChanged int       `json:"files_changed"`
}

type Checkpointer struct {
	mu       sync.Mutex
	project  string // user's project root (work tree)
	dir      string // ~/.spettro/history/<hash>
	gitDir   string // <dir>/repo.git
	disabled bool   // git missing or init failed; snapshots become no-ops
}

// Dir returns the per-project history directory under globalDir.
func Dir(globalDir, projectPath string) string {
	sum := sha256.Sum256([]byte(projectPath))
	return filepath.Join(globalDir, "history", fmt.Sprintf("%x", sum[:8]))
}

// Open prepares (creating if needed) the shadow repository for projectPath.
func Open(globalDir, projectPath string) (*Checkpointer, error) {
	dir := Dir(globalDir, projectPath)
	c := &Checkpointer{
		project: projectPath,
		dir:     dir,
		gitDir:  filepath.Join(dir, "repo.git"),
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
	}
	return c, nil
}

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

// Snapshot commits the current working tree and stores the given conversation
// blob alongside it. label describes the pending tool call; prompt is the
// user prompt of the current run.
func (c *Checkpointer) Snapshot(tool, prompt string, conversation []byte) (Checkpoint, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.disabled {
		return Checkpoint{}, fmt.Errorf("checkpointing disabled")
	}
	if out, err := c.git("add", "-A", "."); err != nil {
		return Checkpoint{}, fmt.Errorf("checkpoint add: %v: %s", err, out)
	}
	msg := fmt.Sprintf("checkpoint before %s", tool)
	if out, err := c.git("commit", "--quiet", "--allow-empty", "--no-verify", "-m", msg); err != nil {
		return Checkpoint{}, fmt.Errorf("checkpoint commit: %v: %s", err, out)
	}
	hash, err := c.git("rev-parse", "HEAD")
	if err != nil {
		return Checkpoint{}, fmt.Errorf("checkpoint rev-parse: %v", err)
	}
	changed := 0
	if out, err := c.git("diff-tree", "--root", "--no-commit-id", "--name-only", "-r", hash); err == nil && out != "" {
		changed = len(strings.Split(out, "\n"))
	}
	if len(prompt) > 200 {
		prompt = prompt[:200] + "…"
	}
	cp := Checkpoint{
		ID:           hash,
		At:           time.Now(),
		Tool:         tool,
		Prompt:       prompt,
		FilesChanged: changed,
	}
	if len(conversation) > 0 {
		if err := os.WriteFile(c.convPath(hash), conversation, 0o600); err != nil {
			return Checkpoint{}, err
		}
	}
	list, _ := c.list()
	list = append(list, cp)
	if err := c.writeList(list); err != nil {
		return Checkpoint{}, err
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

// Conversation returns the conversation blob stored with a checkpoint, or nil
// if none was recorded.
func (c *Checkpointer) Conversation(id string) ([]byte, error) {
	data, err := os.ReadFile(c.convPath(id))
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
	if out, err := c.git("reset", "--quiet", "--hard", id); err != nil {
		return fmt.Errorf("restore reset: %v: %s", err, out)
	}
	if out, err := c.git("clean", "-qfd"); err != nil {
		return fmt.Errorf("restore clean: %v: %s", err, out)
	}
	return nil
}
