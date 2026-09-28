package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"spettro/internal/agent"
)

// Found in a VHS screenshot at 40x15: the approval for a file-write to a deep
// path showed "$ file-write src/components/very/…", with the diff preview
// hidden for lack of room, so nothing on screen named the file. The summary
// row now cuts the middle of the path and keeps the file name.
func TestFileApprovalSummaryKeepsTheFileName(t *testing.T) {
	path := "src/components/very/deeply/nested/module/with/a/really/long/path/new_file_with_a_long_name.txt"
	req := agent.ShellApprovalRequest{
		ToolID:  "file-write",
		Command: "file-write " + path,
		Reason:  "file modification requires approval",
		Diff:    "--- /dev/null\n+++ b/" + path + "\n@@ -0,0 +1 @@\n+x\n",
	}
	for _, size := range fitSizes {
		m := approvalModel(size[0], size[1], req)
		frame := m.View().Content
		assertFrameFits(t, "file approval", frame, size[0], size[1])
		plain := ansi.Strip(frame)
		if !strings.Contains(plain, "$ file-write ") || !strings.Contains(plain, "long_name.txt") {
			t.Fatalf("at %v the summary row does not name the file:\n%s", size, plain)
		}
	}

	// A shell command is still cut at its end: its start is what matters,
	// and the preview shows the rest. A cut row says so.
	row := approvalSummaryRow(agent.ShellApprovalRequest{ToolID: "bash", Command: "bash " + strings.Repeat("x", 200)}, 40)
	if got := ansi.Strip(row); !strings.HasPrefix(got, "  $ bash xxx") || !strings.HasSuffix(got, "…"+approvalCutMarker) {
		t.Fatalf("shell summary row = %q, want it cut at the end", got)
	}
}
