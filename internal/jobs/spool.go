package jobs

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// SpoolStore persists oversized tool outputs to disk so the agent runtime can
// return a truncated head to the model and let it page through the rest with
// job-output using "spool:<n>" IDs. Spool files are session state, like jobs:
// they live in a session-scoped directory and are removed on session end.
//
// Writes happen off the caller's path. The agent spools every tool result
// over 2 KB right after the tool ran, and a synchronous file write there
// cost 0.1 ms or more per result on the step's critical path. So Add only
// assigns the ID and the file path, queues the content and returns; a
// writer goroutine writes the files. Outputs are immutable strings, so the
// queue holds the caller's string without copying it.
//
// Ordering guarantees:
//   - Files are written in Add order: at most one writer goroutine runs at
//     a time, draining a FIFO queue, and it exits when the queue is empty.
//   - Read never sees a partial file: until an entry's write has finished,
//     Read serves the queued content from memory.
//   - When Path returns a path, the file is complete on disk (Path waits
//     for that entry's write).
//   - Flush returns once every write queued before the call has finished;
//     Remove and Cleanup flush the entries they delete first, so a late
//     write can never recreate a removed file.
//
// A write that fails (disk full, directory removed) keeps the content in
// memory for the rest of the session: Read still serves it, Path returns ""
// because there is no file to name. Queued writes are lost if the process
// exits without Cleanup, which is harmless: the spool directory is
// temporary session state.
type SpoolStore struct {
	mu    sync.Mutex
	dir   string
	seq   int
	files map[string]string // spool ID -> file path

	// pending holds the entries whose write has not finished (queued or in
	// progress), queue the ones not started yet, oldest first.
	pending map[string]*spoolWrite
	queue   []*spoolWrite
	// writing is set while a writer goroutine is draining queue.
	writing bool
	// failed holds the content of entries whose write failed.
	failed map[string]string
}

// spoolWrite is one queued spool file.
type spoolWrite struct {
	id, path, content string
	// done is closed once the write has finished, successfully or not.
	done chan struct{}
}

// NewSpoolStore returns an empty store; its directory is created on the
// first Add.
func NewSpoolStore() *SpoolStore {
	return &SpoolStore{files: map[string]string{}, pending: map[string]*spoolWrite{}, failed: map[string]string{}}
}

var defaultSpool = NewSpoolStore()

// Spool returns the process-wide spool store; the spettro process is one
// session, so session-scoped spools live here.
func Spool() *SpoolStore { return defaultSpool }

// Add queues content for a new spool file and returns its ID ("spool:<n>").
// The file is written in the background (see SpoolStore); the only error is
// failing to create the spool directory.
func (s *SpoolStore) Add(content string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dir == "" {
		dir, err := os.MkdirTemp("", "spettro-spool-*")
		if err != nil {
			return "", fmt.Errorf("create spool dir: %w", err)
		}
		s.dir = dir
	}
	s.seq++
	id := fmt.Sprintf("spool:%d", s.seq)
	path := filepath.Join(s.dir, fmt.Sprintf("%d.txt", s.seq))
	s.files[id] = path
	w := &spoolWrite{id: id, path: path, content: content, done: make(chan struct{})}
	s.pending[id] = w
	s.queue = append(s.queue, w)
	if !s.writing {
		s.writing = true
		go s.drain()
	}
	return id, nil
}

// drain is the writer goroutine: it writes queued files in order and exits
// when the queue is empty.
func (s *SpoolStore) drain() {
	for {
		s.mu.Lock()
		if len(s.queue) == 0 {
			s.writing = false
			s.mu.Unlock()
			return
		}
		w := s.queue[0]
		s.queue[0] = nil
		s.queue = s.queue[1:]
		s.mu.Unlock()

		err := writeSpoolFile(w.path, w.content)

		// Only this goroutine removes entries from pending (Remove and
		// Cleanup wait for them instead), so w is still there.
		s.mu.Lock()
		delete(s.pending, w.id)
		if err != nil {
			s.failed[w.id] = w.content
		}
		s.mu.Unlock()
		close(w.done)
	}
}

