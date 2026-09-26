//go:build unix

package shell

import (
	"os/exec"
	"syscall"
)

func configureProcessTree(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

func killProcessTree(cmd *exec.Cmd) error {
	// Negative pid signals the process group created by Setpgid: bash -lc,
	// the program it ran, and anything that program forked.
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err == nil || err == syscall.ESRCH {
		return nil
	}
	return cmd.Process.Kill()
}
