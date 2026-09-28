package agent

import (
	"context"
	"strings"
	"testing"

	"spettro/internal/config"
)

func TestFileChangeSinkRecordsPerCall(t *testing.T) {
	// No sink: recording is a no-op.
	recordFileChange(context.Background(), "/p/a", "old", "new", false)

	ctx, sink := withFileChangeSink(context.Background())
	recordFileChange(ctx, "/p/a", "old", "new", false)
	recordFileChange(ctx, "/p/b", "", "created", true)
	got := sink.list()
	if len(got) != 2 || got[0] != (FileChange{Path: "/p/a", OldText: "old", NewText: "new"}) ||
		got[1] != (FileChange{Path: "/p/b", NewText: "created", Created: true}) {
		t.Fatalf("changes = %+v", got)
	}
	if (*fileChangeSink)(nil).list() != nil {
		t.Error("a nil sink lists nothing")
	}
}

// A change to a very large file keeps its path but drops both texts.
func TestFileChangeDropsOversizedTexts(t *testing.T) {
	big := strings.Repeat("x", maxFileChangeText+1)
	for _, c := range []FileChange{
		newFileChange("/p/a", big, "small", false),
		newFileChange("/p/a", "small", big, false),
	} {
		if !c.TextOmitted || c.OldText != "" || c.NewText != "" || c.Path != "/p/a" {
			t.Errorf("oversized change kept its texts: path=%q omitted=%v", c.Path, c.TextOmitted)
		}
	}
	if c := newFileChange("/p/a", "a", "b", false); c.TextOmitted {
		t.Error("a small change must keep its texts")
	}
}

// A write approval carries the structured change next to the unified diff,
// and the diff is computed from the full texts even when the change drops
// them.
func TestAuthorizeFileChangeCarriesTheChange(t *testing.T) {
	var got ShellApprovalRequest
	rt := &toolRuntime{
		permission:   config.PermissionAskFirst,
		toolPolicies: map[string]config.ToolSpec{"file-edit": {RequiresApproval: true}},
		shellApproval: func(_ context.Context, req ShellApprovalRequest) (ShellApprovalDecision, error) {
			got = req
			return ShellApprovalAllowOnce, nil
		},
	}
	if err := rt.authorizeFileChange(context.Background(), "file-edit", "/p/a.txt", "a.txt", "one\n", "two\n", false); err != nil {
		t.Fatal(err)
	}
	if got.Change == nil || got.Change.Path != "/p/a.txt" || got.Change.OldText != "one\n" || got.Change.NewText != "two\n" {
		t.Fatalf("change = %+v", got.Change)
	}
	if !strings.Contains(got.Diff, "-one") || !strings.Contains(got.Diff, "+two") {
		t.Errorf("diff = %q", got.Diff)
	}

	big := strings.Repeat("line\n", maxFileChangeText/5+10)
	if err := rt.authorizeFileChange(context.Background(), "file-edit", "/p/a.txt", "a.txt", big, big+"tail\n", false); err != nil {
		t.Fatal(err)
	}
	if got.Change == nil || !got.Change.TextOmitted || !strings.Contains(got.Diff, "+tail") {
		t.Errorf("large change: omitted=%v diff has tail=%v", got.Change != nil && got.Change.TextOmitted, strings.Contains(got.Diff, "+tail"))
	}
}

// Every kind of approval request names the agent asking and its working
// directory, so a host running sub-agents in parallel can show the request
// on that agent's tool call (see ShellApprovalRequest.AgentID).
func TestApprovalRequestsNameTheAskingAgent(t *testing.T) {
	var got []ShellApprovalRequest
	cwd := t.TempDir()
	rt := &toolRuntime{
		cwd:          cwd,
		agentID:      "code",
		instanceID:   "code#2",
		permission:   config.PermissionAskFirst,
		allowedShell: map[string]struct{}{},
		toolPolicies: map[string]config.ToolSpec{
			"file-edit": {RequiresApproval: true},
			"web-fetch": {RequiresApproval: true},
		},
		shellApproval: func(_ context.Context, req ShellApprovalRequest) (ShellApprovalDecision, error) {
			got = append(got, req)
			return ShellApprovalAllowOnce, nil
		},
	}
	ctx := context.Background()
	if err := rt.authorizeShellCommand(ctx, "bash", "npm run build"); err != nil {
		t.Fatal(err)
	}
	if err := rt.authorizeFileChange(ctx, "file-edit", cwd+"/a.txt", "a.txt", "one\n", "two\n", false); err != nil {
		t.Fatal(err)
	}
	if err := rt.authorizeNetworkAccess(ctx, "web-fetch", "https://example.com"); err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("want 3 approval requests, got %d", len(got))
	}
	for _, req := range got {
		if req.AgentID != "code#2" || req.CWD != cwd {
			t.Errorf("request %q: agent %q cwd %q, want code#2 in %s", req.Command, req.AgentID, req.CWD, cwd)
		}
	}
}
