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
// or one of its own writes. Search hits (grep, including its symbol form) only put a path in
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
//
// The agent's own shell commands. A formatter, code generator or sed the
// agent runs through a foreground bash call changes files it has stamped,
// and the guard used to refuse its next edit of them as "modified on disk"
// although nobody else touched them. Around each foreground bash command
// (runShellTool) the guard now re-stamps the files that command changed:
//
//   - Every stamp recorded by a read or write also records the file's
//     identity then (size, mtime, inode; stampIDs): a file-read takes it from
//     the open file before reading, so it is never newer than the content;
//     a write takes it right after writing, under the file's lock.
//   - Just before the command starts, snapshotStampsForShell stats the
//     stamped files. Only a file whose identity still equals the recorded one
//     is eligible: one that already differs was changed from outside since
//     the agent saw it, and the guard must keep firing for it. A stamp
//     restored from an earlier turn has no identity; its content is hashed
//     once (bounded by maxShellVerifyFiles and maxShellVerifyBytes) and, when
//     it still matches, gets one.
//   - Right after the command, restampAfterShell stats the eligible files
//     again and re-stamps each whose identity changed, with its new content.
//     The read stamp is left alone: the model has not seen the new lines, so
//     line numbers from its last file-read no longer hold.
//
// The work is a stat of the stamped set (at most maxShellStampFiles), never
// a tree walk, and re-reading is bounded by maxShellRestampBytes; a file
// past a bound simply keeps its old stamp, which errs on the side of the
// guard. Background jobs are not covered: they outlive the call, so their
// changes cannot be told apart from anyone else's. What the rule cannot
// tell apart is a change another process makes to an eligible file while
// the command runs: it is taken for the command's own. Outside that window
// (between calls, or while an approval prompt is open, which is before the
// snapshot) every outside change is still caught.

// Bounds on the work around one foreground shell command (see above).
const (
	maxShellStampFiles   = 1024
	maxShellVerifyFiles  = 32
	maxShellVerifyBytes  = 8 << 20
	maxShellRestampBytes = 32 << 20
)

// fileIdentity is the cheap fingerprint the shell re-stamp compares: size,
// modification time and inode (0 where the platform has none).
type fileIdentity struct {
	size  int64
	mtime int64
	inode uint64
}

// identityOf returns fi's identity; false for a missing or non-regular file.
func identityOf(fi os.FileInfo) (fileIdentity, bool) {
	if fi == nil || !fi.Mode().IsRegular() {
		return fileIdentity{}, false
	}
	return fileIdentity{size: fi.Size(), mtime: fi.ModTime().UnixNano(), inode: fileInode(fi)}, true
}

// statIdentity stats path and returns its identity.
func statIdentity(path string) (fileIdentity, bool) {
	fi, err := os.Stat(path)
	if err != nil {
		return fileIdentity{}, false
	}
	return identityOf(fi)
}

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

