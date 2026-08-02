//go:build !unix

package pty

import (
	"fmt"
	"os/exec"
)

// Supported reports whether this platform can allocate PTYs. Windows has the
// necessary primitive in ConPTY, but wiring it up means driving CreateProcess
// directly — os/exec cannot pass the process attribute list a pseudoconsole is
// attached through — and therefore re-implementing sandbox token handling and
// stdio plumbing by hand. Until that is done and proven, the pty tools report
// unsupported here rather than offering a session that half works.
func Supported() bool { return false }

func (m *Manager) Start(cmd *exec.Cmd, command string, cols, rows uint16) (*Session, error) {
	return nil, fmt.Errorf("pty sessions are unsupported on this platform")
}

func (s *Session) Write(input string) error {
	return fmt.Errorf("pty sessions are unsupported on this platform")
}

func (s *Session) kill() error { return nil }
