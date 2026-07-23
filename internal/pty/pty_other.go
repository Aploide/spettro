//go:build !unix

package pty

import (
	"fmt"
	"os/exec"
)

// Supported reports whether this platform can allocate PTYs. Windows ConPTY
// support is a follow-up; for now the tools report unsupported.
func Supported() bool { return false }

func (m *Manager) Start(cmd *exec.Cmd, command string, cols, rows uint16) (*Session, error) {
	return nil, fmt.Errorf("pty sessions are unsupported on this platform")
}

func (s *Session) Write(input string) error {
	return fmt.Errorf("pty sessions are unsupported on this platform")
}

func (s *Session) kill() error { return nil }
