package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"spettro/internal/config"
	"spettro/internal/jobs"
	"spettro/internal/shell"
	"spettro/internal/shell/shelltest"
)

func newShellTestRuntime(t *testing.T) *toolRuntime {
	t.Helper()
	return &toolRuntime{
		cwd:          t.TempDir(),
		permission:   config.PermissionYOLO,
		readSet:      map[string]struct{}{},
		allowedShell: map[string]struct{}{},
		toolPolicies: map[string]config.ToolSpec{},
	}
}

func shellArgsJSON(t *testing.T, fields map[string]any) []byte {
	t.Helper()
	raw, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// A failing command's output is what the model needs to fix it: it must come
// back whole, with the exit status appended, and marked as an error.
func TestShellFailureKeepsOutputAndAppendsExitStatus(t *testing.T) {
	r := newShellTestRuntime(t)
	cmd := shelltest.Join(shelltest.Echo("--- FAIL: TestParse"), shelltest.EchoStderr("duration_test.go:28: want 1h30m0s"), shelltest.Exit(3))
	out, err := r.runShellTool(context.Background(), "bash", shellArgsJSON(t, map[string]any{"command": cmd}), "bash")
	if err == nil {
		t.Fatal("failing command reported success")
	}
	var described *toolOutputError
	if !errors.As(err, &described) {
		t.Fatalf("error %T does not mark the output as self-describing", err)
	}
	for _, want := range []string{"--- FAIL: TestParse", "duration_test.go:28: want 1h30m0s"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output lost %q: %q", want, out)
		}
	}
	if !strings.HasSuffix(out, "\n[exit status 3]") {
		t.Fatalf("exit status not appended: %q", out)
	}
	if got := toolErrorOutput(out, err); got != out {
		t.Fatalf("toolErrorOutput replaced the output: %q", got)
	}
}

// End to end through parallelExec: the result the model receives carries the
// command output, the status, and the error flag.
func TestParallelExecReportsFailedShellOutput(t *testing.T) {
	r := newShellTestRuntime(t)
	cmd := shelltest.Join(shelltest.Echo("compiler says no"), shelltest.Exit(1))
	calls := []toolCall{{Tool: "bash", Args: shellArgsJSON(t, map[string]any{"command": cmd})}}
	res := r.parallelExec(context.Background(), calls, map[string]struct{}{"bash": {}}, nil)
	if res[0].status != "error" {
		t.Fatalf("status = %q, want error", res[0].status)
	}
	if !strings.Contains(res[0].output, "compiler says no") || !strings.HasSuffix(res[0].output, "[exit status 1]") {
		t.Fatalf("output = %q", res[0].output)
	}
}

func TestToolErrorOutputKeepsUsefulText(t *testing.T) {
	if got := toolErrorOutput("", fmt.Errorf("boom")); got != "error: boom" {
		t.Fatalf("empty output: %q", got)
	}
	if got := toolErrorOutput("git: conflict in a.go\n", fmt.Errorf("exit-worktree: exit status 1")); got != "git: conflict in a.go\nerror: exit-worktree: exit status 1" {
		t.Fatalf("partial output: %q", got)
	}
}

// The go-05/go-06 hang: a timed-out command whose grandchildren kept the
// output pipe open blocked the tool call for minutes. The call must return at
// its deadline with the partial output and a clear timeout message.
func TestShellTimeoutKillsProcessGroupAndKeepsPartialOutput(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX background-job syntax")
	}
	r := newShellTestRuntime(t)
	cmd := "echo started; (sleep 30; echo late) & sleep 30"
	start := time.Now()
	out, err := r.runShellTool(context.Background(), "bash", shellArgsJSON(t, map[string]any{"command": cmd, "timeout": 1}), "bash")
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("timed-out command blocked for %s", elapsed)
	}
	if err == nil {
		t.Fatal("timed-out command reported success")
	}
	if !strings.Contains(out, "started") {
		t.Fatalf("partial output lost: %q", out)
	}
	if !strings.HasSuffix(out, "[command timed out after 1s; process group killed]") {
		t.Fatalf("timeout not reported: %q", out)
	}
}

func TestShellCancelIsReported(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX sleep")
	}
	r := newShellTestRuntime(t)
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(200*time.Millisecond, cancel)
	out, err := r.runShellTool(ctx, "bash", shellArgsJSON(t, map[string]any{"command": "echo begun; sleep 30"}), "bash")
	if err == nil || !strings.Contains(out, "begun") || !strings.HasSuffix(out, "[command cancelled; process group killed]") {
		t.Fatalf("cancel not reported: err=%v out=%q", err, out)
	}
}

// A command that exits 0 while a background child still holds stdout must
// return promptly and as a success.
func TestShellBackgroundChildDoesNotHangSuccess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX background-job syntax")
	}
	r := newShellTestRuntime(t)
	start := time.Now()
	out, err := r.runShellTool(context.Background(), "bash", shellArgsJSON(t, map[string]any{"command": "echo ok; sleep 20 &"}), "bash")
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("lingering child blocked the call for %s", elapsed)
	}
	if err != nil {
		t.Fatalf("exit 0 reported as failure: %v (%q)", err, out)
	}
	if !strings.HasPrefix(out, "ok\n") {
		t.Fatalf("output = %q", out)
	}
}

