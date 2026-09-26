package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"

	"spettro/internal/config"
	"spettro/internal/jobs"
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
}

func TestForegroundShellTimeoutClassification(t *testing.T) {
	r := newShellTestRuntime(t)
	if _, ok := r.foregroundShellTimeout(toolCall{Tool: "bash", Args: []byte(`{"command":"make","run_in_background":true}`)}); ok {
		t.Fatal("background job treated as foreground")
	}
	if _, ok := r.foregroundShellTimeout(toolCall{Tool: "bash-output", Args: []byte(`{"job_id":"job-1"}`)}); ok {
		t.Fatal("job polling treated as foreground")
	}
	if _, ok := r.foregroundShellTimeout(toolCall{Tool: "file-read", Args: []byte(`{"path":"x"}`)}); ok {
		t.Fatal("non-shell tool treated as shell")
	}
	d, ok := r.foregroundShellTimeout(toolCall{Tool: "shell-exec", Args: []byte(`{"command":"go test ./...","timeout":300}`)})
	if !ok || d != 300*time.Second {
		t.Fatalf("foreground timeout = %s/%v, want 300s", d, ok)
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
	if !strings.HasPrefix(out, "log line 1\n") || !strings.HasSuffix(strings.TrimSpace(out), "log line 20000") {
		t.Fatalf("head or tail missing: %q ... %q", out[:40], out[len(out)-40:])
	}
	if !strings.Contains(out, "full output saved to ") {
		t.Fatalf("spool path not noted: %q", out)
	}
}
