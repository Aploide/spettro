package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"spettro/internal/config"
	"spettro/internal/provider"
)

func TestAgentTimeoutDefaults(t *testing.T) {
	coder := config.AgentSpec{ID: "code", AllowedTools: []string{"file-read", "file-edit", "bash"}}
	explorer := config.AgentSpec{ID: "explore", AllowedTools: []string{"file-read", "grep", "glob"}}
	withAgentTimeout := func(sec int) *toolRuntime {
		r := &toolRuntime{toolPolicies: map[string]config.ToolSpec{}}
		if sec >= 0 {
			r.toolPolicies["agent"] = config.ToolSpec{ID: "agent", TimeoutSec: sec}
		}
		return r
	}
	cases := []struct {
		name  string
		sec   int // agent tool timeout_sec; -1 = no agent tool spec
		spec  config.AgentSpec
		limit time.Duration
	}{
		{"shipped default, coding worker", shippedAgentTimeoutSec, coder, codingAgentTimeoutSec * time.Second},
		{"shipped default, read-only worker", shippedAgentTimeoutSec, explorer, shippedAgentTimeoutSec * time.Second},
		{"unset, coding worker", 0, coder, codingAgentTimeoutSec * time.Second},
		{"no spec, coding worker", -1, coder, codingAgentTimeoutSec * time.Second},
		{"user-tuned lower", 120, coder, 120 * time.Second},
		{"user-tuned higher", 1800, coder, 1800 * time.Second},
	}
	for _, c := range cases {
		if got := withAgentTimeout(c.sec).agentTimeout(c.spec); got != c.limit {
			t.Errorf("%s: agentTimeout = %s, want %s", c.name, got, c.limit)
		}
	}
}

func TestScheduleWrapUp(t *testing.T) {
	q := NewSteeringQueue()
	scheduleWrapUp(q, 20*time.Millisecond)
	deadline := time.Now().Add(2 * time.Second)
	for q.Len() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	msgs := q.Drain()
	if len(msgs) != 1 || !strings.Contains(msgs[0], "time limit") {
		t.Fatalf("wrap-up messages = %q", msgs)
	}

	quiet := NewSteeringQueue()
	stop := scheduleWrapUp(quiet, time.Hour)
	if !stop() {
		t.Fatal("stop did not cancel the pending wrap-up")
	}
	if quiet.Len() != 0 {
		t.Fatal("a stopped wrap-up was delivered")
	}
}

func TestPartialWorkFromTraces(t *testing.T) {
	traces := []ToolTrace{
		{Name: "file-edit", Status: "running", Args: `{"path":"a.go"}`},
		{Name: "file-edit", Status: "success", Args: `{"path":"a.go","old_string":"x","new_string":"y"}`},
		{Name: "file-write", Status: "error", Args: `{"path":"failed.go"}`},
		{Name: "multi-edit", Status: "success", Args: `{"file_path":"b.go"}`},
		{Name: "file-write", Status: "success", Args: `{"path":"a.go","content":"z"}`},
		{Name: "bash", Status: "success", Args: `{"command":"git status"}`},
		{Name: "bash", Status: "error", Args: `{"command":"go test ./..."}`},
	}
	if got, want := modifiedFilesFromTraces(traces), []string{"a.go", "b.go"}; !reflect.DeepEqual(got, want) {
		t.Errorf("modified files = %v, want %v", got, want)
	}
	if runtime.GOOS != "windows" {
		if got, want := shellCommandsFromTraces(traces, 10), []string{"go test ./..."}; !reflect.DeepEqual(got, want) {
			t.Errorf("shell commands = %v, want %v", got, want)
		}
	}
	msgs := []provider.Message{
		{Role: provider.RoleUser, Content: "task"},
		{Role: provider.RoleAssistant, Content: "Editing a.go now."},
		{Role: provider.RoleUser, ToolResults: []provider.ToolResult{{Output: "ok"}}},
		{Role: provider.RoleAssistant, Content: "<think>hmm</think>"},
	}
	if got := lastAssistantText(msgs); got != "Editing a.go now." {
		t.Errorf("last assistant text = %q", got)
	}
}

// A sub-agent that runs out of time hands the parent what it did — its last
// words and the files it wrote, flagged as timed out — instead of a bare
// deadline error, and its changes stay on disk.
func TestTimedOutSubagentReportsPartialWork(t *testing.T) {
	fastRetries(t)
	saved := subagentTimeUnit
	subagentTimeUnit = 100 * time.Millisecond
	t.Cleanup(func() { subagentTimeUnit = saved })

	pm, url, _ := newLoopServer(t,
		loopReply{toolName: "agent", toolArgs: `{"target":"code","task":"write the notes"}`},
		loopReply{content: "Writing notes.txt first.", toolName: "file-write", toolArgs: `{"path":"notes.txt","content":"partial"}`},
		loopReply{content: "never delivered", delay: 10 * time.Second},
		loopReply{content: "done"},
	)
	manifest := delegationManifest()
	manifest.Tools[0].TimeoutSec = 5 // x100ms: a user-tuned limit, honoured as is
	manifest.Tools = append(manifest.Tools, config.ToolSpec{ID: "file-write", Name: "File Writer", Kind: "builtin", Enabled: true, TimeoutSec: 30, PermittedActions: []string{"write"}})
	manifest.Agents[1].AllowedTools = []string{"file-write", "comment"}
	manifest.Agents[1].PermittedActions = []string{"read", "write"}
	if err := manifest.Validate(); err != nil {
		t.Fatal(err)
	}
	cwd := t.TempDir()
	ag := LLMAgent{
		Spec:            manifest.Agents[0],
		ProviderManager: pm,
		ProviderName:    func() string { return url },
		ModelName:       func() string { return "m" },
		CWD:             cwd,
		Manifest:        &manifest,
	}
	start := time.Now()
	result, err := ag.Run(context.Background(), "delegate")
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("run took %s: the sub-agent deadline was not applied", elapsed)
	}
	var agentTrace *ToolTrace
	for i := range result.Tools {
		if result.Tools[i].Name == "agent" && result.Tools[i].Status != "running" {
			agentTrace = &result.Tools[i]
		}
	}
	if agentTrace == nil {
		t.Fatalf("no agent trace in %+v", result.Tools)
	}
	if agentTrace.Status != "error" {
		t.Fatalf("timed-out delegation reported as %q", agentTrace.Status)
	}
	var report struct {
		Status        string   `json:"status"`
		Partial       bool     `json:"partial"`
		Summary       string   `json:"summary"`
		FilesModified []string `json:"files_modified"`
	}
	if err := json.Unmarshal([]byte(agentTrace.Output), &report); err != nil {
		t.Fatalf("agent output is not the partial report: %v\n%s", err, agentTrace.Output)
	}
	if report.Status != "timed_out" || !report.Partial {
		t.Fatalf("report status = %q partial=%v, want timed_out/true", report.Status, report.Partial)
	}
	if report.Summary != "Writing notes.txt first." {
		t.Fatalf("summary = %q", report.Summary)
	}
	if !reflect.DeepEqual(report.FilesModified, []string{"notes.txt"}) {
		t.Fatalf("files_modified = %v", report.FilesModified)
	}
	if data, err := os.ReadFile(filepath.Join(cwd, "notes.txt")); err != nil || string(data) != "partial" {
		t.Fatalf("sub-agent's write was lost: %q, %v", data, err)
	}
}