// Argument shapes other harnesses send — cmd instead of command, a
// description, a timeout as a string — must run rather than fail on decoding.
func TestShellToolAcceptsForeignArgumentShapes(t *testing.T) {
	r := newShellTestRuntime(t)
	echo := shelltest.Echo("hi")
	for _, fields := range []map[string]any{
		{"command": echo, "description": "say hi"},
		{"cmd": echo},
		{"command": echo, "timeout": "30"},
	} {
		out, err := r.runShellTool(context.Background(), "bash", shellArgsJSON(t, fields), "bash")
		if err != nil {
			t.Fatalf("%v: %v", fields, err)
		}
		if strings.TrimSpace(out) != "hi" {
			t.Fatalf("%v: output %q", fields, out)
		}
	}
}

func TestShellTimeoutResolution(t *testing.T) {
	r := newShellTestRuntime(t)
	r.toolPolicies["bash"] = config.ToolSpec{ID: "bash", TimeoutSec: 120}
	cases := []struct {
		requested int
		want      time.Duration
	}{
		{0, 120 * time.Second},      // configured default
		{5, 5 * time.Second},        // per-call override
		{900, 600 * time.Second},    // clamped to the cap
		{120000, 120 * time.Second}, // milliseconds from another harness
		{1000, 600 * time.Second},   // under an hour is seconds, clamped
		{-4, 120 * time.Second},     // nonsense falls back to the default
	}
	for _, c := range cases {
		if got := r.shellTimeout("bash", c.requested); got != c.want {
			t.Errorf("shellTimeout(%d) = %s, want %s", c.requested, got, c.want)
		}
	}
	// A configured default above the cap raises the cap with it.
	r.goalMode = true
	r.shellTimeoutSec = 1800
	if got := r.shellTimeout("bash", 1500); got != 1500*time.Second {
		t.Errorf("goal-mode per-call timeout = %s, want 1500s", got)
	}
	// Within a raised ceiling a large value is still seconds, not milliseconds.
	r.shellTimeoutSec = 7200
	if got := r.shellTimeout("bash", 5400); got != 5400*time.Second {
		t.Errorf("goal-mode 5400 = %s, want 5400s", got)
	}
	if got := r.shellTimeout("bash", 900000); got != 900*time.Second {
		t.Errorf("goal-mode 900000 = %s, want 900s (milliseconds)", got)
	}
}

func TestForegroundShellCallClassification(t *testing.T) {
	r := newShellTestRuntime(t)
	if r.isForegroundShellCall(toolCall{Tool: "bash", Args: []byte(`{"command":"make","run_in_background":true}`)}) {
		t.Fatal("background job treated as foreground")
	}
	if r.isForegroundShellCall(toolCall{Tool: "bash", Args: []byte(`{"job_id":"job-1"}`)}) {
		t.Fatal("job polling treated as foreground")
	}
	if r.isForegroundShellCall(toolCall{Tool: "file-read", Args: []byte(`{"path":"x"}`)}) {
		t.Fatal("non-shell tool treated as shell")
	}
	if !r.isForegroundShellCall(toolCall{Tool: "bash", Args: []byte(`{"command":"go test ./...","timeout":300}`)}) {
		t.Fatal("foreground command not recognised")
	}
}

// newApprovalTestRuntime is a runtime that must ask before running bash, with
// the given default timeout; approve is the user's side of the prompt.
func newApprovalTestRuntime(t *testing.T, defaultSec int, approve func(context.Context) (ShellApprovalDecision, error)) (*toolRuntime, *bool) {
	t.Helper()
	r := newShellTestRuntime(t)
	r.permission = config.PermissionAskFirst
	r.toolPolicies["bash"] = config.ToolSpec{ID: "bash", TimeoutSec: defaultSec, RequiresApproval: true}
	asked := new(bool)
	r.shellApproval = func(ctx context.Context, _ ShellApprovalRequest) (ShellApprovalDecision, error) {
		*asked = true
		return approve(ctx)
	}
	return r, asked
}

// The approval prompt gets the tool's default window whatever per-call
// timeout the model asked for: a short command timeout must not shorten the
// time the user has to read the prompt.
func TestShellApprovalWindowIgnoresPerCallTimeout(t *testing.T) {
	var window time.Duration
	r, asked := newApprovalTestRuntime(t, 120, func(ctx context.Context) (ShellApprovalDecision, error) {
		if dl, ok := ctx.Deadline(); ok {
			window = time.Until(dl)
		}
		return ShellApprovalAllowOnce, nil
	})
	call := toolCall{Tool: "bash", Args: shellArgsJSON(t, map[string]any{"command": shelltest.Echo("hi"), "timeout": 5})}
	if _, err := r.executeWithTimeout(context.Background(), call, map[string]struct{}{"bash": {}}); err != nil {
		t.Fatal(err)
	}
	if !*asked {
		t.Fatal("command was not sent for approval")
	}
	if window < 110*time.Second || window > 120*time.Second {
		t.Fatalf("approval window = %s, want the 120s default", window)
	}
}

