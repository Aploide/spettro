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
type SpoolStore struct {
	mu    sync.Mutex
	dir   string
	seq   int
	files map[string]string // spool ID -> file path
}

func NewSpoolStore() *SpoolStore {
	return &SpoolStore{files: map[string]string{}}
}

var defaultSpool = NewSpoolStore()

// Spool returns the process-wide spool store; the spettro process is one
// session, so session-scoped spools live here.
func Spool() *SpoolStore { return defaultSpool }

// Add writes content to a new spool file and returns its ID ("spool:<n>").
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
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		return "", fmt.Errorf("write spool file: %w", err)
	}
	s.files[id] = path
	return id, nil
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
	s.mu.Lock()
	path, ok := s.files[strings.TrimSpace(id)]
	s.mu.Unlock()
	if !ok {
		return "", 0, 0, fmt.Errorf("unknown spool %q", id)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", 0, 0, fmt.Errorf("read spool %s: %w", id, err)
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
	return string(data[offset:end]), end, len(data), nil
}

// Cleanup deletes every spool file and resets the store; call on session end.
func (s *SpoolStore) Cleanup() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dir != "" {
		_ = os.RemoveAll(s.dir)
	}
	s.dir = ""
	s.files = map[string]string{}
}
