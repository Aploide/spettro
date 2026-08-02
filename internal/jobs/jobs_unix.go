//go:build unix

package jobs

import (
	"os/exec"
	"syscall"
)

// detach puts the command in its own process group so killing the job takes
// down the whole tree (bash -lc spawns children) without touching spettro.
func detach(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// afterStart has nothing to do on Unix: Setpgid already established the
// process group before the child ran.
func afterStart(cmd *exec.Cmd) {}

// afterWait likewise has nothing to release: a process group holds no kernel
// object of its own, and it disappears with its last member.
func afterWait(cmd *exec.Cmd) {}

func kill(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	// Negative pid signals the process group created by Setpgid.
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err == nil || err == syscall.ESRCH {
		return nil
	}
	return cmd.Process.Kill()
}
