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
