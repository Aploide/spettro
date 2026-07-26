//go:build !unix || ios

package pty

import (
	"fmt"
	"os/exec"
)

// Supported reports whether this platform can allocate PTYs.
//
// This file covers two very different cases. On Windows the backend is simply
// not written yet — ConPTY support is a follow-up. On iOS it can never be
// written: `ios` satisfies Go's `unix` build tag, so without the explicit
// `|| ios` term above the build would select the creack backend and
// Supported() would answer true on a device that has neither /dev/ptmx nor
// the right to fork a child to attach to it. The agent's pty tools are
// removed from the toolset there anyway (internal/agent/exec_capability.go);
// this is the layer below that, so the honest answer survives even if a
// caller reaches the package directly.
func Supported() bool { return false }

func (m *Manager) Start(cmd *exec.Cmd, command string, cols, rows uint16) (*Session, error) {
	return nil, fmt.Errorf("pty sessions are unsupported on this platform")
}

func (s *Session) Write(input string) error {
	return fmt.Errorf("pty sessions are unsupported on this platform")
}

func (s *Session) kill() error { return nil }
