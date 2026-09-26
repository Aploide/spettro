package shell

import (
	"bytes"
	"os/exec"
	"sync"
	"time"
)

// DefaultWaitDelay bounds how long Wait keeps reading a command's output pipes
// after the command itself has exited or been killed. A grandchild that
// inherited the pipe — a test binary spawned by `go test`, a server started
// with `&` — would otherwise hold it open and block CombinedOutput forever,
// long past any deadline on the command.
const DefaultWaitDelay = 3 * time.Second

// ConfigureProcessTree makes cmd run as the root of its own process tree so
// cancelling it takes down everything it spawned, not just the interpreter:
// on Unix the command gets its own process group and cancellation signals the
// whole group; on Windows it gets its own process group and the tree is
// terminated with taskkill. It also sets WaitDelay (when unset) so output
// collection can never outlive the command by more than DefaultWaitDelay.
//
// cmd must have been created with exec.CommandContext (sandbox.Command does
// this): the kill runs as cmd.Cancel, which os/exec only accepts for commands
// bound to a context. Existing SysProcAttr settings, such as the sandbox's
// Windows token, are preserved.
func ConfigureProcessTree(cmd *exec.Cmd) {
	configureProcessTree(cmd)
	cmd.Cancel = func() error { return KillProcessTree(cmd) }
	if cmd.WaitDelay == 0 {
		cmd.WaitDelay = DefaultWaitDelay
	}
}

// KillProcessTree forcibly terminates a started command configured with
// ConfigureProcessTree together with every process in its tree. It is a no-op
// for a command that has not started.
func KillProcessTree(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	return killProcessTree(cmd)
}

// liveTrees holds the commands started by CombinedOutput that are still
// running. Each runs in its own process group, out of reach of signals aimed at
// spettro's own job, so they are killed explicitly when spettro goes away
// (KillAllProcessTrees, KillProcessTreesOnHangup).
var liveTrees sync.Map // *exec.Cmd -> struct{}

// CombinedOutput is cmd.CombinedOutput for a command configured with
// ConfigureProcessTree, recording the command's tree as live while it runs so
// KillAllProcessTrees can reach it.
func CombinedOutput(cmd *exec.Cmd) ([]byte, error) {
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	liveTrees.Store(cmd, struct{}{})
	err := cmd.Wait()
	liveTrees.Delete(cmd)
	return buf.Bytes(), err
}

// KillAllProcessTrees kills every command tree CombinedOutput is still
// running. spettro calls it on the way out: without it a foreground command
// in its own process group (a hung test run, a dev server) would outlive the
// session.
func KillAllProcessTrees() {
	liveTrees.Range(func(key, _ any) bool {
		_ = KillProcessTree(key.(*exec.Cmd))
		return true
	})
}
