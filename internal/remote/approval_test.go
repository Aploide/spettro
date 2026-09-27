package remote

import (
	"context"
	"fmt"
	"net/http"
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

// publishedApprovals returns the approval_request events published so far.
func publishedApprovals(s *Server) []Event {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []Event
	for _, ev := range s.recent {
		if ev.Kind == "approval_request" {
			out = append(out, ev)
		}
	}
	return out
}

// Two approvals of the same tool can be pending at once (parallel
// sub-agents). Each event carries its own approval_id, and an answer naming
// it reaches that request and no other; an answer naming only the tool_id is
// refused while it is ambiguous rather than handed to whichever request
// registered last. Network approvals have no tool_id at all.
func TestApprovalAnswersReachTheRequestTheyName(t *testing.T) {
	for _, toolID := range []string{"bash", ""} {
		s := startTestServer(t)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		results := map[string]chan ApprovalDecision{"first": make(chan ApprovalDecision, 1), "second": make(chan ApprovalDecision, 1)}
		for i, name := range []string{"first", "second"} {
			go func() {
				dec, _ := s.RequestApproval(ctx, ApprovalRequest{ToolID: toolID, Command: "echo " + name})
				results[name] <- dec
			}()
			// Wait for its event, so the two register in a known order.
			deadline := time.Now().Add(2 * time.Second)
			for len(publishedApprovals(s)) < i+1 {
				if time.Now().After(deadline) {
					t.Fatal("approval not published")
				}
				time.Sleep(5 * time.Millisecond)
			}
		}
		events := publishedApprovals(s)
		ids := map[string]string{}
		for _, ev := range events {
			ids[ev.Data["command"].(string)], _ = ev.Data["approval_id"].(string)
		}
		if ids["echo first"] == "" || ids["echo first"] == ids["echo second"] {
			t.Fatalf("approval ids: %v", ids)
		}

		resp := doReq(t, s, http.MethodPost, "/approval", "test-token", fmt.Sprintf(`{"tool_id":%q,"decision":"allow-once"}`, toolID))
		if resp.StatusCode != http.StatusConflict {
			t.Fatalf("tool %q: an ambiguous tool_id answer got %d, want 409", toolID, resp.StatusCode)
		}
		resp = doReq(t, s, http.MethodPost, "/approval", "test-token", fmt.Sprintf(`{"approval_id":%q,"decision":"allow-once"}`, ids["echo first"]))
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("answer by approval_id = %d", resp.StatusCode)
		}
		if dec := <-results["first"]; dec.Decision != "allow-once" {
			t.Fatalf("first got %q", dec.Decision)
		}
		select {
		case dec := <-results["second"]:
			t.Fatalf("the second request was answered (%q) by an answer meant for the first", dec.Decision)
		case <-time.After(50 * time.Millisecond):
		}
		// With one left, a tool_id alone is unambiguous again.
		resp = doReq(t, s, http.MethodPost, "/approval", "test-token", fmt.Sprintf(`{"tool_id":%q,"decision":"deny"}`, toolID))
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("tool %q: unambiguous tool_id answer = %d", toolID, resp.StatusCode)
		}
		if dec := <-results["second"]; dec.Decision != "deny" {
			t.Fatalf("second got %q", dec.Decision)
		}
		cancel()
	}
}

// The text fields stay the exact bytes, and when they hold characters that
// do not show, the event says so and carries a copy with each written out.
func TestApprovalEventNamesHiddenCharacters(t *testing.T) {
	sneaky := "echo safe\rrm -rf ~ # \u202eevil\u202c"
	data := ApprovalEvent(ApprovalRequest{ToolID: "bash", Command: sneaky})
	if data["command"] != sneaky || data["command_hidden_chars"] != true {
		t.Fatalf("command=%q hidden=%v", data["command"], data["command_hidden_chars"])
	}
	if visible := data["command_visible"]; visible != `echo safe^Mrm -rf ~ # \u202eevil\u202c` {
		t.Fatalf("command_visible = %q", visible)
	}
	plain := ApprovalEvent(ApprovalRequest{ToolID: "bash", Command: "ls -la\n\tpwd"})
	if plain["command_hidden_chars"] != false {
		t.Fatal("a plain command was flagged")
	}
	if _, ok := plain["command_visible"]; ok {
		t.Fatal("a plain command got a visible copy")
	}
}
