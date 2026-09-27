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
