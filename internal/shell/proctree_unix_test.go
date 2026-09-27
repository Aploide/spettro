//go:build unix

package shell

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// processGone polls until pid no longer exists (its orphaned parent was killed,
// so init reaps it shortly after the kill lands).
func processGone(pid int) bool {
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

// The hang this guards against: a timed-out command whose grandchild inherited
// the output pipe. Killing only the interpreter left the grandchild holding the
// pipe and CombinedOutput blocked until it exited on its own.
func TestConfigureProcessTreeKillsWholeGroupOnCancel(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", "sleep 30 & echo $!; wait")
	ConfigureProcessTree(cmd)
	start := time.Now()
	out, err := cmd.CombinedOutput()
	if elapsed := time.Since(start); elapsed > DefaultWaitDelay+2*time.Second {
		t.Fatalf("CombinedOutput blocked for %s after the deadline", elapsed)
	}
	if err == nil {
		t.Fatal("cancelled command reported success")
	}
	pid, convErr := strconv.Atoi(strings.TrimSpace(string(out)))
	if convErr != nil {
		t.Fatalf("partial output lost: %q", out)
	}
	if !processGone(pid) {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		t.Fatalf("grandchild %d survived the group kill", pid)
	}
}

// A command that exits normally while a background child still holds its
// output must not block Wait: WaitDelay caps the wait and the exit status is
// still success.
func TestConfigureProcessTreeBoundsLingeringPipeHolders(t *testing.T) {
	cmd := exec.CommandContext(context.Background(), "sh", "-c", "echo done; sleep 30 &")
	cmd.WaitDelay = 200 * time.Millisecond
	ConfigureProcessTree(cmd)
	if cmd.WaitDelay != 200*time.Millisecond {
		t.Fatalf("explicit WaitDelay overridden: %s", cmd.WaitDelay)
	}
	start := time.Now()
	out, err := cmd.CombinedOutput()
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("CombinedOutput blocked for %s", elapsed)
	}
	if err != nil && !errors.Is(err, exec.ErrWaitDelay) {
		t.Fatalf("unexpected error: %v", err)
	}
	if cmd.ProcessState == nil || !cmd.ProcessState.Success() {
		t.Fatalf("exit status lost: %v", cmd.ProcessState)
	}
	if !strings.Contains(string(out), "done") {
		t.Fatalf("output lost: %q", out)
	}
	_ = KillProcessTree(cmd)
}

func TestKillProcessTreeNotStarted(t *testing.T) {
	if err := KillProcessTree(exec.Command("true")); err != nil {
		t.Fatalf("kill of unstarted command: %v", err)
	}
	if err := KillProcessTree(nil); err != nil {
		t.Fatalf("kill of nil command: %v", err)
	}
}

// waitForPIDFile polls until path holds a pid.
func waitForPIDFile(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if raw, err := os.ReadFile(path); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(raw))); err == nil {
				return pid
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("no pid written to %s", path)
	return 0
}

// KillAllProcessTrees reaches a command CombinedOutput is still running, and
// the command's output and status come back as for exec's CombinedOutput.
func TestKillAllProcessTreesKillsRunningCommand(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pid")
	cmd := exec.CommandContext(context.Background(), "sh", "-c", "echo started; sleep 30 & echo $! > "+pidFile+"; wait")
	ConfigureProcessTree(cmd)
	type result struct {
		out []byte
		err error
	}
	done := make(chan result, 1)
	go func() {
		out, err := CombinedOutput(cmd)
		done <- result{out, err}
	}()
	pid := waitForPIDFile(t, pidFile)
	KillAllProcessTrees()
	select {
	case res := <-done:
		if res.err == nil {
			t.Fatal("killed command reported success")
		}
		if !strings.Contains(string(res.out), "started") {
			t.Fatalf("output lost: %q", res.out)
		}
	case <-time.After(DefaultWaitDelay + 3*time.Second):
		t.Fatal("CombinedOutput did not return after KillAllProcessTrees")
	}
	if !processGone(pid) {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		t.Fatalf("grandchild %d survived KillAllProcessTrees", pid)
	}
}

// hangupHelperEnv makes the test binary act as a spettro stand-in: it installs
// the hangup handler and runs a long command in its own process group.
const hangupHelperEnv = "SPETTRO_PROCTREE_HANGUP_HELPER"

// hangupMarkerEnv, when set, makes the helper register an extra hangup
// cleanup that creates the named file.
const hangupMarkerEnv = "SPETTRO_PROCTREE_HANGUP_MARKER"

// hangupHelperLinger is how long the helper stays alive after its command
// returns, waiting for the hangup handler to re-raise SIGHUP. It only has to
// outlast the tests' own deadlines: a helper still alive after it is one the
// hangup failed to end.
const hangupHelperLinger = time.Minute

// runTestHelperIfRequested turns the test binary into the hangup helper when
// a hangup test started it as one; see TestMain in home_isolation_test.go.
func runTestHelperIfRequested() {
	if pidFile := os.Getenv(hangupHelperEnv); pidFile != "" {
		runHangupHelper(pidFile)
	}
}

