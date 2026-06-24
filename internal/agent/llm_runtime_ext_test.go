package agent

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

func TestExtractDuckDuckGoResults(t *testing.T) {
	html := `
<div class="results">
  <a class="result__a" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fexample.com%2Fpost">Example Post</a>
  <a class="result__a" href="https://duckduckgo.com/l/?uddg=https%3A%2F%2Fgolang.org%2Fdoc">Go Docs</a>
  <a class="result__a" href="/l/?uddg=https%3A%2F%2Fexample.com%2Fpost">Duplicate Example</a>
</div>`
	rows := extractDuckDuckGoResults(html, 10)
	if len(rows) != 2 {
		t.Fatalf("expected 2 unique rows, got %d: %#v", len(rows), rows)
	}
	if !strings.Contains(rows[0], "Example Post") || !strings.Contains(rows[0], "https://example.com/post") {
		t.Fatalf("unexpected first row: %q", rows[0])
	}
	if !strings.Contains(rows[1], "Go Docs") || !strings.Contains(rows[1], "https://golang.org/doc") {
		t.Fatalf("unexpected second row: %q", rows[1])
	}
}

func TestResolveDuckDuckGoResultURL(t *testing.T) {
	got := resolveDuckDuckGoResultURL("//duckduckgo.com/l/?uddg=https%3A%2F%2Fexample.org%2Fpath")
	if got != "https://example.org/path" {
		t.Fatalf("unexpected resolved url: %q", got)
	}
	if resolveDuckDuckGoResultURL("https://duckduckgo.com/l/?x=1") != "" {
		t.Fatalf("expected empty for missing uddg")
	}
	if resolveDuckDuckGoResultURL("javascript:alert(1)") != "" {
		t.Fatalf("expected empty for invalid non-http link")
	}
}

func TestRunTaskStopMarksRuntime(t *testing.T) {
	rt := &toolRuntime{}
	raw, _ := json.Marshal(map[string]string{"reason": "stop now"})
	msg, err := rt.runTaskStop(raw)
	if err != nil {
		t.Fatalf("task-stop error: %v", err)
	}
	if msg != "stop now" {
		t.Fatalf("unexpected message: %q", msg)
	}
	if !rt.shouldStop() {
		t.Fatalf("expected stop requested")
	}
	if rt.stopMessage() != "stop now" {
		t.Fatalf("unexpected stop reason: %q", rt.stopMessage())
	}
}

func TestRunGoalCompleteMarksRuntime(t *testing.T) {
	rt := &toolRuntime{goalMode: true}
	raw, _ := json.Marshal(map[string]any{"summary": "all tests pass", "verified": true})
	msg, err := rt.runGoalComplete(raw)
	if err != nil {
		t.Fatalf("goal-complete error: %v", err)
	}
	if msg != "goal marked complete" {
		t.Fatalf("unexpected message: %q", msg)
	}
	if !rt.goalIsComplete() {
		t.Fatalf("expected goal complete")
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.goalSummary != "all tests pass" {
		t.Fatalf("unexpected summary: %q", rt.goalSummary)
	}
	if !rt.goalVerified {
		t.Fatalf("expected goalVerified to be true")
	}
}

func TestRunGoalCompleteRejectedOutsideGoalMode(t *testing.T) {
	rt := &toolRuntime{goalMode: false}
	raw, _ := json.Marshal(map[string]any{"summary": "done"})
	_, err := rt.runGoalComplete(raw)
	if err == nil {
		t.Fatalf("expected error when goal-complete called outside goal mode")
	}
	if !strings.Contains(err.Error(), "only available in goal mode") {
		t.Fatalf("unexpected error: %v", err)
	}
	if rt.goalIsComplete() {
		t.Fatalf("goal should not be marked complete in non-goal mode")
	}
}

func TestRunConfigToolSetAndGetPermission(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	rt := &toolRuntime{cwd: filepath.Join(home, "repo")}

	// First set should persist because key is not preset yet.
	setRaw, _ := json.Marshal(map[string]string{
		"action": "set",
		"key":    "permission",
		"value":  "restricted",
	})
	if _, err := rt.runConfigTool(setRaw); err != nil {
		t.Fatalf("config set error: %v", err)
	}

	getRaw, _ := json.Marshal(map[string]string{
		"action": "get",
		"key":    "permission",
	})
	out, err := rt.runConfigTool(getRaw)
	if err != nil {
		t.Fatalf("config get error: %v", err)
	}
	if out != "permission=restricted" {
		t.Fatalf("unexpected output: %q", out)
	}

	// Second set without force should not override preset values.
	setAgainRaw, _ := json.Marshal(map[string]string{
		"action": "set",
		"key":    "permission",
		"value":  "yolo",
	})
	out, err = rt.runConfigTool(setAgainRaw)
	if err != nil {
		t.Fatalf("config set again error: %v", err)
	}
	if !strings.Contains(out, "preset; unchanged") {
		t.Fatalf("expected preset unchanged message, got %q", out)
	}
	getRaw, _ = json.Marshal(map[string]string{
		"action": "get",
		"key":    "permission",
	})
	out, err = rt.runConfigTool(getRaw)
	if err != nil {
		t.Fatalf("config get error: %v", err)
	}
	if out != "permission=restricted" {
		t.Fatalf("expected unchanged preset permission, got %q", out)
	}

	// Force must override preset values.
	forceRaw, _ := json.Marshal(map[string]any{
		"action": "set",
		"key":    "permission",
		"value":  "yolo",
		"force":  true,
	})
	if _, err := rt.runConfigTool(forceRaw); err != nil {
		t.Fatalf("forced config set error: %v", err)
	}
	out, err = rt.runConfigTool(getRaw)
	if err != nil {
		t.Fatalf("config get after force error: %v", err)
	}
	if out != "permission=yolo" {
		t.Fatalf("expected forced permission yolo, got %q", out)
	}
}

func TestAuthorizeNetworkAccessAllowsWithoutApprovalInYolo(t *testing.T) {
	rt := &toolRuntime{
		cwd:        t.TempDir(),
		permission: "yolo",
	}
	if err := rt.authorizeNetworkAccess(context.Background(), "web-search", "example"); err != nil {
		t.Fatalf("expected no error in yolo mode, got %v", err)
	}
}
