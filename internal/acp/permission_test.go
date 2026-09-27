package acp

import (
	"strings"
	"testing"

	acpsdk "github.com/coder/acp-go-sdk"

	"spettro/internal/agent"
)

// openCard announces a running call on turn, the way the runtime's "running"
// trace does, and returns the ID of the card the editor was told about.
func openCard(t *testing.T, turn *turnState, agentID, name, args string) acpsdk.ToolCallId {
	t.Helper()
	turn.onTool(agent.ToolTrace{AgentID: agentID, Name: name, Status: "running", Args: args})
	queue := turn.open[agentID+"\x00"+name+"\x00"+args]
	if len(queue) == 0 {
		t.Fatalf("no open card for %s %s %s", agentID, name, args)
	}
	return queue[len(queue)-1].id
}

// cardFor is the card an approval request attaches to; it fails the test
// when the request would get a card of its own.
func cardFor(t *testing.T, turn *turnState, ar agent.ShellApprovalRequest) acpsdk.ToolCallId {
	t.Helper()
	id, attached := turn.approvalToolCallID(subjectOf(ar), ar)
	if !attached {
		t.Fatalf("approval %+v attached to no open card", ar)
	}
	return id
}

// The runtime accepts other harnesses' argument names (file_path for path,
// cmd for command), and the trace carries the arguments as the model wrote
// them, so an approval must find its card under either name.
func TestApprovalMatchesAliasedArguments(t *testing.T) {
	turn := newSilentTurn()
	turn.cwd = "/proj"
	editA := openCard(t, turn, "coding", "file-edit", `{"file_path":"a.txt","old_str":"x","new_str":"y"}`)
	openCard(t, turn, "coding", "file-edit", `{"file_path":"b.txt","old_str":"x","new_str":"y"}`)
	if got := cardFor(t, turn, agent.ShellApprovalRequest{
		ToolID: "file-edit", Command: "file-edit a.txt",
		Change: &agent.FileChange{Path: "/proj/a.txt"},
	}); got != editA {
		t.Errorf("edit approval for a.txt went to %s, want %s", got, editA)
	}

	runTests := openCard(t, turn, "coding", "bash", `{"cmd":"go test ./..."}`)
	openCard(t, turn, "coding", "bash", `{"cmd":"make"}`)
	if got := cardFor(t, turn, agent.ShellApprovalRequest{
		ToolID: "bash", Command: "go test ./...", Segments: []string{"go test ./..."},
	}); got != runTests {
		t.Errorf("command approval went to %s, want %s", got, runTests)
	}
}

// Sub-agents run in parallel, so several agents can have a card of the same
// tool open. An approval belongs to the agent that asked, and a relative
// path in its arguments is relative to that agent's directory (a worktree
// for isolated sub-agents), not the session's.
func TestApprovalStaysWithTheAskingAgent(t *testing.T) {
	turn := newSilentTurn()
	turn.cwd = "/proj"

	// Two workers in their own worktrees edit different files; the second
	// card is the newer one, so the old "most recent card" fallback picked it.
	editA := openCard(t, turn, "code#1", "file-edit", `{"path":"a.go","old_string":"x","new_string":"y"}`)
	openCard(t, turn, "code#2", "file-edit", `{"path":"b.go","old_string":"x","new_string":"y"}`)
	if got := cardFor(t, turn, agent.ShellApprovalRequest{
		ToolID: "file-edit", Command: "file-edit a.go", AgentID: "code#1", CWD: "/wt/1",
		Change: &agent.FileChange{Path: "/wt/1/a.go"},
	}); got != editA {
		t.Errorf("code#1's edit approval went to %s, want its own card %s", got, editA)
	}

	// Two workers run the same command: each approval lands on the asking
	// worker's card, never both on one.
	test1 := openCard(t, turn, "code#1", "bash", `{"command":"go test ./..."}`)
	test2 := openCard(t, turn, "code#2", "bash", `{"command":"go test ./..."}`)
	ask := func(agentID string) acpsdk.ToolCallId {
		return cardFor(t, turn, agent.ShellApprovalRequest{
			ToolID: "bash", Command: "go test ./...", Segments: []string{"go test ./..."}, AgentID: agentID,
		})
	}
	if got := ask("code#1"); got != test1 {
		t.Errorf("code#1's approval went to %s, want %s", got, test1)
	}
	if got := ask("code#2"); got != test2 {
		t.Errorf("code#2's approval went to %s, want %s", got, test2)
	}

	// An agent with no open card of that tool gets a card of its own rather
	// than another agent's.
	ar := agent.ShellApprovalRequest{ToolID: "bash", Command: "ls -R", Segments: []string{"ls -R"}, AgentID: "code#3"}
	if id, attached := turn.approvalToolCallID(subjectOf(ar), ar); attached {
		t.Errorf("code#3's approval attached to %s, which is another agent's card", id)
	}
}

