//go:build unix

package pty

import (
	"os/exec"
	"strings"
	"testing"
	"time"
)

// startSession launches command under a fresh manager, skipping the test when
// the environment cannot allocate a PTY (some minimal CI containers).
func startSession(t *testing.T, m *Manager, name string, args ...string) *Session {
	t.Helper()
	s, err := m.Start(exec.Command(name, args...), name, 0, 0)
	if err != nil {
		if strings.Contains(err.Error(), "start pty") {
			t.Skipf("no pty available: %v", err)
		}
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = s.kill() })
	return s
}

// waitOutput polls ReadNew until want appears or the deadline passes.
func waitOutput(t *testing.T, s *Session, want string) string {
	t.Helper()
	var got strings.Builder
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		out, _, _ := s.ReadNew()
		got.WriteString(out)
		if strings.Contains(got.String(), want) {
			return got.String()
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("output %q never appeared; got %q", want, got.String())
	return ""
}

func TestStartWriteEchoKill(t *testing.T) {
	m := NewManager()
	s := startSession(t, m, "cat")
	if s.ID != "pty-1" {
		t.Fatalf("id = %q, want pty-1", s.ID)
	}
	if err := s.Write("hello\r"); err != nil {
		t.Fatalf("write: %v", err)
	}
	// cat under a pty echoes the typed input and then prints it back.
	waitOutput(t, s, "hello")

	if err := m.Kill(s.ID); err != nil {
		t.Fatalf("kill: %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for s.Running() && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if s.Running() {
		t.Fatal("session still running after kill")
	}
	if err := s.Write("x"); err == nil {
		t.Fatal("write after exit should fail")
	}
	if err := m.Kill(s.ID); err != nil {
		t.Fatalf("kill of exited session should be a no-op, got %v", err)
	}
}

func TestInteractiveShellSession(t *testing.T) {
	m := NewManager()
	s := startSession(t, m, "sh", "-i")
	if err := s.Write("echo $((6*7))\r"); err != nil {
		t.Fatalf("write: %v", err)
	}
	waitOutput(t, s, "42")
	if err := s.Write("exit\r"); err != nil {
		t.Fatalf("write exit: %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for s.Running() && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if s.Running() {
		t.Fatal("shell did not exit")
	}
	if _, _, exitInfo := s.ReadNew(); exitInfo == "" {
		t.Fatal("exitInfo not recorded")
	}
}

func TestRingBufferOverflow(t *testing.T) {
	s := &Session{}
	chunk := make([]byte, 64<<10)
	for i := range chunk {
		chunk[i] = 'a'
	}
	for i := 0; i < 8; i++ {
		s.append(chunk)
	}
	if got := len(s.Scrollback()); got != outputCap {
		t.Fatalf("scrollback = %d bytes, want %d", got, outputCap)
	}
	// The read cursor stays consistent across drops: a reader positioned
	// before the dropped region resumes at the oldest retained byte.
	out, _, _ := s.ReadNew()
	if len(out) != outputCap {
		t.Fatalf("ReadNew after overflow = %d bytes, want %d", len(out), outputCap)
	}
	s.append([]byte("tail"))
	out, _, _ = s.ReadNew()
	if out != "tail" {
		t.Fatalf("incremental read = %q, want %q", out, "tail")
	}
}

func TestManagerListAndCount(t *testing.T) {
	m := NewManager()
	s1 := startSession(t, m, "cat")
	s2 := startSession(t, m, "cat")
	if n := m.RunningCount(); n != 2 {
		t.Fatalf("running = %d, want 2", n)
	}
	if l := m.List(); len(l) != 2 || l[0].ID != s1.ID || l[1].ID != s2.ID {
		t.Fatalf("list order wrong: %+v", l)
	}
	m.KillAll()
	deadline := time.Now().Add(3 * time.Second)
	for m.RunningCount() > 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if n := m.RunningCount(); n != 0 {
		t.Fatalf("running after KillAll = %d, want 0", n)
	}
	if _, ok := m.Get("nope"); ok {
		t.Fatal("Get of unknown id succeeded")
	}
	if err := m.Kill("nope"); err == nil {
		t.Fatal("Kill of unknown id should error")
	}
}
