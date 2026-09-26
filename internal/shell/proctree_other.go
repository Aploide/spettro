//go:build !unix && !windows

package shell

import "os/exec"

func configureProcessTree(cmd *exec.Cmd) {}

func killProcessTree(cmd *exec.Cmd) error { return cmd.Process.Kill() }

// KillProcessTreesOnHangup is a no-op: there is no SIGHUP here.
func KillProcessTreesOnHangup() {}
