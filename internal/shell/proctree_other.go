//go:build !unix && !windows

package shell

import (
	"os"
	"os/exec"
)

func configureProcessTree(cmd *exec.Cmd) {}

func killProcessTree(cmd *exec.Cmd) error { return cmd.Process.Kill() }

// KillProcessTreesOnHangup is a no-op: there is no SIGHUP here.
func KillProcessTreesOnHangup(cleanups ...func()) {}

// groupAlive reports false: without Unix process groups a leftover process
// cannot be tracked as a unit once the command has exited.
func groupAlive(cmd *exec.Cmd) bool { return false }

// abandonPipe leaves the pipe to the reading goroutine, which ends when the
// last holder exits: closing a pipe with a read pending is not safe here.
func abandonPipe(pr *os.File) {}