// setStampLocked records sum as key's stamp (and, with read, its read
// stamp). hasID says whether id is the file's identity when it held exactly
// that content; without one any identity recorded earlier is dropped, since
// it described other content.
func (r *toolRuntime) setStampLocked(key string, sum [32]byte, read bool, id fileIdentity, hasID bool) {
	if r.fileStamps == nil {
		r.fileStamps = map[string][32]byte{}
	}
	r.fileStamps[key] = sum
	if hasID {
		if r.stampIDs == nil {
			r.stampIDs = map[string]fileIdentity{}
		}
		r.stampIDs[key] = id
	} else {
		delete(r.stampIDs, key)
	}
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

// recordFileStamp remembers content as rel's last-seen state. Callers have
// just written or read content under the file's lock, so the identity is
// taken now, after the content.
func (r *toolRuntime) recordFileStamp(rel string, content []byte) {
	key := r.stampKey(rel)
	sum := sha256.Sum256(content)
	id, hasID := statIdentity(key)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.setStampLocked(key, sum, false, id, hasID)
}

// recordReadStamp stamps rel after a file-read and remembers that content as
// the one whose line numbers the model was shown.
func (r *toolRuntime) recordReadStamp(rel string, content []byte) {
	key := r.stampKey(rel)
	fi, _ := os.Stat(key)
	r.recordReadStampSum(rel, sha256.Sum256(content), fi)
}

// recordReadStampSum is recordReadStamp for a hash computed while streaming.
// opened is the file's stat from before the content was read (nil when
// unknown), so the identity recorded is never newer than the content: if
// the file changed during the read, the identity no longer matches and the
// shell re-stamp leaves the file to the guard.
func (r *toolRuntime) recordReadStampSum(rel string, sum [32]byte, opened os.FileInfo) {
	key := r.stampKey(rel)
	id, hasID := identityOf(opened)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.setStampLocked(key, sum, true, id, hasID)
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
// differs from it: the user, another process, a background job or another
// agent changed the file after the agent last saw it (the agent's own
// foreground shell commands re-stamp what they change; see the top of this
// file). A file the agent never read has no stamp and passes; edits then
// rely on old_string matching.
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

// shellStamp is one stamped file a foreground shell command may re-stamp:
// its stamp and identity when the command started.
type shellStamp struct {
	key    string
	sum    [32]byte
	before fileIdentity
}

// snapshotStampsForShell returns the stamped files whose content is known to
// still match their stamp, with their identity now, for restampAfterShell
// once the command has run. A file changed since the agent saw it is left
// out, so its stale stamp keeps guarding it.
func (r *toolRuntime) snapshotStampsForShell() []shellStamp {
	type stamped struct {
		key   string
		sum   [32]byte
		id    fileIdentity
		hasID bool
	}
	r.mu.Lock()
	entries := make([]stamped, 0, min(len(r.fileStamps), maxShellStampFiles))
	for key, sum := range r.fileStamps {
		if len(entries) == maxShellStampFiles {
			break
		}
		id, hasID := r.stampIDs[key]
		entries = append(entries, stamped{key, sum, id, hasID})
	}
	r.mu.Unlock()

	out := make([]shellStamp, 0, len(entries))
	verifiedFiles, verifiedBytes := 0, int64(0)
	for _, e := range entries {
		now, ok := statIdentity(e.key)
		switch {
		case !ok:
			continue
		case e.hasID:
			if now == e.id {
				out = append(out, shellStamp{key: e.key, sum: e.sum, before: now})
			}
			continue
		case verifiedFiles >= maxShellVerifyFiles || verifiedBytes+now.size > maxShellVerifyBytes:
			continue
		}
		// A stamp restored from an earlier turn: trust it only once its
		// content is confirmed, then remember the identity so later
		// commands need only a stat.
		verifiedFiles++
		verifiedBytes += now.size
		data, err := os.ReadFile(e.key)
		if err != nil || sha256.Sum256(data) != e.sum {
			continue
		}
		r.mu.Lock()
		if cur, ok := r.fileStamps[e.key]; ok && cur == e.sum {
			if r.stampIDs == nil {
				r.stampIDs = map[string]fileIdentity{}
			}
			r.stampIDs[e.key] = now
		}
		r.mu.Unlock()
		out = append(out, shellStamp{key: e.key, sum: e.sum, before: now})
	}
	return out
}

// restampAfterShell re-stamps the files of snap that the shell command just
// run changed (their identity differs from the snapshot's), and returns
// their paths for the note on the command's output. A file deleted by the
// command keeps its stamp; one whose stamp something else replaced while
// the command ran (a concurrent write tool) is left to that stamp.
func (r *toolRuntime) restampAfterShell(snap []shellStamp) []string {
	var changed []string
	budget := int64(maxShellRestampBytes)
	for _, s := range snap {
		now, ok := statIdentity(s.key)
		if !ok || now == s.before || now.size > budget {
			continue
		}
		budget -= now.size
		// now was taken before this read, so if the file changes again
		// meanwhile the recorded identity is the older one and the next
		// command leaves the file to the guard.
		data, err := os.ReadFile(s.key)
		if err != nil {
			continue
		}
		sum := sha256.Sum256(data)
		r.mu.Lock()
		cur, stamped := r.fileStamps[s.key]
		if stamped && cur == s.sum {
			if sum == s.sum {
				// Same content (a touch, a no-op format): only the
				// identity moved.
				if r.stampIDs != nil {
					r.stampIDs[s.key] = now
				}
			} else {
				r.setStampLocked(s.key, sum, false, now, true)
				changed = append(changed, s.key)
			}
		}
		r.mu.Unlock()
	}
	sort.Strings(changed)
	return changed
}

// maxRestampNotePaths caps the paths the shell re-stamp note lists.
const maxRestampNotePaths = 5

// restampNote tells the model which files it had read that its command just
// changed: they are no longer what it saw, so line numbers and old_string
// text from its last read may not hold. keys are stamp keys (real paths),
// shown relative to the workspace when inside it.
func (r *toolRuntime) restampNote(keys []string) string {
	if len(keys) == 0 {
		return ""
	}
	root := resolveExistingPath(r.cwd)
	shown := make([]string, 0, min(len(keys), maxRestampNotePaths))
	for _, key := range keys[:min(len(keys), maxRestampNotePaths)] {
		if rel, err := filepath.Rel(root, key); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			key = filepath.ToSlash(rel)
		}
		shown = append(shown, key)
	}
	list := strings.Join(shown, ", ")
	if more := len(keys) - len(shown); more > 0 {
		list += fmt.Sprintf(" and %d more", more)
	}
	return fmt.Sprintf("note: this command changed files you had read (%s); file-read them before quoting them in old_string", list)
}

func staleReadError(tool, rel string) error {
	return fmt.Errorf("%s: %s was modified on disk since you last read it (by the user, another process, a background job or another agent); file-read it again, then redo the change against the current content", tool, rel)
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
				// A restored stamp has no identity: the shell re-stamp
				// verifies its content before trusting one.
				delete(r.stampIDs, fs.Path)
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
