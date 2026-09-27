package shell

import (
	"errors"
	"fmt"
	"os"
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

// liveTrees holds the commands started by CombinedOutput whose process group
// is still running: the command itself, or processes it left behind when it
// exited (a server started with &). Each runs in its own process group, out of
// reach of signals aimed at spettro's own job, so they are killed explicitly
// when spettro goes away (KillAllProcessTrees, KillProcessTreesOnHangup).
var liveTrees sync.Map // *exec.Cmd -> struct{}

// treesMu orders command starts against the kill-all sweep
// (KillAllProcessTrees). startTracked holds the read lock from cmd.Start until
// the command is recorded in liveTrees, and the sweep takes the write lock, so
// the sweep can never run in the window where a command is already running
// but not yet recorded, and miss it. Starts share the read lock, so parallel
// tool calls still start their commands concurrently.
var treesMu sync.RWMutex

// treesClosed is set, under treesMu, once the hangup cleanup has begun (see
// shutDownProcessTrees). From then on CombinedOutput refuses to start
// commands: the agent reacting to its killed command must not start a new one
// after the sweep, because nothing would kill it before spettro exits.
var treesClosed bool

// ErrShuttingDown is returned by CombinedOutput, without starting the command,
// once spettro has begun shutting down because its terminal hung up.
var ErrShuttingDown = errors.New("spettro is shutting down; command not started")

// ErrBackgroundLeft is returned by CombinedOutput when the command exited
// successfully but left processes running in its process group (started with
// & or nohup). They keep running, their later output is discarded, and they
// stay tracked so session cleanup kills them instead of orphaning them.
var ErrBackgroundLeft = errors.New("command exited but left background processes running")

// outputGrace bounds how long CombinedOutput keeps collecting output after the
// command exits while something it started still holds the output pipe.
var outputGrace = 500 * time.Millisecond

// captureHeadBytes and captureTailBytes bound the output CombinedOutput keeps
// in memory: the first and last bytes, with the middle replaced by a marker.
// Tool output is cut to a few tens of thousands of characters anyway; this
// only stops a runaway command (yes, cat of a huge log) from exhausting memory.
var (
	captureHeadBytes = 2 << 20
	captureTailBytes = 2 << 20
)

// leftoverPollInterval is how often a leftover process group is checked, so
// it is forgotten once it has ended.
const leftoverPollInterval = time.Second

// CombinedOutput runs a command configured with ConfigureProcessTree and
// returns its combined stdout and stderr (bounded, see captureHeadBytes). The
// command's tree is recorded as live while it runs so KillAllProcessTrees can
// reach it. It returns as soon as the command exits (plus outputGrace when a
// leftover process holds the pipe); leftover processes are reported with
// ErrBackgroundLeft and remain tracked until they end. After a hangup it
// starts nothing and returns ErrShuttingDown.
func CombinedOutput(cmd *exec.Cmd) ([]byte, error) {
	pr, pw, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	// Handing exec an *os.File means no copying goroutine: Wait returns when
	// the command exits, not when every process holding the pipe does.
	cmd.Stdout = pw
	cmd.Stderr = pw
	if err := startTracked(cmd); err != nil {
		pr.Close()
		pw.Close()
		return nil, err
	}
	pw.Close()

	var mu sync.Mutex
	buf := newCaptureBuffer(captureHeadBytes, captureTailBytes)
	collecting := true
	eof := make(chan struct{})
	go func() {
		defer close(eof)
		defer pr.Close()
		chunk := make([]byte, 32<<10)
		for {
			n, rerr := pr.Read(chunk)
			if n > 0 {
				mu.Lock()
				if collecting {
					buf.Write(chunk[:n])
				}
				mu.Unlock()
			}
			if rerr != nil {
				return
			}
		}
	}()

	waitErr := cmd.Wait()
	pipeClosed := false
	select {
	case <-eof:
		pipeClosed = true
	case <-time.After(outputGrace):
	}
	mu.Lock()
	collecting = false
	out := buf.Bytes()
	mu.Unlock()

	if !groupAlive(cmd) {
		liveTrees.Delete(cmd)
		if !pipeClosed {
			// A holder outside the group (it started its own session): stop
			// reading rather than wait for it.
			abandonPipe(pr)
		}
		return out, waitErr
	}
	// The command left processes running. Keep draining the pipe so they
	// are not killed by SIGPIPE on their next write, keep the group tracked
	// for session cleanup, and forget it once it has ended.
	go func() {
		for groupAlive(cmd) {
			time.Sleep(leftoverPollInterval)
		}
		liveTrees.Delete(cmd)
		abandonPipe(pr)
	}()
	if waitErr != nil {
		return out, waitErr
	}
	return out, ErrBackgroundLeft
}

// startTracked starts cmd and records it in liveTrees as a single step with
// respect to the kill-all sweep (see treesMu). Once the hangup cleanup has
// begun it starts nothing and returns ErrShuttingDown.
func startTracked(cmd *exec.Cmd) error {
	treesMu.RLock()
	defer treesMu.RUnlock()
	if treesClosed {
		return ErrShuttingDown
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	liveTrees.Store(cmd, struct{}{})
	return nil
}

// captureBuffer keeps the first head and the last tail bytes written to it.
type captureBuffer struct {
	head, tail []byte
	headMax    int
	tailMax    int
	total      int64
}

func newCaptureBuffer(headMax, tailMax int) *captureBuffer {
	return &captureBuffer{headMax: headMax, tailMax: tailMax}
}

func (b *captureBuffer) Write(p []byte) {
	b.total += int64(len(p))
	if room := b.headMax - len(b.head); room > 0 {
		n := min(room, len(p))
		b.head = append(b.head, p[:n]...)
		p = p[n:]
	}
	if len(p) == 0 || b.tailMax <= 0 {
		return
	}
	b.tail = append(b.tail, p...)
	if len(b.tail) > 2*b.tailMax {
		b.tail = append(b.tail[:0], b.tail[len(b.tail)-b.tailMax:]...)
	}
}

func (b *captureBuffer) Bytes() []byte {
	tail := b.tail
	if len(tail) > b.tailMax {
		tail = tail[len(tail)-b.tailMax:]
	}
	omitted := b.total - int64(len(b.head)) - int64(len(tail))
	out := make([]byte, 0, len(b.head)+len(tail)+64)
	out = append(out, b.head...)
	if omitted > 0 {
		out = fmt.Appendf(out, "\n[... %d bytes of output omitted ...]\n", omitted)
	}
	return append(out, tail...)
}

// KillAllProcessTrees kills every command tree CombinedOutput is still
// running or left behind. spettro calls it on the way out: without it a
// foreground command in its own process group (a hung test run, a dev server
// started with &) would outlive the session.
//
// It holds treesMu for writing while it sweeps, so a command whose start is
// in progress is either recorded before the sweep (and killed by it) or
// starts after the sweep has finished.
func KillAllProcessTrees() {
	treesMu.Lock()
	defer treesMu.Unlock()
	liveTrees.Range(func(key, _ any) bool {
		_ = KillProcessTree(key.(*exec.Cmd))
		return true
	})
}
