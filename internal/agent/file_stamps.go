package agent

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"spettro/internal/diff"
	"spettro/internal/provider"
)

// File stamps back the stale-read guard. A stamp is the SHA-256 of a file's
// content the last time the agent saw all of it: a file-read (ranged or not)
// or one of its own writes. Search hits (grep, repo-search) only put a path in
// readSet and never stamp it, so they don't license overwriting the file.
// A hash rather than an mtime, so a touch or a same-content rewrite doesn't
// force a pointless re-read.
//
// Stamps are keyed by the file's real path (symlinks resolved), so reaching
// one file through a link and through its real path is one file to the guard.
// They are conversation state, not run state: every step's stamp changes ride
// on its tool-results message (provider.Message.FileStamps) and a run
// restores them from the messages it carries, so a file read in an earlier
// turn still counts as read, and an edit the user made between turns is still
// caught.

// stampKey is the key rel's stamps are stored under: its real path.
func (r *toolRuntime) stampKey(rel string) string {
	return resolveExistingPath(filepath.Join(r.cwd, filepath.FromSlash(rel)))
}

// resolveExistingPath resolves symlinks on the longest existing prefix of abs
// and re-appends the missing tail, so a file about to be created gets the key
// it will have once it exists.
func resolveExistingPath(abs string) string {
	real, rem := filepath.Clean(abs), ""
	for {
		if resolved, err := filepath.EvalSymlinks(real); err == nil {
			if rem == "" {
				return resolved
			}
			return filepath.Join(resolved, rem)
		}
		parent := filepath.Dir(real)
		if parent == real {
			return filepath.Clean(abs)
		}
		rem = filepath.Join(filepath.Base(real), rem)
		real = parent
	}
}

func (r *toolRuntime) setStampLocked(key string, sum [32]byte, read bool) {
	if r.fileStamps == nil {
		r.fileStamps = map[string][32]byte{}
	}
	r.fileStamps[key] = sum
	if read {
		if r.readStamps == nil {
			r.readStamps = map[string][32]byte{}
		}
		r.readStamps[key] = sum
	}
	if r.stampsChanged == nil {
		r.stampsChanged = map[string]struct{}{}
	}
	r.stampsChanged[key] = struct{}{}
}

// recordFileStamp remembers content as rel's last-seen state.
func (r *toolRuntime) recordFileStamp(rel string, content []byte) {
	key := r.stampKey(rel)
	sum := sha256.Sum256(content)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.setStampLocked(key, sum, false)
}

// recordReadStamp stamps rel after a file-read and remembers that content as
// the one whose line numbers the model was shown.
func (r *toolRuntime) recordReadStamp(rel string, content []byte) {
	r.recordReadStampSum(rel, sha256.Sum256(content))
}

// recordReadStampSum is recordReadStamp for a hash computed while streaming.
func (r *toolRuntime) recordReadStampSum(rel string, sum [32]byte) {
	key := r.stampKey(rel)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.setStampLocked(key, sum, true)
}

// unchangedSinceRead reports whether content is exactly what the last
// file-read of rel returned, so line numbers from that read still hold.
func (r *toolRuntime) unchangedSinceRead(rel string, content []byte) bool {
	key := r.stampKey(rel)
	r.mu.Lock()
	sum, ok := r.readStamps[key]
	r.mu.Unlock()
	return ok && sum == sha256.Sum256(content)
}

// stampFromDisk stamps rel with whatever is on disk now, for tools that wrote
// the file without holding its content (downloads, generated media).
func (r *toolRuntime) stampFromDisk(rel, abs string) {
	if data, err := os.ReadFile(abs); err == nil {
		r.recordFileStamp(rel, data)
	}
}

// stampMatches reports whether rel is stamped with exactly content.
func (r *toolRuntime) stampMatches(rel string, content []byte) bool {
	key := r.stampKey(rel)
	r.mu.Lock()
	sum, ok := r.fileStamps[key]
	r.mu.Unlock()
	return ok && sum == sha256.Sum256(content)
}

// hasFileStamp reports whether the agent has seen rel in full.
func (r *toolRuntime) hasFileStamp(rel string) bool {
	key := r.stampKey(rel)
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.fileStamps[key]
	return ok
}

// checkFileStamp rejects a write when rel has a stamp and its current content
// differs from it: the user, a formatter, a shell command or another agent
// changed the file after the agent last saw it. A file the agent never read
// has no stamp and passes; edits then rely on old_string matching.
func (r *toolRuntime) checkFileStamp(tool, rel string, current []byte) error {
	key := r.stampKey(rel)
	r.mu.Lock()
	sum, ok := r.fileStamps[key]
	r.mu.Unlock()
	if !ok || sum == sha256.Sum256(current) {
		return nil
	}
	return staleReadError(tool, rel)
}

func staleReadError(tool, rel string) error {
	return fmt.Errorf("%s: %s was modified on disk since you last read it (by the user, a formatter, a shell command or another agent); file-read it again, then redo the change against the current content", tool, rel)
}