// writeSpoolFile writes content to a new file at path.
func writeSpoolFile(path, content string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	// WriteString, unlike os.WriteFile, needs no []byte copy of the content.
	_, err = f.WriteString(content)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// wait blocks until the writes of the given entries have finished. It must
// be called without s.mu held.
func wait(writes []*spoolWrite) {
	for _, w := range writes {
		<-w.done
	}
}

// Path returns the file backing a spool ID, or "" for an unknown ID or one
// whose write failed. Tool output footers name it so the model can also
// search the full output with ordinary shell tools (grep, tail) instead of
// paging through it. It waits for the entry's pending write.
func (s *SpoolStore) Path(id string) string {
	id = strings.TrimSpace(id)
	s.mu.Lock()
	w := s.pending[id]
	s.mu.Unlock()
	if w != nil {
		<-w.done
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, failed := s.failed[id]; failed {
		return ""
	}
	return s.files[id]
}

// Dir returns the spool directory of this store, or "" when nothing has been
// spooled yet. Storage cleanup uses it to exempt the live session's spool.
func (s *SpoolStore) Dir() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dir
}

// Read returns up to max bytes of the spool starting at absolute byte offset,
// the next offset to read from, and the total spool size. max <= 0 means no
// per-read cap.
func (s *SpoolStore) Read(id string, offset, max int) (chunk string, next, size int, err error) {
	id = strings.TrimSpace(id)
	s.mu.Lock()
	path, ok := s.files[id]
	var inMemory string
	var fromMemory bool
	if w := s.pending[id]; w != nil {
		inMemory, fromMemory = w.content, true
	} else if content, failed := s.failed[id]; failed {
		inMemory, fromMemory = content, true
	}
	s.mu.Unlock()
	if !ok {
		return "", 0, 0, fmt.Errorf("unknown spool %q", id)
	}
	data := inMemory
	if !fromMemory {
		raw, err := os.ReadFile(path)
		if err != nil {
			return "", 0, 0, fmt.Errorf("read spool %s: %w", id, err)
		}
		data = string(raw)
	}
	if offset < 0 {
		offset = 0
	}
	if offset > len(data) {
		offset = len(data)
	}
	end := len(data)
	if max > 0 && offset+max < end {
		end = offset + max
	}
	return data[offset:end], end, len(data), nil
}

// Flush waits until every write queued before the call has finished.
func (s *SpoolStore) Flush() {
	s.mu.Lock()
	writes := make([]*spoolWrite, 0, len(s.pending))
	for _, w := range s.pending {
		writes = append(writes, w)
	}
	s.mu.Unlock()
	wait(writes)
}

// Remove deletes the given spool entries and their files. Unknown IDs are
// ignored. A host that serves several conversations from one process (ACP)
// uses it to drop one conversation's outputs without touching the others'.
func (s *SpoolStore) Remove(ids ...string) {
	s.mu.Lock()
	var writes []*spoolWrite
	for _, id := range ids {
		if w := s.pending[strings.TrimSpace(id)]; w != nil {
			writes = append(writes, w)
		}
	}
	s.mu.Unlock()
	wait(writes)

	s.mu.Lock()
	defer s.mu.Unlock()
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if path, ok := s.files[id]; ok {
			_ = os.Remove(path)
			delete(s.files, id)
			delete(s.failed, id)
		}
	}
}

// Cleanup deletes every spool file and resets the store; call on session end.
// It waits for queued writes first, including ones added while it waits,
// so no write is in progress when the directory is removed.
func (s *SpoolStore) Cleanup() {
	s.mu.Lock()
	for len(s.pending) > 0 {
		writes := make([]*spoolWrite, 0, len(s.pending))
		for _, w := range s.pending {
			writes = append(writes, w)
		}
		s.mu.Unlock()
		wait(writes)
		s.mu.Lock()
	}
	defer s.mu.Unlock()
	if s.dir != "" {
		_ = os.RemoveAll(s.dir)
	}
	s.dir = ""
	s.files = map[string]string{}
	s.failed = map[string]string{}
}
