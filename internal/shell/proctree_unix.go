//go:build unix

package shell

import (
	"os"
	"os/exec"
	"os/signal"
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

// KillProcessTreesOnHangup makes a SIGHUP — the terminal or tmux pane spettro
// runs in was closed — kill every live command tree before spettro exits. The
// shell delivers that SIGHUP to spettro's process group only; commands in
// their own groups would otherwise be orphaned and run on with no timeout.
// After the cleanup the default action is restored and the signal re-raised,
// so spettro still terminates exactly as it did without the handler.
func KillProcessTreesOnHangup() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGHUP)
	go func() {
		<-ch
		KillAllProcessTrees()
		signal.Reset(syscall.SIGHUP)
		_ = syscall.Kill(os.Getpid(), syscall.SIGHUP)
	}()
}