// recheckBeforeWrite re-reads abs after a write was approved and refuses the
// write when the file no longer holds what the change was computed from
// (existed=false: it did not exist). An approval prompt can stay open for a
// long time, and the user may edit the file in their editor meanwhile;
// writing content computed from the old bytes would silently drop that edit.
func (r *toolRuntime) recheckBeforeWrite(tool, rel, abs string, existed bool, before []byte) error {
	now, err := os.ReadFile(abs)
	switch {
	case err != nil && !existed && os.IsNotExist(err):
		return nil
	case err != nil:
		if os.IsNotExist(err) {
			return fmt.Errorf("%s: %s was deleted while the change was waiting for approval; nothing was written", tool, rel)
		}
		return err
	case !existed || !bytes.Equal(now, before):
		return fmt.Errorf("%s: %s was modified on disk while the change was waiting for approval (by the user, a formatter or another process); nothing was written — file-read it again, then redo the change against the current content", tool, rel)
	}
	return nil
}

// takeStampDelta returns the stamps changed since the last call, for the
// tool-results message that carries them into the conversation.
func (r *toolRuntime) takeStampDelta() []provider.FileStamp {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.stampsChanged) == 0 {
		return nil
	}
	keys := make([]string, 0, len(r.stampsChanged))
	for k := range r.stampsChanged {
		keys = append(keys, k)
	}
	r.stampsChanged = nil
	return r.stampRecordsLocked(keys)
}

// stampSnapshot returns every stamp, for a message that replaces the
// messages whose deltas carried them (a compaction summary).
func (r *toolRuntime) stampSnapshot() []provider.FileStamp {
	r.mu.Lock()
	defer r.mu.Unlock()
	keys := make([]string, 0, len(r.fileStamps))
	for k := range r.fileStamps {
		keys = append(keys, k)
	}
	return r.stampRecordsLocked(keys)
}

func (r *toolRuntime) stampRecordsLocked(keys []string) []provider.FileStamp {
	sort.Strings(keys)
	out := make([]provider.FileStamp, 0, len(keys))
	for _, k := range keys {
		fs := provider.FileStamp{Path: k}
		if sum, ok := r.fileStamps[k]; ok {
			fs.Seen = hex.EncodeToString(sum[:])
		}
		if sum, ok := r.readStamps[k]; ok {
			fs.Read = hex.EncodeToString(sum[:])
		}
		out = append(out, fs)
	}
	return out
}

// restoreStamps rebuilds the stamps a conversation's earlier runs recorded,
// replaying each message's records in order.
func (r *toolRuntime) restoreStamps(msgs []provider.Message) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, m := range msgs {
		for _, fs := range m.FileStamps {
			if seen, ok := decodeStampSum(fs.Seen); ok {
				if r.fileStamps == nil {
					r.fileStamps = map[string][32]byte{}
				}
				r.fileStamps[fs.Path] = seen
			}
			if read, ok := decodeStampSum(fs.Read); ok {
				if r.readStamps == nil {
					r.readStamps = map[string][32]byte{}
				}
				r.readStamps[fs.Path] = read
			}
		}
	}
}

func decodeStampSum(h string) ([32]byte, bool) {
	var sum [32]byte
	b, err := hex.DecodeString(h)
	if err != nil || len(b) != len(sum) {
		return sum, false
	}
	copy(sum[:], b)
	return sum, true
}

// lockFile serializes the agent's read-modify-write tools on one file, so two
// edits in the same batch can't both start from the old content and lose one.
// Locks are keyed by the real path, like stamps.
func (r *toolRuntime) lockFile(abs string) func() {
	key := resolveExistingPath(abs)
	r.mu.Lock()
	if r.fileLocks == nil {
		r.fileLocks = map[string]*sync.Mutex{}
	}
	l, ok := r.fileLocks[key]
	if !ok {
		l = &sync.Mutex{}
		r.fileLocks[key] = l
	}
	r.mu.Unlock()
	l.Lock()
	return l.Unlock
}

// Bounds for the diff echoed back after an edit.
const (
	editDiffMaxLines = 40
	editDiffMaxCols  = 200
)

// editDiffSummary is the compact diff returned to the model after an edit so
// it can confirm what changed without re-reading the file: hunks with three
// lines of context and a +added/-removed count, capped in size.
func editDiffSummary(rel, before, after string) string {
	d := diff.Unified(rel, strings.ReplaceAll(before, "\r\n", "\n"), strings.ReplaceAll(after, "\r\n", "\n"))
	if d == "" {
		return "no changes (new_string produced identical content)"
	}
	// Drop the "--- a/..." and "+++ b/..." header, and only it: a removed
	// "-- comment" line (SQL, Lua) also renders as "--- comment".
	lines := strings.Split(strings.TrimRight(d, "\n"), "\n")
	lines = lines[min(2, len(lines)):]
	if len(lines) > 0 && !strings.HasPrefix(lines[0], "@@") {
		// diff.Unified's one-line summary for oversized files.
		return "diff not shown: " + strings.Trim(lines[0], "()")
	}
	var body []string
	adds, dels := 0, 0
	for _, l := range lines {
		switch {
		case strings.HasPrefix(l, "+"):
			adds++
		case strings.HasPrefix(l, "-"):
			dels++
		}
		body = append(body, clipRunes(l, editDiffMaxCols))
	}
	if len(body) > editDiffMaxLines {
		more := len(body) - editDiffMaxLines
		body = append(body[:editDiffMaxLines], fmt.Sprintf("... (%d more diff lines)", more))
	}
	return fmt.Sprintf("diff (+%d -%d lines):\n%s", adds, dels, strings.Join(body, "\n"))
}
