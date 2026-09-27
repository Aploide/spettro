package remote

import (
	"context"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// An approval_request event carries the whole command and the whole diff,
// not a preview: a remote client may be where the user decides.
func TestApprovalEventCarriesEverything(t *testing.T) {
	command := "cat <<'EOF' > big.txt\n" + strings.Repeat("heredoc line\n", 5000) + "EOF"
	diff := "--- /dev/null\n+++ b/big.go\n@@ -0,0 +1,20000 @@\n" + strings.Repeat("+generated line of code\n", 20000)
	data := ApprovalEvent(ApprovalRequest{ToolID: "file-write", Command: command, Reason: "r", Segments: []string{"s"}, Diff: diff})
	if data["command"] != command || data["diff"] != diff {
		t.Fatal("the event does not carry the whole command and diff")
	}
	if data["command_truncated"] != false || data["diff_truncated"] != false {
		t.Fatalf("flags: %v %v", data["command_truncated"], data["diff_truncated"])
	}
	if data["diff_bytes"] != len(diff) || data["tool_id"] != "file-write" || data["reason"] != "r" {
		t.Fatalf("event: tool_id=%v reason=%v diff_bytes=%v", data["tool_id"], data["reason"], data["diff_bytes"])
	}
	if _, ok := ApprovalEvent(ApprovalRequest{ToolID: "bash", Command: "ls"})["diff"]; ok {
		t.Fatal("a command approval should have no diff field")
	}
}

// Past the bound a text is cut on a rune boundary and says so, in the text
// and in the flags, so no client can show it as the whole.
func TestClipApprovalTextSaysItWasCut(t *testing.T) {
	text := strings.Repeat("é", 100)
	got, cut := ClipApprovalText(text, 51)
	if !cut || !utf8.ValidString(got) || !strings.Contains(got, "[truncated: ") || !strings.Contains(got, "not the whole text") {
		t.Fatalf("clip = %q, %v", got, cut)
	}
	if !strings.HasPrefix(got, strings.Repeat("é", 25)+"\n") {
		t.Fatalf("clip kept the wrong prefix: %q", got)
	}
	if same, cut := ClipApprovalText("short", 51); cut || same != "short" {
		t.Fatal("a short text was changed")
	}

	huge := strings.Repeat("x", MaxApprovalFieldBytes+10)
	data := ApprovalEvent(ApprovalRequest{ToolID: "bash", Command: huge})
	if data["command_truncated"] != true || data["command_bytes"] != len(huge) || !strings.Contains(data["command"].(string), "[truncated: 10 of") {
		t.Fatalf("oversized command: truncated=%v bytes=%v", data["command_truncated"], data["command_bytes"])
	}
}

// RequestApproval publishes that event.
func TestRequestApprovalPublishesTheWholeRequest(t *testing.T) {
	s := startTestServer(t)
	diff := "--- a/a\n+++ b/a\n@@ -1 +1 @@\n-" + strings.Repeat("o", 50000) + "\n+" + strings.Repeat("n", 50000) + "\n"
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, _ = s.RequestApproval(ctx, ApprovalRequest{ToolID: "file-edit", Command: "file-edit a", Diff: diff})
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, ev := range s.recent {
		if ev.Kind == "approval_request" {
			if ev.Data["diff"] != diff || ev.Data["command"] != "file-edit a" {
				t.Fatal("the published approval is not the whole request")
			}
			return
		}
	}
	t.Fatal("no approval_request was published")
}
