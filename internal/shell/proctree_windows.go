//go:build windows

package shell

import (
	"os/exec"
	"strconv"
	"syscall"
)

func configureProcessTree(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	// A separate process group keeps a console Ctrl-C aimed at spettro from
	// reaching the command; the tree is ended explicitly below instead.
	cmd.SysProcAttr.CreationFlags |= syscall.CREATE_NEW_PROCESS_GROUP
}

func killProcessTree(cmd *exec.Cmd) error {
	// Windows cannot signal a process group as a unit; taskkill /T walks the
	// parent/child links from the interpreter down and ends every process.
	kill := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid))
	if err := kill.Run(); err == nil {
		return nil
	}
	return cmd.Process.Kill()
}