// A card already showing a prompt belongs to a call blocked on that prompt,
// so it cannot be the call asking again: a second identical call of the same
// agent gets the second prompt, and the first card is free again once its
// prompt is answered.
func TestApprovalSkipsCardsAlreadyAwaitingAnAnswer(t *testing.T) {
	turn := newSilentTurn()
	first := openCard(t, turn, "coding", "bash", `{"command":"make"}`)
	second := openCard(t, turn, "coding", "bash", `{"command":"make"}`)
	ar := agent.ShellApprovalRequest{ToolID: "bash", Command: "make", Segments: []string{"make"}, AgentID: "coding"}

	a := cardFor(t, turn, ar)
	b := cardFor(t, turn, ar)
	if a == b || (a != first && a != second) || (b != first && b != second) {
		t.Fatalf("two open prompts share a card or miss both: %s, %s (cards %s, %s)", a, b, first, second)
	}
	if id, attached := turn.approvalToolCallID(subjectOf(ar), ar); attached {
		t.Errorf("a third prompt attached to %s while both cards await an answer", id)
	}
	turn.settleApprovalCard(a, true, agent.ShellApprovalAllowOnce)
	if got := cardFor(t, turn, ar); got != a {
		t.Errorf("after its prompt was answered, card %s must be free again, got %s", a, got)
	}
}

// Network approvals match the argument the runtime uses as the approval
// target, which is not always a URL.
func TestNetworkApprovalMatchesItsTarget(t *testing.T) {
	turn := newSilentTurn()
	golang := openCard(t, turn, "coding", "web-search", `{"query":"go   generics"}`)
	openCard(t, turn, "coding", "web-search", `{"query":"rust traits"}`)
	if got := cardFor(t, turn, agent.ShellApprovalRequest{Command: "network web-search go generics", AgentID: "coding"}); got != golang {
		t.Errorf("search approval went to %s, want %s", got, golang)
	}

	resource := openCard(t, turn, "coding", "mcp-read-resource", `{"server_id":"docs","resource_id":"readme"}`)
	openCard(t, turn, "coding", "mcp-read-resource", `{"server_id":"docs","resource_id":"changelog"}`)
	if got := cardFor(t, turn, agent.ShellApprovalRequest{Command: "network mcp-read-resource docs:readme", AgentID: "coding"}); got != resource {
		t.Errorf("resource approval went to %s, want %s", got, resource)
	}
}

// "Always allow" remembers the exact target the runtime asked about, so its
// label names that kind of target: a web search remembers the query, not a
// site.
func TestAlwaysAllowLabelNamesWhatIsRemembered(t *testing.T) {
	cases := map[string]string{
		"network web-fetch https://example.com/a": "Always allow this URL",
		"network download https://example.com/f":  "Always allow this URL",
		"network web-search go generics":          "Always allow this search",
		"network mcp-list-resources docs":         "Always allow this MCP server",
		"network mcp-auth docs":                   "Always allow this MCP server",
		"network mcp-read-resource docs:readme":   "Always allow this MCP resource",
	}
	for command, want := range cases {
		if got := alwaysAllowLabel(subjectOf(agent.ShellApprovalRequest{Command: command})); got != want {
			t.Errorf("%s: label %q, want %q", command, got, want)
		}
	}
	command := subjectOf(agent.ShellApprovalRequest{ToolID: "bash", Command: "make", Segments: []string{"make"}})
	if got := alwaysAllowLabel(command); got != "Always allow this command" {
		t.Errorf("command label %q", got)
	}
}

