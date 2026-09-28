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

// groupAlive reports whether any process of cmd's process group still runs.
// While it does, the group id cannot be reused, so signalling it is safe.
func groupAlive(cmd *exec.Cmd) bool {
	err := syscall.Kill(-cmd.Process.Pid, 0)
	return err == nil || err == syscall.EPERM
}

// abandonPipe stops reading a command's output pipe. Pipes from os.Pipe are
// pollable here, so closing unblocks the pending read.
func abandonPipe(pr *os.File) { _ = pr.Close() }

// KillProcessTreesOnHangup makes a SIGHUP — the terminal or tmux pane spettro
// runs in was closed — kill every live command tree, and run the extra
// cleanups (background jobs, PTY sessions, which also live in their own
// process groups or sessions), before spettro exits. The shell delivers that
// SIGHUP to spettro's process group only; commands in their own groups would
// otherwise be orphaned and run on with no timeout. After the cleanup the
// default action is restored and the signal re-raised, so spettro still
// terminates exactly as it did without the handler.
//
// From the hangup on, CombinedOutput refuses to start new commands
// (ErrShuttingDown): the process is about to die, and a command started after
// the sweep would be orphaned.
//
// When SIGHUP is already ignored (spettro was started under nohup) nothing is
// installed: installing the handler would un-ignore it, so a hangup would
// kill every running command while spettro itself — the re-raised signal
// ignored again — carried on. Under nohup, commands survive the hangup too.
func KillProcessTreesOnHangup(cleanups ...func()) {
	if signal.Ignored(syscall.SIGHUP) {
		return
	}
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGHUP)
	go func() {
		<-ch
		shutDownProcessTrees()
		for _, fn := range cleanups {
			fn()
		}
		signal.Reset(syscall.SIGHUP)
		_ = syscall.Kill(os.Getpid(), syscall.SIGHUP)
	}()
}

// shutDownProcessTrees makes every later CombinedOutput call fail with
// ErrShuttingDown and then kills every live command tree. The order matters:
// closing first means no command can start after the sweep, and the sweep's
// write lock (see treesMu) means none that started before it is missed. When
// it returns, no command started through CombinedOutput can outlive spettro.
func shutDownProcessTrees() {
	treesMu.Lock()
	treesClosed = true
	treesMu.Unlock()
	KillAllProcessTrees()
}
