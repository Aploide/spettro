package agent

import (
	"crypto/sha256"
	"fmt"
	"os"
	"sync"
)

// File stamps back the stale-read guard. A stamp is the SHA-256 of a file's
// content the last time the agent saw all of it: a file-read (ranged or not)
// or one of its own writes. Search hits (grep, repo-search) only put a path in
// readSet and never stamp it, so they don't license overwriting the file.
// A hash rather than an mtime, so a touch or a same-content rewrite doesn't
// force a pointless re-read.

// recordFileStamp remembers content as rel's last-seen state.
func (r *toolRuntime) recordFileStamp(rel string, content []byte) {
	sum := sha256.Sum256(content)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fileStamps == nil {
		r.fileStamps = map[string][32]byte{}
	}
	r.fileStamps[rel] = sum
}

// stampFromDisk stamps rel with whatever is on disk now, for tools that wrote
// the file without holding its content (downloads, generated media).
func (r *toolRuntime) stampFromDisk(rel, abs string) {
	if data, err := os.ReadFile(abs); err == nil {
		r.recordFileStamp(rel, data)
	}
}

// hasFileStamp reports whether the agent has seen rel in full.
func (r *toolRuntime) hasFileStamp(rel string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.fileStamps[rel]
	return ok
}

// checkFileStamp rejects a write when rel has a stamp and its current content
// differs from it: the user, a formatter, a shell command or another agent
// changed the file after the agent last saw it. A file the agent never read
// has no stamp and passes; edits then rely on old_string matching.
func (r *toolRuntime) checkFileStamp(tool, rel string, current []byte) error {
	r.mu.Lock()
	sum, ok := r.fileStamps[rel]
	r.mu.Unlock()
	if !ok || sum == sha256.Sum256(current) {
		return nil
	}
	return fmt.Errorf("%s: %s was modified on disk since you last read it (by the user, a formatter, a shell command or another agent); file-read it again, then redo the change against the current content", tool, rel)
}

// lockFile serializes the agent's read-modify-write tools on one file, so two
// edits in the same batch can't both start from the old content and lose one.
func (r *toolRuntime) lockFile(abs string) func() {
	r.mu.Lock()
	if r.fileLocks == nil {
		r.fileLocks = map[string]*sync.Mutex{}
	}
	l, ok := r.fileLocks[abs]
	if !ok {
		l = &sync.Mutex{}
		r.fileLocks[abs] = l
	}
	r.mu.Unlock()
	l.Lock()
	return l.Unlock
}