// approvalTexts is the text of every text block of an approval prompt.
func approvalTexts(ar agent.ShellApprovalRequest) []string {
	var out []string
	for _, c := range approvalContent(ar) {
		if c.Content != nil && c.Content.Content.Text != nil {
			out = append(out, c.Content.Content.Text.Text)
		}
	}
	return out
}

// The prompt carries the whole command: the card's title and rawInput are
// clipped (120 runes, 2 KiB), so without this an editor showed a 50k
// heredoc as its first line and asked to approve it.
func TestApprovalContentCarriesTheWholeCommand(t *testing.T) {
	command := "cat <<'EOF' > out.txt\n" + strings.Repeat("line with ``` fences and text\n", 2000) + "EOF\nrm -rf build"
	texts := approvalTexts(agent.ShellApprovalRequest{ToolID: "bash", Command: command, Segments: []string{"cat", "rm -rf build"}, Reason: "non-whitelisted command"})
	if len(texts) != 2 || !strings.Contains(texts[0], command) || strings.Contains(texts[0], "truncated") {
		t.Fatalf("the command block does not hold the whole command (%d blocks)", len(texts))
	}
	// The fence is longer than the backtick runs inside, so the command
	// cannot close it and show its tail as prose.
	if !strings.HasPrefix(texts[0], "````sh\n") || !strings.HasSuffix(texts[0], "\n````") {
		t.Fatalf("fence: %.20q ... %.20q", texts[0], texts[0][len(texts[0])-20:])
	}
	if !strings.Contains(texts[1], "needs approval: cat | rm -rf build") {
		t.Fatalf("note = %q", texts[1])
	}

	network := approvalTexts(agent.ShellApprovalRequest{Command: "network web-fetch https://example.com/?q=" + strings.Repeat("a", 5000) + "&key=TAIL"})
	if len(network) == 0 || !strings.Contains(network[0], "&key=TAIL") {
		t.Fatal("a network approval does not carry the whole target")
	}
}

// A diff shown as text (no structured change, or a file too large for one)
// is whole, where it used to be cut at 16 KiB.
func TestApprovalContentCarriesTheWholeDiff(t *testing.T) {
	diff := "--- /dev/null\n+++ b/big.txt\n@@ -0,0 +1,5000 @@\n" + strings.Repeat("+a line of the new file\n", 5000)
	for _, ar := range []agent.ShellApprovalRequest{
		{ToolID: "file-write", Command: "file-write big.txt", Diff: diff},
		{ToolID: "file-write", Command: "file-write big.txt", Diff: diff, Change: &agent.FileChange{Path: "/p/big.txt", Created: true, TextOmitted: true}},
	} {
		var found bool
		for _, text := range approvalTexts(ar) {
			if strings.Contains(text, strings.TrimRight(diff, "\n")) {
				found = true
			}
			if strings.Contains(text, "truncated") || strings.Contains(text, "more bytes not shown") {
				t.Fatalf("a %d-byte diff was cut", len(diff))
			}
		}
		if !found {
			t.Fatalf("the diff is not in the prompt whole (change=%v)", ar.Change != nil)
		}
	}
}

// Past the (very generous) bound the text is cut with an explicit note
// outside the code block.
func TestApprovalTextBlockSaysItWasCut(t *testing.T) {
	block := approvalTextBlock("sh", strings.Repeat("x", maxApprovalTextBytes+100))
	text := block.Content.Content.Text.Text
	if !strings.Contains(text, "\n```\n\n[truncated: 100 of ") || !strings.HasSuffix(text, "not the whole text]") {
		t.Fatalf("cut block ends %q", text[len(text)-120:])
	}
}
