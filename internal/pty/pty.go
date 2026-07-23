// Package pty tracks interactive pseudo-terminal sessions started by the
// agent (REPLs, debuggers, ssh, watch-mode servers). Like background jobs,
// sessions are process-wide session state: they outlive individual agent
// turns and are killed when the session ends.
package pty

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/x/ansi"
)

// outputCap bounds the retained scrollback per session. Once exceeded the
// oldest bytes are dropped; offsets keep counting absolute bytes written so
// incremental reads stay consistent.
const outputCap = 256 << 10 // 256 KiB

// Session is one live pseudo-terminal with its command and scrollback.
type Session struct {
	ID      string
	Command string
	Started time.Time

	mu       sync.Mutex
	buf      []byte
	dropped  int // absolute offset of buf[0]
	readPos  int // absolute offset of the last model read
	done     bool
	exitInfo string
	master   io.WriteCloser // pty master side; model input is written here
	pid      int            // process group leader (child runs under Setsid)
}

// append feeds bytes read from the pty master into the ring buffer.
func (s *Session) append(p []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.buf = append(s.buf, p...)
	if over := len(s.buf) - outputCap; over > 0 {
		s.buf = s.buf[over:]
		s.dropped += over
	}
}

// ReadNew returns output produced since the previous ReadNew call and whether
// the session is still running. The read cursor advances to the end of the
// buffer.
func (s *Session) ReadNew() (out string, running bool, exitInfo string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	pos := s.readPos
	if pos < s.dropped {
		pos = s.dropped
	}
	rel := min(pos-s.dropped, len(s.buf))
	out = string(s.buf[rel:])
	s.readPos = s.dropped + len(s.buf)
	return out, !s.done, s.exitInfo
}

// Scrollback returns the full retained buffer (raw bytes, ANSI included)
// without moving the read cursor.
func (s *Session) Scrollback() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return string(s.buf)
}

// Tail returns the last n settled lines of the scrollback (all lines when
// n <= 0) without moving the read cursor. Used for live-tail views.
func (s *Session) Tail(n int) []string {
	lines := strings.Split(Settle(s.Scrollback()), "\n")
	if n > 0 && len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines
}

// Settle renders raw terminal bytes as plain text the way a terminal would
// show them once quiescent: ANSI escapes are stripped, \r moves the cursor
// to column 0 (overwriting, so readline's per-keystroke line redraws
// collapse to the final line instead of concatenating partial frames), and
// \b steps back one column.
func Settle(out string) string {
	out = ansi.Strip(out)
	var lines []string
	line := make([]rune, 0, 80)
	col := 0
	flush := func() {
		lines = append(lines, string(line))
		line = line[:0]
		col = 0
	}
	for _, r := range out {
		switch r {
		case '\n':
			flush()
		case '\r':
			col = 0
		case '\b':
			if col > 0 {
				col--
			}
		default:
			if col < len(line) {
				line[col] = r
			} else {
				line = append(line, r)
			}
			col++
		}
	}
	if len(line) > 0 {
		lines = append(lines, string(line))
	}
	return strings.Join(lines, "\n")
}

func (s *Session) Running() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.done
}

// Manager tracks the session's live PTYs.
type Manager struct {
	mu       sync.Mutex
	sessions map[string]*Session
	seq      int
}

func NewManager() *Manager {
	return &Manager{sessions: map[string]*Session{}}
}

var defaultManager = NewManager()

// Default returns the process-wide manager; the spettro process is one
// session, so session-scoped PTYs live here.
func Default() *Manager { return defaultManager }

func (m *Manager) register(s *Session) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.seq++
	s.ID = fmt.Sprintf("pty-%d", m.seq)
	m.sessions[s.ID] = s
	return s.ID
}

func (m *Manager) Get(id string) (*Session, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[strings.TrimSpace(id)]
	return s, ok
}

// Kill terminates the session's process group (SIGTERM, then SIGKILL after a
// short grace) and frees its pty. Killing an already-exited session is a
// no-op.
func (m *Manager) Kill(id string) error {
	s, ok := m.Get(id)
	if !ok {
		return fmt.Errorf("unknown pty session %q", id)
	}
	return s.kill()
}

// KillAll terminates every running session; call on session exit.
func (m *Manager) KillAll() {
	m.mu.Lock()
	sessions := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		sessions = append(sessions, s)
	}
	m.mu.Unlock()
	for _, s := range sessions {
		_ = s.kill()
	}
}

// List returns all sessions ordered by start time.
func (m *Manager) List() []*Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		out = append(out, s)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Started.Before(out[b].Started) })
	return out
}

// RunningCount reports how many sessions are still running.
func (m *Manager) RunningCount() int {
	n := 0
	for _, s := range m.List() {
		if s.Running() {
			n++
		}
	}
	return n
}