// Time spent waiting for approval is not charged to the command: it still
// gets its whole timeout once approved.
func TestShellTimeoutStartsAfterApproval(t *testing.T) {
	r, asked := newApprovalTestRuntime(t, 120, func(context.Context) (ShellApprovalDecision, error) {
		time.Sleep(1500 * time.Millisecond)
		return ShellApprovalAllowOnce, nil
	})
	cmd := shelltest.Join(shelltest.Sleep(time.Second), shelltest.Echo("end"))
	call := toolCall{Tool: "bash", Args: shellArgsJSON(t, map[string]any{"command": cmd, "timeout": 2})}
	out, err := r.executeWithTimeout(context.Background(), call, map[string]struct{}{"bash": {}})
	if err != nil {
		t.Fatalf("command lost its budget to the approval wait: %v (%q)", err, out)
	}
	if !*asked || !strings.Contains(out, "end") {
		t.Fatalf("asked=%v output %q", *asked, out)
	}
}

// An approval nobody answers expires after the default window with a clear
// error, and the command never runs.
func TestShellApprovalExpiryIsReported(t *testing.T) {
	r, _ := newApprovalTestRuntime(t, 1, func(ctx context.Context) (ShellApprovalDecision, error) {
		<-ctx.Done()
		return ShellApprovalDeny, ctx.Err()
	})
	call := toolCall{Tool: "bash", Args: shellArgsJSON(t, map[string]any{"command": shelltest.Echo("ran")})}
	out, err := r.executeWithTimeout(context.Background(), call, map[string]struct{}{"bash": {}})
	if err == nil || !strings.Contains(err.Error(), "no approval decision within 1s") {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(out, "ran") {
		t.Fatalf("command ran without approval: %q", out)
	}
}

// Large shell output keeps both ends and names the spool file holding all of it.
func TestShellOutputKeepsHeadAndTailWithSpoolPath(t *testing.T) {
	t.Cleanup(jobs.Spool().Cleanup)
	r := newShellTestRuntime(t)
	out, err := r.runShellTool(context.Background(), "bash", shellArgsJSON(t, map[string]any{"command": shelltest.ManyLines("log line ", 20000)}), "bash")
	if err != nil {
		t.Fatal(err)
	}
	if len(out) > shellOutputHistoryLimit+1 || len(out) < shellOutputHistoryLimit/2 {
		t.Fatalf("output length %d not near the %d budget", len(out), shellOutputHistoryLimit)
	}
	// PowerShell ends each line with CRLF; compare the text, not the line ending.
	text := strings.ReplaceAll(out, "\r\n", "\n")
	if !strings.HasPrefix(text, "log line 1\n") || !strings.HasSuffix(strings.TrimSpace(text), "log line 20000") {
		t.Fatalf("head or tail missing: %q ... %q", out[:40], out[len(out)-40:])
	}
	if !strings.Contains(out, "full output saved to ") {
		t.Fatalf("spool path not noted: %q", out)
	}
}

// cwd/workdir (the spellings other harnesses use) change where the command
// runs; silently running it in the workspace root would act on the wrong
// directory. They are confined to the workspace like any path.
func TestShellToolHonoursWorkingDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a POSIX shell")
	}
	r := newShellTestRuntime(t)
	if err := os.MkdirAll(filepath.Join(r.cwd, "frontend"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"cwd", "workdir"} {
		marker := "marker_" + key
		_, err := r.runShellTool(context.Background(), "bash", shellArgsJSON(t, map[string]any{"command": "touch " + marker, key: "frontend"}), "bash")
		if err != nil {
			t.Fatalf("%s: %v", key, err)
		}
		if _, err := os.Stat(filepath.Join(r.cwd, "frontend", marker)); err != nil {
			t.Fatalf("%s: command did not run in frontend/", key)
		}
	}
	for _, dir := range []string{"..", "missing"} {
		if _, err := r.runShellTool(context.Background(), "bash", shellArgsJSON(t, map[string]any{"command": "true", "cwd": dir}), "bash"); err == nil {
			t.Fatalf("cwd %q accepted", dir)
		}
	}
}

// A process left running with & is reported, and the call does not wait
// out the full pipe delay for it.
func TestShellToolReportsLeftoverBackgroundProcess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a POSIX shell")
	}
	r := newShellTestRuntime(t)
	start := time.Now()
	out, err := r.runShellTool(context.Background(), "bash", shellArgsJSON(t, map[string]any{"command": "sleep 30 & echo started"}), "bash")
	defer shell.KillAllProcessTrees()
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("call took %s", time.Since(start))
	}
	if !strings.Contains(out, "started") || !strings.Contains(out, "run_in_background") {
		t.Fatalf("output = %q", out)
	}
}
