//go:build unix

package pty

import (
	"fmt"
	"os/exec"
	"syscall"
	"time"

	creack "github.com/creack/pty"
)

// Supported reports whether this platform can allocate PTYs.
func Supported() bool { return true }

// Start allocates a pseudo-terminal, launches cmd under it, registers the
// session, and reaps the process in the background. The cmd must not have
// been started yet and must not have Stdin/Stdout/Stderr set; the caller is
// responsible for any sandbox wrapping. creack/pty starts the child with
// Setsid+Setctty, so it leads its own process group and killing -pid takes
// down the whole tree.
func (m *Manager) Start(cmd *exec.Cmd, command string, cols, rows uint16) (*Session, error) {
	if cols == 0 {
		cols = 120
	}
	if rows == 0 {
		rows = 32
	}
	master, err := creack.StartWithSize(cmd, &creack.Winsize{Cols: cols, Rows: rows})
	if err != nil {
		return nil, fmt.Errorf("start pty: %w", err)
	}

	s := &Session{
		Command: command,
		Started: time.Now(),
		master:  master,
		pid:     cmd.Process.Pid,
	}
	m.register(s)

	go func() {
		buf := make([]byte, 4096)
		for {
			n, rerr := master.Read(buf)
			if n > 0 {
				s.append(buf[:n])
			}
			if rerr != nil {
				break
			}
		}
		werr := cmd.Wait()
		s.mu.Lock()
		s.done = true
		if werr != nil {
			s.exitInfo = werr.Error()
		} else {
			s.exitInfo = "exit status 0"
		}
		s.mu.Unlock()
		_ = master.Close()
	}()
	return s, nil
}

// Write sends input bytes to the session's terminal. Control characters pass
// through verbatim (\r submits a line, \x03 is Ctrl-C).
func (s *Session) Write(input string) error {
	s.mu.Lock()
	master, done := s.master, s.done
	s.mu.Unlock()
	if done || master == nil {
		return fmt.Errorf("pty session %s has exited", s.ID)
	}
	_, err := master.Write([]byte(input))
	return err
}

// kill terminates the session's process group: SIGTERM, a short grace, then
// SIGKILL. Killing an already-exited session is a no-op.
func (s *Session) kill() error {
	s.mu.Lock()
	pid, done := s.pid, s.done
	s.mu.Unlock()
	if done || pid <= 0 {
		return nil
	}
	_ = syscall.Kill(-pid, syscall.SIGTERM)
	for i := 0; i < 20; i++ {
		if !s.Running() {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
		return err
	}
	return nil
}
