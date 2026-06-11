package tui

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"spettro/internal/session"
	"spettro/internal/storage"
)

// newAutoSaveModel returns a Model wired to a temp global dir with one user
// message, ready to exercise the autosave paths.
func newAutoSaveModel(t *testing.T) (*Model, string) {
	t.Helper()
	tmp := t.TempDir()
	m := NewModelForTesting()
	m.store = &storage.Store{ProjectDir: filepath.Join(tmp, ".spettro"), GlobalDir: tmp}
	m.cwd = tmp
	m.messages = []ChatMessage{{Role: RoleUser, Content: "hello", At: time.Now()}}
	return &m, tmp
}

func sessionFileExists(t *testing.T, globalDir, id string) bool {
	t.Helper()
	if id == "" {
		return false
	}
	_, err := os.Stat(filepath.Join(session.SessionDir(globalDir, id), "messages.json"))
	return err == nil
}

// TestRefreshViewportDoesNotSave verifies PERF-2's decoupling: rendering must
// not persist the session.
func TestRefreshViewportDoesNotSave(t *testing.T) {
	m, tmp := newAutoSaveModel(t)
	m.ensureSession()
	id := m.sessionID
	m.refreshViewport()
	if sessionFileExists(t, tmp, id) {
		t.Fatal("refreshViewport must not write a session file")
	}
}

// TestAutoSaveWritesSession verifies a forced save persists immediately.
func TestAutoSaveWritesSession(t *testing.T) {
	m, tmp := newAutoSaveModel(t)
	m.autoSave()
	if !sessionFileExists(t, tmp, m.sessionID) {
		t.Fatal("autoSave should write a session file")
	}
}

// TestAutoSaveDebouncedRespectsInterval verifies the debounce window: a second
// call inside the window is a no-op, while a forced save still writes.
func TestAutoSaveDebouncedRespectsInterval(t *testing.T) {
	m, tmp := newAutoSaveModel(t)

	m.autoSaveDebounced() // first call: lastAutoSaveAt was zero, so it saves
	if !sessionFileExists(t, tmp, m.sessionID) {
		t.Fatal("first debounced save should write")
	}
	firstSave := m.lastAutoSaveAt
	if firstSave.IsZero() {
		t.Fatal("expected lastAutoSaveAt to be set after first save")
	}

	// Second call within the interval must be skipped (timestamp unchanged).
	m.messages = append(m.messages, ChatMessage{Role: RoleAssistant, Content: "hi", At: time.Now()})
	m.autoSaveDebounced()
	if !m.lastAutoSaveAt.Equal(firstSave) {
		t.Fatal("debounced save within interval should be skipped")
	}

	// Simulate the interval elapsing: the next debounced save should fire.
	m.lastAutoSaveAt = time.Now().Add(-autoSaveMinInterval - time.Second)
	m.autoSaveDebounced()
	if m.lastAutoSaveAt.Equal(firstSave) || time.Since(m.lastAutoSaveAt) > autoSaveMinInterval {
		t.Fatal("debounced save after interval should fire")
	}
}

// TestAutoSaveSkipsEmptyConversation verifies a session with no user/assistant
// content is never persisted (matches prior behavior).
func TestAutoSaveSkipsEmptyConversation(t *testing.T) {
	tmp := t.TempDir()
	m := NewModelForTesting()
	m.store = &storage.Store{ProjectDir: filepath.Join(tmp, ".spettro"), GlobalDir: tmp}
	m.cwd = tmp
	m.messages = []ChatMessage{{Role: RoleSystem, Content: "ready", At: time.Now()}}
	m.ensureSession()
	id := m.sessionID
	m.autoSave()
	if sessionFileExists(t, tmp, id) {
		t.Fatal("autoSave must skip system-only conversations")
	}
}