// runHangupHelper is the body of the helper process the hangup tests start:
// a spettro stand-in that installs the hangup handler and then blocks in a
// long command running in its own process group. It never returns.
//
// The command returning does not mean the hangup failed. The handler kills
// the command first and only re-raises SIGHUP after the extra cleanups ran, so
// CombinedOutput returns while the handler is still working. Exiting right
// away would race the re-raise (the test then saw "exit status 3" instead of
// death by SIGHUP) and could skip the extra cleanups altogether, so the
// helper waits for the signal instead and exits on its own only if it never
// comes.
func runHangupHelper(pidFile string) {
	var cleanups []func()
	if marker := os.Getenv(hangupMarkerEnv); marker != "" {
		cleanups = append(cleanups, func() { _ = os.WriteFile(marker, nil, 0o644) })
	}
	KillProcessTreesOnHangup(cleanups...)
	cmd := exec.CommandContext(context.Background(), "sh", "-c", "sleep 30 & echo $! > "+pidFile+"; wait")
	ConfigureProcessTree(cmd)
	_, _ = CombinedOutput(cmd)
	time.Sleep(hangupHelperLinger)
	os.Exit(3) // only reached if the hangup did not end the process
}

// hangUpAndWaitForDeath sends SIGHUP to a started hangup helper and fails the
// test unless the helper dies of that signal within a bound: death by SIGHUP
// is what the handler's re-raise produces once its cleanup has finished.
func hangUpAndWaitForDeath(t *testing.T, helper *exec.Cmd) {
	t.Helper()
	if err := helper.Process.Signal(syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	waited := make(chan error, 1)
	go func() { waited <- helper.Wait() }()
	select {
	case err := <-waited:
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Fatalf("helper exit = %v, want death by SIGHUP", err)
		}
		if ws, ok := exitErr.Sys().(syscall.WaitStatus); !ok || !ws.Signaled() || ws.Signal() != syscall.SIGHUP {
			t.Fatalf("helper exit = %v, want death by SIGHUP", err)
		}
	case <-time.After(10 * time.Second):
		_ = helper.Process.Kill()
		t.Fatal("helper did not exit on SIGHUP")
	}
}

// Closing the terminal sends SIGHUP to spettro's process group only. The
// command in its own group must still die, and spettro must still terminate.
func TestHangupKillsCommandTrees(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pid")
	helper := exec.Command(os.Args[0], "-test.run=^$")
	helper.Env = append(os.Environ(), hangupHelperEnv+"="+pidFile)
	if err := helper.Start(); err != nil {
		t.Fatal(err)
	}
	pid := waitForPIDFile(t, pidFile)
	hangUpAndWaitForDeath(t, helper)
	if !processGone(pid) {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		t.Fatalf("command %d survived the hangup", pid)
	}
}

// A command that exits while something it started with & keeps running (and
// holding the output pipe) returns promptly, and the leftover process stays
// tracked so session cleanup still reaches it.
func TestCombinedOutputTracksLeftoverBackgroundProcesses(t *testing.T) {
	cmd := exec.CommandContext(context.Background(), "sh", "-c", "sleep 47 & echo started $!")
	ConfigureProcessTree(cmd)
	start := time.Now()
	out, err := CombinedOutput(cmd)
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("CombinedOutput took %s for a command that exited at once", elapsed)
	}
	if !errors.Is(err, ErrBackgroundLeft) {
		t.Fatalf("err = %v, want ErrBackgroundLeft", err)
	}
	if cmd.ProcessState == nil || !cmd.ProcessState.Success() {
		t.Fatalf("exit status lost: %v", cmd.ProcessState)
	}
	fields := strings.Fields(string(out))
	if len(fields) != 2 || fields[0] != "started" {
		t.Fatalf("output = %q", out)
	}
	pid, _ := strconv.Atoi(fields[1])
	if processGone(pid) {
		t.Fatal("the background process must be left running until cleanup")
	}
	KillAllProcessTrees()
	if !processGone(pid) {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		t.Fatalf("leftover background process %d survived KillAllProcessTrees", pid)
	}
}

// Captured output is bounded: the head and tail are kept and the middle is
// replaced by a marker, so a runaway command cannot exhaust memory.
func TestCombinedOutputCapsCapturedBytes(t *testing.T) {
	origHead, origTail := captureHeadBytes, captureTailBytes
	captureHeadBytes, captureTailBytes = 1000, 1000
	t.Cleanup(func() { captureHeadBytes, captureTailBytes = origHead, origTail })
	cmd := exec.CommandContext(context.Background(), "sh", "-c", "echo FIRST; head -c 200000 /dev/zero | tr '\\0' 'a'; echo; echo LAST")
	ConfigureProcessTree(cmd)
	out, err := CombinedOutput(cmd)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) > 2200 {
		t.Fatalf("captured %d bytes, want about 2000", len(out))
	}
	s := string(out)
	if !strings.HasPrefix(s, "FIRST\n") || !strings.HasSuffix(s, "LAST\n") || !strings.Contains(s, "bytes of output omitted") {
		t.Fatalf("head/tail/marker missing: %.120q ... %.120q", s, s[max(0, len(s)-120):])
	}
}

