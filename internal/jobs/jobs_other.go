//go:build !unix

package jobs

import "os/exec"

func detach(cmd *exec.Cmd) {}

func kill(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}
