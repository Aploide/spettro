package agent

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"spettro/internal/config"
	"spettro/internal/shell/shelltest"
)

// TestGrepPathScopesTheSearch pins the grep path argument the tool schema
// advertises: it must be accepted (not rejected as an unknown field) and limit
// the walk to that directory or file.
func TestGrepPathScopesTheSearch(t *testing.T) {
	cwd := t.TempDir()
	for rel, body := range map[string]string{
		"a/one.go":        "needle in a\n",
		"b/two.go":        "needle in b\n",
		"vendor/three.go": "needle in vendor\n",
	} {
		writeFileAt(t, filepath.Join(cwd, rel), body)
	}
	r := &toolRuntime{cwd: cwd, permission: config.PermissionYOLO, readSet: map[string]struct{}{}}
	grep := func(args string) (string, error) {
		return r.execute(context.Background(), toolCall{Tool: "grep", Args: json.RawMessage(args)}, map[string]struct{}{"grep": {}})
	}

	out, err := grep(`{"pattern":"needle","path":"a"}`)
	if err != nil {
		t.Fatalf("grep with path: %v", err)
	}
	if !strings.Contains(out, "a/one.go:1:") || strings.Contains(out, "two.go") {
		t.Fatalf("path=a must search only a/:\n%s", out)
	}
	if out, err = grep(`{"pattern":"needle","path":"b/two.go"}`); err != nil || !strings.Contains(out, "b/two.go:1:") || strings.Contains(out, "one.go") {
		t.Fatalf("path naming a file must search just that file: %v\n%s", err, out)
	}
	if out, err = grep(`{"pattern":"needle"}`); err != nil || strings.Contains(out, "vendor/") {
		t.Fatalf("the default walk skips vendor/: %v\n%s", err, out)
	}
	if out, err = grep(`{"pattern":"needle","path":"vendor"}`); err != nil || !strings.Contains(out, "vendor/three.go:1:") {
		t.Fatalf("an explicitly requested skipped directory must be searched: %v\n%s", err, out)
	}
	if _, err = grep(`{"pattern":"needle","path":"../"}`); err == nil || !strings.Contains(err.Error(), "outside workspace") {
		t.Fatalf("path outside the workspace must be refused, got %v", err)
	}
	if _, err = grep(`{"pattern":"needle","path":"missing"}`); err == nil {
		t.Fatal("a missing path must be reported, not searched as empty")
	}
}

// TestShellTimeoutArgumentIsHonored pins the timeout argument the shell
// schemas and prompts advertise: accepted, enforced, and reported as a timeout.
func TestShellTimeoutArgumentIsHonored(t *testing.T) {
	r := &toolRuntime{cwd: t.TempDir(), permission: config.PermissionYOLO, readSet: map[string]struct{}{}}
	args, err := json.Marshal(map[string]any{"command": shelltest.Echo("fast"), "timeout": 30})
	if err != nil {
		t.Fatal(err)
	}
	if out, err := r.runShellTool(context.Background(), "shell-exec", args, "shell-exec"); err != nil || !strings.Contains(out, "fast") {
		t.Fatalf("a timeout argument must be accepted: %v %q", err, out)
	}

	args, err = json.Marshal(map[string]any{"command": shelltest.Sleep(5 * time.Second), "timeout": 1})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, err = r.runShellTool(context.Background(), "bash", args, "bash")
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("expected a timeout error, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > 4500*time.Millisecond {
		t.Fatalf("timeout 1s was not enforced (took %s)", elapsed)
	}
}

// TestShellCallTimeoutArgument checks which calls own their deadline (a
// foreground shell command) and how a requested timeout is clamped, including
// a manifest that grants a tool more than the default cap.
func TestShellCallTimeoutArgument(t *testing.T) {
	r := &toolRuntime{toolPolicies: map[string]config.ToolSpec{"bash": {TimeoutSec: 1200}}}
	foreground := []struct {
		tool, args string
		want       bool
	}{
		{"shell-exec", `{"command":"go test ./...","timeout":300}`, true},
		{"shell-exec", `{"command":"x"}`, true},
		{"shell-exec", `{"command":"x","timeout":60,"run_in_background":true}`, false},
		{"bash-output", `{"job_id":"job-1","timeout":60}`, false},
		{"file-read", `{"path":"x","timeout":60}`, false},
	}
	for _, c := range foreground {
		if got := r.isForegroundShellCall(toolCall{Tool: c.tool, Args: json.RawMessage(c.args)}); got != c.want {
			t.Errorf("isForegroundShellCall(%s %s) = %v, want %v", c.tool, c.args, got, c.want)
		}
	}
	if got := r.shellTimeout("shell-exec", 300); got != 300*time.Second {
		t.Errorf("timeout 300 = %s, want 300s", got)
	}
	if got := r.shellTimeout("shell-exec", 1000); got != maxShellTimeoutSec*time.Second {
		t.Errorf("timeout 1000 = %s, want the %ds cap", got, maxShellTimeoutSec)
	}
	if got := r.shellTimeout("bash", 900); got != 900*time.Second {
		t.Errorf("manifest grants bash 1200s, so timeout 900 = %s, want 900s", got)
	}
}

// TestShellTimeoutRaisesToolDeadline checks that a timeout above the default
// per-tool limit is not cut short by executeWithTimeout's own deadline.
func TestShellTimeoutRaisesToolDeadline(t *testing.T) {
	r := &toolRuntime{cwd: t.TempDir(), permission: config.PermissionYOLO, readSet: map[string]struct{}{},
		toolPolicies: map[string]config.ToolSpec{"shell-exec": {TimeoutSec: 1}}}
	args, err := json.Marshal(map[string]any{"command": shelltest.Join(shelltest.Sleep(1500*time.Millisecond), shelltest.Echo("finished")), "timeout": 10})
	if err != nil {
		t.Fatal(err)
	}
	out, err := r.executeWithTimeout(context.Background(), toolCall{Tool: "shell-exec", Args: args}, map[string]struct{}{"shell-exec": {}})
	if err != nil || !strings.Contains(out, "finished") {
		t.Fatalf("timeout=10 must outlast the 1s tool default: %v %q", err, out)
	}
}
