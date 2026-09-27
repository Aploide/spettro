package checkpoint

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"time"
)

// checkpointsFile is the on-disk format of checkpoints.json. The project path
// is recorded alongside the list so storage cleanup can detect orphaned
// history dirs (the dir name is sha256(projectPath) — one-way).
type checkpointsFile struct {
	ProjectPath string       `json:"project_path"`
	Checkpoints []Checkpoint `json:"checkpoints"`
}

// listCache caches the parsed checkpoints.json, which every snapshot reads
// (for the previous checkpoint) and rewrites.
//
//   - What: the checkpoint list as this process last read or wrote it.
//   - Key: the file's size and modification time at that moment.
//   - Invalidation: list stats the file on every call and re-reads it when
//     the size or time differ (another Spettro process on the same project
//     wrote it) or the file is gone; writeList stores what it wrote.
//   - Owner: the Checkpointer; every access holds Checkpointer.mu.
type listCache struct {
	valid bool
	size  int64
	mtime time.Time
	list  []Checkpoint
}

func (c *Checkpointer) listPath() string {
	return filepath.Join(c.dir, "checkpoints.json")
}

// List returns all checkpoints, oldest first.
func (c *Checkpointer) List() ([]Checkpoint, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	list, err := c.list()
	return slices.Clone(list), err
}

// list returns the checkpoints, oldest first. The result shares its elements
// with the cache: callers must not modify them. Its capacity is clipped, so
// appending to it copies.
func (c *Checkpointer) list() ([]Checkpoint, error) {
	st, err := os.Stat(c.listPath())
	if err != nil {
		c.listCache = listCache{}
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	if c.listCache.valid && c.listCache.size == st.Size() && c.listCache.mtime.Equal(st.ModTime()) {
		return slices.Clip(c.listCache.list), nil
	}
	data, err := os.ReadFile(c.listPath())
	if err != nil {
		c.listCache = listCache{}
		return nil, err
	}
	list, err := parseCheckpointsFile(data)
	if err != nil {
		c.listCache = listCache{}
		return nil, err
	}
	c.listCache = listCache{valid: true, size: st.Size(), mtime: st.ModTime(), list: list}
	return slices.Clip(list), nil
}

// parseCheckpointsFile decodes checkpoints.json, current or legacy format.
func parseCheckpointsFile(data []byte) ([]Checkpoint, error) {
	var file checkpointsFile
	if err := json.Unmarshal(data, &file); err != nil {
		// Legacy format: a bare array without project_path metadata.
		var out []Checkpoint
		if err := json.Unmarshal(data, &out); err != nil {
			return nil, err
		}
		return out, nil
	}
	return file.Checkpoints, nil
}

func (c *Checkpointer) writeList(list []Checkpoint) error {
	raw, err := json.MarshalIndent(checkpointsFile{
		ProjectPath: c.project,
		Checkpoints: list,
	}, "", "  ")
	if err != nil {
		return err
	}
	c.listCache = listCache{}
	if err := os.WriteFile(c.listPath(), raw, 0o600); err != nil {
		return err
	}
	// Cache what was written, keyed by the file's state right after the
	// write. A size mismatch means another process wrote in between: leave
	// the cache empty so the next list reads its version.
	if st, err := os.Stat(c.listPath()); err == nil && st.Size() == int64(len(raw)) {
		c.listCache = listCache{valid: true, size: st.Size(), mtime: st.ModTime(), list: slices.Clip(list)}
	}
	return nil
}

// lastID returns the ID of the newest checkpoint in list, "" when empty.
func lastID(list []Checkpoint) string {
	if len(list) == 0 {
		return ""
	}
	return list[len(list)-1].ID
}