// Under nohup SIGHUP is ignored: the handler must not be installed, or a
// hangup would SIGKILL every running command while spettro carries on.
func TestHangupIgnoredUnderNohupLeavesCommandsAlone(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pid")
	helper := exec.Command("sh", "-c", `trap "" HUP; exec "$0" -test.run='^$'`, os.Args[0])
	helper.Env = append(os.Environ(), hangupHelperEnv+"="+pidFile)
	if err := helper.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = helper.Process.Kill(); _ = helper.Wait() }()
	pid := waitForPIDFile(t, pidFile)
	defer syscall.Kill(pid, syscall.SIGKILL)
	if err := helper.Process.Signal(syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond)
	if err := syscall.Kill(pid, 0); err != nil {
		t.Fatalf("command %d was killed by an ignored hangup", pid)
	}
	if err := helper.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatal("helper died on an ignored hangup")
	}
}

// The hangup cleanup also runs the extra cleanups (background jobs, PTY
// sessions) main registers, before the process dies.
func TestHangupRunsExtraCleanups(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "pid")
	marker := filepath.Join(dir, "cleaned")
	helper := exec.Command(os.Args[0], "-test.run=^$")
	helper.Env = append(os.Environ(), hangupHelperEnv+"="+pidFile, hangupMarkerEnv+"="+marker)
	if err := helper.Start(); err != nil {
		t.Fatal(err)
	}
	pid := waitForPIDFile(t, pidFile)
	defer syscall.Kill(pid, syscall.SIGKILL)
	hangUpAndWaitForDeath(t, helper)
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("extra hangup cleanup did not run")
	}
}

// liveTreeCount returns how many commands liveTrees currently records.
func liveTreeCount() int {
	n := 0
	liveTrees.Range(func(_, _ any) bool {
		n++
		return true
	})
	return n
}

// reopenProcessTrees undoes shutDownProcessTrees so later tests in this
// process can start commands again.
func reopenProcessTrees() {
	treesMu.Lock()
	treesClosed = false
	treesMu.Unlock()
}

// After the hangup sweep no command may start: one started later would run
// on, unkilled, once spettro is gone. Callers keep starting commands while the
// sweep happens (the agent reacting to its killed command); every one of them
// must either be killed by the sweep or be refused with ErrShuttingDown, so
// every caller returns promptly. A command slipping past the sweep would keep
// its caller blocked for the whole sleep.
func TestShutDownProcessTreesLeavesNoCommandBehind(t *testing.T) {
	t.Cleanup(reopenProcessTrees)
	const callers = 8
	finished := make(chan error, callers)
	// stop tells the callers to quit instead of starting another command. It
	// only matters when the test fails: then some callers are still looping,
	// and without it they would keep starting commands once reopenProcessTrees
	// lets them, leaving the last batch running after the test binary exits.
	stop := make(chan struct{})
	var workers sync.WaitGroup
	// Registered after reopenProcessTrees, so it runs before it (cleanups run
	// last-in first-out): every caller has returned before commands may start
	// again.
	t.Cleanup(func() { stopCallers(stop, &workers) })
	for range callers {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				cmd := exec.CommandContext(context.Background(), "sh", "-c", "sleep 30")
				ConfigureProcessTree(cmd)
				if _, err := CombinedOutput(cmd); errors.Is(err, ErrShuttingDown) {
					finished <- nil
					return
				} else if err == nil {
					finished <- errors.New("a command meant to be killed exited successfully")
					return
				}
				// Killed by the sweep: start another, as an agent retrying would.
			}
		}()
	}
	// Sweep while commands are running, so the sweep has something to kill.
	deadline := time.Now().Add(5 * time.Second)
	for liveTreeCount() < callers/2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	shutDownProcessTrees()
	for range callers {
		select {
		case err := <-finished:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("a command started after the shutdown sweep was left running")
		}
	}
}

// stopCallers makes the command-starting goroutines of
// TestShutDownProcessTreesLeavesNoCommandBehind return, and waits for them.
// Closing stop keeps each caller from starting another command, and the
// repeated sweeps kill whatever command a caller is blocked on. A caller that
// checked stop just before it was closed can still start one more command
// after a sweep, so the sweep repeats until every caller has returned; treesMu
// guarantees each sweep sees every command whose start has completed.
func stopCallers(stop chan struct{}, workers *sync.WaitGroup) {
	close(stop)
	done := make(chan struct{})
	go func() {
		workers.Wait()
		close(done)
	}()
	for {
		KillAllProcessTrees()
		select {
		case <-done:
			return
		case <-time.After(50 * time.Millisecond):
		}
	}
}
