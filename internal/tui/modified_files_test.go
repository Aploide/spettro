package tui

import (
	"testing"
	"time"
)

// TestScheduleModifiedRefreshThrottles verifies the ≥1s guard: the first call
// returns a cmd, an immediate second call returns nil, and after the interval
// elapses it returns a cmd again.
func TestScheduleModifiedRefreshThrottles(t *testing.T) {
	m := NewModelForTesting()

	if cmd := m.scheduleModifiedRefresh(); cmd == nil {
		t.Fatal("first scheduleModifiedRefresh should return a cmd")
	}
	if cmd := m.scheduleModifiedRefresh(); cmd != nil {
		t.Fatal("scheduleModifiedRefresh within the interval should return nil")
	}
	// Simulate the interval elapsing.
	m.lastModifiedRefreshAt = time.Now().Add(-minModifiedRefreshInterval - time.Second)
	if cmd := m.scheduleModifiedRefresh(); cmd == nil {
		t.Fatal("scheduleModifiedRefresh after the interval should return a cmd")
	}
}

// TestScheduleRepoScanThrottles verifies the ≥2s guard for the repo-file
// scan that feeds @-mention suggestions: the first call returns a cmd, an
// immediate second call returns nil, and after the interval elapses it
// returns a cmd again.
func TestScheduleRepoScanThrottles(t *testing.T) {
	m := NewModelForTesting()

	if cmd := m.scheduleRepoScan(); cmd == nil {
		t.Fatal("first scheduleRepoScan should return a cmd")
	}
	if cmd := m.scheduleRepoScan(); cmd != nil {
		t.Fatal("scheduleRepoScan within the interval should return nil")
	}
	// Simulate the interval elapsing.
	m.lastRepoScanAt = time.Now().Add(-minRepoScanInterval - time.Second)
	if cmd := m.scheduleRepoScan(); cmd == nil {
		t.Fatal("scheduleRepoScan after the interval should return a cmd")
	}
}

// TestModifiedFilesMsgAppliesToModel verifies the async result is applied.
func TestModifiedFilesMsgAppliesToModel(t *testing.T) {
	m := NewModelForTesting()
	files := []modifiedFileEntry{{Path: "a.go", Added: 1}, {Path: "b.go", Deleted: 2}}
	out, _ := m.update(modifiedFilesMsg{branch: "feature", files: files})
	nm, ok := out.(Model)
	if !ok {
		t.Fatal("update should return a Model")
	}
	if nm.gitBranch != "feature" {
		t.Fatalf("expected branch feature, got %q", nm.gitBranch)
	}
	if len(nm.modifiedFiles) != 2 {
		t.Fatalf("expected 2 modified files, got %d", len(nm.modifiedFiles))
	}
}

// TestAttachToolDiffTargetsBySeq verifies an async diff lands on the right tool
// entry in both the rendered messages and the live-tools slice.
func TestAttachToolDiffTargetsBySeq(t *testing.T) {
	m := NewModelForTesting()
	m.messages = []ChatMessage{
		{Role: RoleAssistant, Kind: "tool-stream", Tools: []ToolItem{
			{Name: "file-write", Args: `{"path":"x"}`, Status: "success", Seq: 1},
			{Name: "file-write", Args: `{"path":"y"}`, Status: "success", Seq: 2},
		}},
	}
	m.liveTools = []ToolItem{
		{Name: "file-write", Args: `{"path":"x"}`, Status: "success", Seq: 1},
		{Name: "file-write", Args: `{"path":"y"}`, Status: "success", Seq: 2},
	}

	m.attachToolDiff(2, "DIFF-FOR-Y")

	if m.messages[0].Tools[0].Diff != "" {
		t.Fatal("seq 1 entry should not receive the diff")
	}
	if m.messages[0].Tools[1].Diff != "DIFF-FOR-Y" {
		t.Fatalf("seq 2 message entry should receive the diff, got %q", m.messages[0].Tools[1].Diff)
	}
	if m.liveTools[1].Diff != "DIFF-FOR-Y" {
		t.Fatalf("seq 2 live-tool entry should receive the diff, got %q", m.liveTools[1].Diff)
	}
}

// TestToolDiffMsgIgnoresEmptyAndZeroSeq verifies the handler is a no-op for an
// empty diff or zero seq (computeFileDiff returns "" for non-edit tools).
func TestToolDiffMsgIgnoresEmptyAndZeroSeq(t *testing.T) {
	m := NewModelForTesting()
	m.messages = []ChatMessage{
		{Role: RoleAssistant, Kind: "tool-stream", Tools: []ToolItem{
			{Name: "shell-exec", Args: `{"command":"ls"}`, Status: "success", Seq: 1},
		}},
	}
	// Empty diff: nothing attached.
	out, _ := m.update(toolDiffMsg{seq: 1, diff: ""})
	nm, _ := out.(Model)
	if nm.messages[0].Tools[0].Diff != "" {
		t.Fatal("empty diff should not be attached")
	}
	// Zero seq: nothing attached.
	out, _ = nm.update(toolDiffMsg{seq: 0, diff: "x"})
	nm2, _ := out.(Model)
	if nm2.messages[0].Tools[0].Diff != "" {
		t.Fatal("zero seq should not attach anything")
	}
}
