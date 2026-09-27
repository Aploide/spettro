package tui

// Frame-fit tests: whatever the agent throws at the TUI (a file-write holding
// a whole file, a 50k-character heredoc, a 10k-character minified line, output
// full of tabs, colours and carriage returns, an approval for any of those),
// every frame View produces must fit the terminal exactly: no row wider than
// the terminal, no more rows than it has, and nothing hidden without a
// visible marker. Each test renders at several sizes, 40x15 (the smallest
// size the TUI promises to stay usable at) and 80x24 included.

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"spettro/internal/agent"
	"spettro/internal/session"
)

// fitSizes are the terminal sizes every frame-fit test renders at.
var fitSizes = [][2]int{{40, 15}, {80, 24}, {120, 40}, {200, 50}}

// assertFrameFits fails when the frame is taller or wider than the terminal,
// or still holds a tab or carriage return (whose width the terminal, not the
// layout, would decide).
func assertFrameFits(t *testing.T, name, frame string, width, height int) {
	t.Helper()
	lines := strings.Split(frame, "\n")
	if len(lines) > height {
		t.Fatalf("%s at %dx%d: frame is %d rows tall:\n%s", name, width, height, len(lines), ansi.Strip(frame))
	}
	for i, line := range lines {
		if w := ansi.StringWidth(line); w > width {
			t.Fatalf("%s at %dx%d: row %d is %d cells wide: %q", name, width, height, i, w, ansi.Strip(line))
		}
		if strings.ContainsAny(ansi.Strip(line), "\t\r") {
			t.Fatalf("%s at %dx%d: row %d holds a tab or carriage return: %q", name, width, height, i, line)
		}
	}
}

// hugeFile is a Go file of n tab-indented lines, the shape a file-write of
// generated code has.
func hugeFile(n int) string {
	var b strings.Builder
	for i := range n {
		fmt.Fprintf(&b, "\tfunc f%d() string { return %q }\n", i, strings.Repeat("x", 30))
	}
	return b.String()
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// hugeToolCalls is a turn's worth of tool calls at their worst: every
// argument and output far wider and longer than any terminal.
func hugeToolCalls() []ToolItem {
	file := hugeFile(2000)
	noSpace := strings.Repeat("A", 10000)
	nested := "{}"
	for i := range 200 {
		nested = fmt.Sprintf(`{"k%d":%s}`, i, nested)
	}
	return []ToolItem{
		{Name: "file-write", Status: "success", Output: "wrote 2000 lines",
			Args: mustJSON(map[string]any{"path": "internal/" + strings.Repeat("deep/", 40) + "file.go", "content": file})},
		{Name: "file-edit", Status: "success", Output: noSpace,
			Args: mustJSON(map[string]any{"path": "a.go", "edits": []map[string]string{{"old_string": file, "new_string": noSpace}}}),
			Diff: "--- a/a.go\n+++ b/a.go\n@@ -1,3 +1,3 @@\n-\t\t\told " + noSpace + "\n+\t\t\tnew " + strings.Repeat("宽", 300) + "\n \tctx\n"},
		{Name: "bash", Status: "success", Args: mustJSON(map[string]any{"command": "cat <<'EOF' > big.go\n" + file + "EOF"}),
			Output: "line1\n" + noSpace + "\n\tTabbed \x1b[31mred\x1b[0m\rprogress 50%\rprogress 100%\n[exit status 1]"},
		{Name: "bash", Status: "error", Args: mustJSON(map[string]any{"command": noSpace}), Output: strings.Repeat("out line with words\n", 500)},
		{Name: "mystery-tool", Status: "success", Args: nested, Output: nested},
		{Name: "grep", Status: "running", Args: mustJSON(map[string]any{"pattern": noSpace})},
	}
}

// hugeTranscriptModel is a model at width x height whose transcript and
// activity panel hold hugeToolCalls.
func hugeTranscriptModel(width, height int) Model {
	m := footerModel(width, height)
	m.messages = append(m.messages,
		ChatMessage{Role: RoleUser, Content: strings.Repeat("Z", 5000)},
		ChatMessage{Role: RoleAssistant, Content: "done " + strings.Repeat("B", 3000), Meta: strings.Repeat("meta ", 100), Tools: hugeToolCalls()},
	)
	for _, it := range hugeToolCalls() {
		m.applyToolTraceToObservability(agent.ToolTrace{Name: it.Name, Status: it.Status, Args: it.Args, Output: it.Output})
	}
	m.AddModifiedFileForTesting(strings.Repeat("very/long/path/", 20)+"x.go", 10, 2, false, true, false)
	m.gitBranch = strings.Repeat("feature-", 20)
	return m
}

// Huge tool calls fit every size in every display mode: collapsed, expanded
// (ctrl+o), full output (ctrl+g), with and without the side panel.
func TestFrameFitsWithHugeToolCalls(t *testing.T) {
	for _, size := range fitSizes {
		for _, mode := range []struct {
			name             string
			showTools, full  bool
			sidePanelVisible bool
		}{
			{"collapsed", false, false, false},
			{"expanded", true, false, false},
			{"full-output", true, true, false},
			{"expanded+panel", true, false, true},
			{"full-output+panel", true, true, true},
		} {
			m := hugeTranscriptModel(size[0], size[1])
			m.showTools, m.showFullOutput, m.showSidePanel = mode.showTools, mode.full, mode.sidePanelVisible
			m = m.recalcLayout()
			m.refreshViewport()
			for _, offset := range []int{0, 40, 1 << 20} {
				m.vp.SetYOffset(offset)
				assertFrameFits(t, mode.name, m.View().Content, size[0], size[1])
			}
			for i, line := range strings.Split(m.renderMessages(), "\n") {
				if w := ansi.StringWidth(line); w > m.transcriptWidth() {
					t.Fatalf("%s at %v: transcript row %d is %d cells, the viewport is %d: %q",
						mode.name, size, i, w, m.transcriptWidth(), ansi.Strip(line))
				}
			}
		}
	}
}

// What does not fit is marked, never silently dropped: a cut path keeps its
// file name, a cut output line says so and names the key that shows it whole,
// and the full-output view wraps instead of cutting.
func TestHugeToolCallsSayWhatTheyHide(t *testing.T) {
	m := hugeTranscriptModel(80, 24)
	m.showTools = true
	expanded := ansi.Strip(m.renderMessages())
	for _, want := range []string{
		"file.go",                          // the file name survives a very deep path
		"long lines cut · ctrl+g for full", // a 10k-character line was cut
		"more lines · ctrl+g for full",     // 500 output lines were capped
		"progress 100%",                    // a progress meter shows its final state
	} {
		if !strings.Contains(expanded, want) {
			t.Errorf("expanded transcript lacks %q", want)
		}
	}
	if strings.Contains(expanded, "\x1b[31m") || strings.Contains(expanded, "progress 50%") {
		t.Error("escape sequences or overwritten progress reached the transcript")
	}

	m.showFullOutput = true
	full := ansi.Strip(m.renderMessages())
	if strings.Contains(full, "long lines cut") || strings.Contains(full, "more lines") {
		t.Error("the full-output view still cuts or caps output")
	}
	if got := strings.Count(strings.ReplaceAll(full, "\n", ""), "A"); got < 10000 {
		t.Errorf("the full-output view lost part of the 10k-character output line: %d A's shown", got)
	}
}

// A multi-line command (a heredoc) is folded onto the label row instead of
// breaking it apart.
func TestToolLabelsAreOneRow(t *testing.T) {
	label := formatToolLabel("bash", mustJSON(map[string]any{"command": "cat <<'EOF' > x\n\tone\n\ttwo\nEOF"}))
	if strings.ContainsAny(label, "\n\t") {
		t.Fatalf("label still spans rows: %q", label)
	}
	if !strings.HasPrefix(label, "Ran $ cat <<'EOF' > x one two") {
		t.Fatalf("label = %q", label)
	}
	path := formatToolLabel("file-write", mustJSON(map[string]any{"path": strings.Repeat("deep/", 40) + "file.go"}))
	if !strings.HasSuffix(path, "/file.go") || ansi.StringWidth(path) > len("Wrote ")+labelPathWidth {
		t.Fatalf("a long path should be cut from the left: %q", path)
	}
}

// Tool names the engine renamed still render sensibly, whether a new session
// uses the canonical name or a resumed one replays the retired name.
func TestToolLabelsCanonicalAndRetiredNames(t *testing.T) {
	cases := []struct {
		name, args, done, running string
	}{
		{"lsp", `{"op":"diagnostics","path":"a.go"}`, "Checked diagnostics for a.go", "Checking diagnostics for a.go…"},
		{"lsp", `{"op":"references","path":"a.go","symbol":"Run"}`, "Found references to Run", "Finding references to Run…"},
		{"lsp", `{"op":"definition","path":"a.go","line":12}`, "Found definition of a.go:12", "Finding definition of a.go:12…"},
		{"lsp", `{"op":"hover","path":"a.go","symbol":"Run"}`, "Looked up Run", "Looking up Run…"},
		{"lsp", `{"op":"restart"}`, "Restarted language servers", "Restarting language servers…"},
		{"diagnostics", `{}`, "Checked diagnostics", "Checking diagnostics…"},
		{"references", `{"path":"a.go","symbol":"X","kind":"definition"}`, "Found definition of X", "Finding definition of X…"},
		{"hover", `{"path":"a.go","symbol":"X"}`, "Looked up X", "Looking up X…"},
		{"lsp-restart", `{"server":"gopls"}`, "Restarted language server gopls", "Restarting language server gopls…"},
		{"skill", `{"name":"pdf"}`, "Loaded skill pdf", "Loading skill pdf…"},
		{"skill", `{}`, "Listed skills", "Listing skills…"},
		{"skill-read", `{"skill":"pdf"}`, "Loaded skill pdf", "Loading skill pdf…"},
		{"activate-skill", `{"name":"pdf"}`, "Loaded skill pdf", "Loading skill pdf…"},
		{"skill-list", `{"query":"x"}`, "Listed skills", "Listing skills…"},
		{"shell-exec", `{"command":"ls"}`, "Ran $ ls", "Running $ ls…"},
		{"multi-edit", `{"path":"a.go"}`, "Edited a.go", "Editing a.go…"},
		{"task-create", `{"id":"t1"}`, "Created task t1", "Creating task…"},
		{"grok-image", `{"prompt":"x"}`, "Grok Image", "Using Grok Image…"},
	}
	for _, tc := range cases {
		if got := formatToolLabel(tc.name, tc.args); got != tc.done {
			t.Errorf("formatToolLabel(%s, %s) = %q, want %q", tc.name, tc.args, got, tc.done)
		}
		if got := formatRunningLabel(tc.name, tc.args); got != tc.running {
			t.Errorf("formatRunningLabel(%s, %s) = %q, want %q", tc.name, tc.args, got, tc.running)
		}
	}
	group := []ToolItem{{Name: "lsp", Args: `{"op":"hover"}`}, {Name: "lsp", Args: `{"op":"hover"}`}}
	if got := formatToolGroupLabel("lsp", group); got != "Queried the language server 2 times" {
		t.Errorf("lsp group label = %q", got)
	}
}

// The trace the agent emits after a permission decision reads as one.
func TestApprovalTraceLabel(t *testing.T) {
	got := formatToolLabel("approval", `{"decision":"allowed","source":"user","tool_id":"bash","reason":"approved once"}`)
	if got != "Approval: allowed by user (approved once)" {
		t.Fatalf("approval label = %q", got)
	}
}

// A transcript that follows the latest output keeps following it when the
// pane shrinks: an approval dialog opening used to leave the view stuck at
// the old bottom, and nothing new was shown until the user scrolled.
func TestTranscriptKeepsFollowingWhenThePaneShrinks(t *testing.T) {
	m := footerModel(100, 30)
	for i := range 60 {
		m.messages = append(m.messages, ChatMessage{Role: RoleUser, Content: fmt.Sprintf("message %d", i)})
	}
	m = m.recalcLayout()
	m.refreshViewport()
	if !m.vp.AtBottom() {
		t.Fatal("the transcript should start at the bottom")
	}
	m.pendingAuth = &shellApprovalRequestMsg{
		request:  agent.ShellApprovalRequest{ToolID: "bash", Command: "cat <<'EOF'\n" + strings.Repeat("x\n", 200) + "EOF"},
		response: make(chan shellApprovalResponse, 1),
	}
	m = m.recalcLayout()
	if !m.vp.AtBottom() || !strings.Contains(m.vp.View(), "message 59") {
		t.Fatalf("the latest message scrolled out of view when the dialog opened:\n%s", ansi.Strip(m.vp.View()))
	}

	// A user who scrolled up keeps their place.
	m.pendingAuth = nil
	m = m.recalcLayout()
	m.vp.SetYOffset(0)
	m.pendingAuth = &shellApprovalRequestMsg{request: agent.ShellApprovalRequest{ToolID: "bash", Command: "ls"}, response: make(chan shellApprovalResponse, 1)}
	m = m.recalcLayout()
	if m.vp.YOffset() != 0 {
		t.Fatalf("a scrolled-up transcript jumped to offset %d", m.vp.YOffset())
	}
}

// PgUp/PgDn scroll the transcript from the keyboard; back at the bottom the
// view follows new output again.
func TestPageKeysScrollTheTranscript(t *testing.T) {
	m := footerModel(100, 30)
	for i := range 80 {
		m.messages = append(m.messages, ChatMessage{Role: RoleUser, Content: fmt.Sprintf("message %d", i)})
	}
	m = m.recalcLayout()
	m.refreshViewport()
	next, _ := m.updateMain(tea.KeyPressMsg{Code: tea.KeyPgUp})
	m = next.(Model)
	if m.vp.AtBottom() {
		t.Fatal("pgup did not scroll the transcript")
	}
	for range 20 {
		next, _ = m.updateMain(tea.KeyPressMsg{Code: tea.KeyPgDown})
		m = next.(Model)
	}
	if !m.vp.AtBottom() {
		t.Fatal("pgdown did not return to the bottom")
	}
	m.messages = append(m.messages, ChatMessage{Role: RoleUser, Content: "newest"})
	m.refreshViewport()
	if !strings.Contains(m.vp.View(), "newest") {
		t.Fatal("back at the bottom, the transcript should follow new output")
	}
}

// A file diff computed in the background lands on an earlier tool row after
// later output is already on screen. The transcript has to stay at the
// bottom as that row grows, or following stops for the rest of the run.
func TestTranscriptKeepsFollowingWhenADiffArrives(t *testing.T) {
	m := footerModel(100, 30)
	m.showTools = true
	m.messages = append(m.messages, ChatMessage{Role: RoleAssistant, Tools: []ToolItem{{Name: "file-write", Status: "success", Args: `{"path":"a.go"}`, Seq: 7}}})
	for i := range 40 {
		m.messages = append(m.messages, ChatMessage{Role: RoleUser, Content: fmt.Sprintf("message %d", i)})
	}
	m = m.recalcLayout()
	m.refreshViewport()
	diff := "--- /dev/null\n+++ b/a.go\n@@ -0,0 +1,15 @@\n" + strings.Repeat("+line\n", 15)
	next, _ := m.update(toolDiffMsg{seq: 7, diff: diff})
	m = next.(Model)
	if !m.vp.AtBottom() || !strings.Contains(m.vp.View(), "message 39") {
		t.Fatalf("the transcript stopped following when a diff arrived:\n%s", ansi.Strip(m.vp.View()))
	}
}

// Markdown tables and code blocks wider than the transcript are fitted, not
// wrapped: a wrapped table border or code line lands on a row of its own and
// the table falls apart.
func TestMarkdownTablesAndCodeFit(t *testing.T) {
	md := "```go\n\tfunc long() { return \"" + strings.Repeat("L", 300) + "\" }\n```\n\n" +
		"| id | **description** | 宽 |\n|---|---|---|\n| 1 | " + strings.Repeat("cell ", 60) + " | 中文中文 |\n"
	for _, width := range []int{20, 40, 80, 118} {
		out := renderMarkdown(md, width)
		lines := strings.Split(out, "\n")
		for i, line := range lines {
			if w := ansi.StringWidth(line); w > width {
				t.Fatalf("width %d: row %d is %d cells: %q", width, i, w, ansi.Strip(line))
			}
		}
		plain := ansi.Strip(out)
		if !strings.Contains(plain, "…") {
			t.Fatalf("width %d: a cut must be marked:\n%s", width, plain)
		}
		// Every table row is one row: it starts and ends with a border.
		for _, line := range strings.Split(plain, "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "│") && !strings.HasSuffix(line, "│") && width >= 40 {
				t.Fatalf("width %d: a table row was split: %q", width, line)
			}
		}
	}
}

// A resumed session replays its tool events into the activity panel under
// whatever names they were recorded with; retired names and huge arguments
// must render inside the panel like any other.
func TestResumedRetiredToolNamesRender(t *testing.T) {
	var events []session.AgentEvent
	for _, name := range []string{"shell-exec", "multi-edit", "task-create", "task-update", "diagnostics", "grok-image", "skill-read", "repo-search", "ls"} {
		events = append(events, session.AgentEvent{
			Kind: "tool", AgentID: "coding", ToolName: name, Status: "success",
			ToolArgs:   mustJSON(map[string]any{"command": "cat <<'EOF'\n" + hugeFile(300) + "EOF", "path": strings.Repeat("d/", 100) + "f.go", "query": strings.Repeat("q", 500)}),
			ToolOutput: hugeFile(300),
		})
	}
	for _, width := range []int{110, 140, 200} {
		m := footerModel(width, 40)
		m.showSidePanel = true
		m.rebuildActivitiesFromEvents(events)
		m = m.recalcLayout()
		frame := m.View().Content
		assertFrameFits(t, "resumed session", frame, width, 40)
		if !strings.Contains(ansi.Strip(frame), "Ran $ cat <<'EOF'") {
			t.Fatalf("width %d: the replayed shell-exec call is not listed:\n%s", width, ansi.Strip(frame))
		}
	}
}

// approvalRequests are ask-first approvals at their worst.
func approvalRequests() map[string]agent.ShellApprovalRequest {
	var diff strings.Builder
	diff.WriteString("--- /dev/null\n+++ b/gen.go\n@@ -0,0 +1,2000 @@\n")
	for i := range 2000 {
		fmt.Fprintf(&diff, "+\tfunc f%d() { return %q }\n", i, strings.Repeat("x", 80))
	}
	return map[string]agent.ShellApprovalRequest{
		"heredoc": {ToolID: "bash", Reason: "non-whitelisted command requires approval",
			Command:  "cat <<'EOF' > x\n" + strings.Repeat("line of heredoc text\n", 3000) + "EOF",
			Segments: []string{strings.Repeat("s", 400)}},
		"one-long-line": {ToolID: "bash", Reason: strings.Repeat("why ", 200), Command: strings.Repeat("B", 20000)},
		"file-write": {ToolID: "file-write", Reason: "file modification requires approval",
			Command: "file-write " + strings.Repeat("dir/", 50) + "gen.go", Diff: diff.String()},
		"network": {ToolID: "web-fetch", Command: "network web-fetch https://" + strings.Repeat("a", 500)},
	}
}

func approvalModel(width, height int, req agent.ShellApprovalRequest) Model {
	m := footerModel(width, height)
	m.messages = append(m.messages, ChatMessage{Role: RoleUser, Content: "go"})
	m.pendingAuth = &shellApprovalRequestMsg{request: req, response: make(chan shellApprovalResponse, 1)}
	m.cfg.ShowPermissionDebug = true
	m = m.recalcLayout()
	m.refreshViewport()
	return m
}

// The approval dialog fits at every size, collapsed or expanded, picking or
// typing an alternative, and never loses its summary row or any option.
func TestApprovalDialogFitsWithHugeArguments(t *testing.T) {
	for name, req := range approvalRequests() {
		for _, size := range fitSizes[:3] {
			for _, expanded := range []bool{false, true} {
				for _, cursor := range []int{0, approvalInsteadOption} {
					m := approvalModel(size[0], size[1], req)
					m.approvalPreviewExpanded = expanded
					m.approvalCursor = cursor
					m = m.recalcLayout()
					frame := m.View().Content
					label := fmt.Sprintf("%s expanded=%v cursor=%d", name, expanded, cursor)
					assertFrameFits(t, label, frame, size[0], size[1])
					plain := ansi.Strip(frame)
					if !strings.Contains(plain, "  $ ") && !strings.Contains(plain, "Fetching") {
						t.Fatalf("%s at %v: the summary row is missing:\n%s", label, size, plain)
					}
					if cursor == 0 {
						for _, option := range []string{"allow this command?", "Allow once", "Allow always", "Deny", "Tell the agent"} {
							if !strings.Contains(plain, option) {
								t.Fatalf("%s at %v: %q is not on screen:\n%s", label, size, option, plain)
							}
						}
					} else if !strings.Contains(plain, "what to do instead") {
						t.Fatalf("%s at %v: the instead prompt is missing:\n%s", label, size, plain)
					}
					if name != "network" && !strings.Contains(plain, " of ") && !strings.Contains(plain, "preview hidden") {
						t.Fatalf("%s at %v: a partly shown preview must say so:\n%s", label, size, plain)
					}
				}
			}
		}
	}
}

// A dialog opened mid-run shares the screen with the working indicator and
// the todo/agent footer. On a short terminal the footer yields to the dialog
// (it was drawn at full size and pushed the dialog's bottom rows, then the
// status bar, off the screen).
func TestDialogsFitAlongsideTheRunFooter(t *testing.T) {
	var options []agent.AskUserOption
	for i := range 8 {
		options = append(options, agent.AskUserOption{Label: fmt.Sprintf("option %d %s", i, strings.Repeat("label ", 8))})
	}
	form := agent.AskUserForm{Questions: []agent.AskUserQuestion{{
		Question: strings.Repeat("Which one should the agent take next? ", 12), Options: options,
	}}}
	for _, size := range [][2]int{{40, 15}, {50, 18}, {80, 24}, {120, 40}} {
		base := func() Model {
			m := footerModel(size[0], size[1])
			m.messages = append(m.messages, ChatMessage{Role: RoleUser, Content: "go"})
			footerTodos(&m, 10)
			footerWorkers(&m, 3)
			m.thinking = true
			m.agentStartAt = time.Now()
			return m
		}
		for name, req := range approvalRequests() {
			m := base()
			m.pendingAuth = &shellApprovalRequestMsg{request: req, response: make(chan shellApprovalResponse, 1)}
			m = m.recalcLayout()
			frame := m.View().Content
			assertFrameFits(t, "approval "+name+" during a run", frame, size[0], size[1])
			if !strings.Contains(ansi.Strip(frame), "Tell the agent") {
				t.Fatalf("%v %s: the picker lost an option:\n%s", size, name, ansi.Strip(frame))
			}
		}
		m := base()
		m.SetPendingAskUserFormForTesting(form)
		m = m.recalcLayout()
		frame := m.View().Content
		assertFrameFits(t, "question during a run", frame, size[0], size[1])
		if !strings.Contains(ansi.Strip(frame), "pick") {
			t.Fatalf("%v: the question's key hint is missing:\n%s", size, ansi.Strip(frame))
		}
	}
}

// The preview scrolls with pgup/pgdn and expands with ctrl+o, staying inside
// the frame, so the whole of a huge command can be read before approving it.
func TestApprovalPreviewScrollsAndExpands(t *testing.T) {
	heredoc := agent.ShellApprovalRequest{ToolID: "bash", Command: "cat <<'EOF' > x\n" + strings.Repeat("line of heredoc text\n", 300) + "EOF"}
	m := approvalModel(80, 24, heredoc)
	footer := func(m Model) string {
		for _, line := range strings.Split(ansi.Strip(m.View().Content), "\n") {
			if strings.Contains(line, "lines ") && strings.Contains(line, " of ") {
				return strings.TrimSpace(strings.Trim(line, "│ "))
			}
		}
		t.Fatalf("no preview footer:\n%s", ansi.Strip(m.View().Content))
		return ""
	}
	press := func(m Model, key tea.KeyPressMsg) Model {
		next, _ := m.updateShellApproval(key)
		out := next.(Model).recalcLayout()
		assertFrameFits(t, "after "+key.String(), out.View().Content, 80, 24)
		return out
	}
	first := footer(m)
	if !strings.HasPrefix(first, "lines 1-") || !strings.Contains(first, "pgup/pgdn scroll") {
		t.Fatalf("initial footer = %q", first)
	}
	m = press(m, tea.KeyPressMsg{Code: tea.KeyPgDown})
	if got := footer(m); got == first || strings.HasPrefix(got, "lines 1-") {
		t.Fatalf("pgdown did not scroll: %q", got)
	}
	for range 400 {
		m = press(m, tea.KeyPressMsg{Code: tea.KeyPgDown})
		if strings.Contains(ansi.Strip(m.View().Content), "EOF") {
			break
		}
	}
	if !strings.Contains(ansi.Strip(m.View().Content), "EOF") {
		t.Fatal("pgdown never reached the end of the command")
	}
	for range 400 {
		m = press(m, tea.KeyPressMsg{Code: tea.KeyPgUp})
		if strings.HasPrefix(footer(m), "lines 1-") {
			break
		}
	}
	if got := footer(m); !strings.HasPrefix(got, "lines 1-") {
		t.Fatalf("pgup did not return to the top: %q", got)
	}
	collapsedRows := lineCount(m.View().Content, "line of heredoc text")
	m = press(m, tea.KeyPressMsg{Code: 'o', Mod: tea.ModCtrl})
	if !m.approvalPreviewExpanded || !strings.Contains(footer(m), "ctrl+o collapse") {
		t.Fatalf("ctrl+o did not expand the preview: %q", footer(m))
	}
	tall := approvalModel(80, 60, heredoc)
	tall.approvalPreviewExpanded = true
	tall = tall.recalcLayout()
	if got := lineCount(tall.View().Content, "line of heredoc text"); got <= collapsedRows {
		t.Fatalf("an expanded preview on a tall terminal shows %d rows, collapsed showed %d", got, collapsedRows)
	}
}

func lineCount(frame, substr string) int {
	n := 0
	for _, line := range strings.Split(frame, "\n") {
		if strings.Contains(line, substr) {
			n++
		}
	}
	return n
}

// A carriage return cannot hide part of a command waiting for approval: the
// dialog shows it as ^M instead of letting the terminal jump to column 0.
func TestApprovalShowsControlCharacters(t *testing.T) {
	m := approvalModel(100, 30, agent.ShellApprovalRequest{ToolID: "bash", Command: "rm -rf ~ #\recho hi"})
	if frame := ansi.Strip(m.View().Content); !strings.Contains(frame, "$ rm -rf ~ #^Mecho hi") {
		t.Fatalf("the carriage return was not made visible:\n%s", frame)
	}
}

// The preview is rendered once per width, not on every animation frame.
func TestApprovalPreviewIsCached(t *testing.T) {
	m := approvalModel(80, 24, approvalRequests()["file-write"])
	first := m.approvalPreviewLines(m.approvalContentWidth())
	again := m.approvalPreviewLines(m.approvalContentWidth())
	if len(first) == 0 || &first[0] != &again[0] {
		t.Fatal("the approval preview was rendered again for the same width")
	}
	m.width = 100
	if resized := m.approvalPreviewLines(m.approvalContentWidth()); &resized[0] == &first[0] {
		t.Fatal("a resize must re-render the preview")
	}
}

// The ask-user form fits with a question, options and a preview far larger
// than the terminal.
func TestAskUserDialogFitsWithHugeContent(t *testing.T) {
	var options []agent.AskUserOption
	for i := range 12 {
		options = append(options, agent.AskUserOption{
			Label:       fmt.Sprintf("option %d %s", i, strings.Repeat("long ", 50)),
			Description: strings.Repeat("desc ", 80) + strings.Repeat("Q", 300),
			Preview:     "\tcode\t" + strings.Repeat("P", 400) + "\n" + strings.Repeat("宽", 200),
		})
	}
	for _, size := range fitSizes {
		m := footerModel(size[0], size[1])
		m.SetPendingAskUserFormForTesting(agent.AskUserForm{Questions: []agent.AskUserQuestion{{
			Question: strings.Repeat("Why? ", 300) + strings.Repeat("W", 500),
			Header:   strings.Repeat("H", 100),
			Options:  options,
		}}})
		m = m.recalcLayout()
		assertFrameFits(t, "ask-user", m.View().Content, size[0], size[1])
	}
}

// The side panel takes exactly the column the layout gives it, whatever it
// lists, and its list starts on the row the mouse hit-testing expects.
func TestSidePanelFitsItsColumn(t *testing.T) {
	for _, width := range []int{110, 120, 150, 200} {
		m := hugeTranscriptModel(width, 40)
		m.showSidePanel = true
		for i := range 30 {
			m.AddModifiedFileForTesting(fmt.Sprintf("%s/file%d.go", strings.Repeat("pkg/", 30), i), i, i, false, true, false)
		}
		for i := range 12 {
			m.todos = append(m.todos, session.Todo{ID: fmt.Sprintf("t%d", i), Content: "task 宽宽 " + strings.Repeat("x", 200) + "\nsecond line", Status: session.TaskStatusPending})
		}
		m = m.recalcLayout()
		m.refreshViewport()
		frame := m.View().Content
		assertFrameFits(t, "side panel", frame, width, 40)
		panel := m.viewSidePanel(m.sidePanelWidth())
		for i, line := range strings.Split(panel, "\n") {
			if w := ansi.StringWidth(line); w > m.sidePanelWidth() {
				t.Fatalf("width %d: panel row %d is %d cells, its column is %d", width, i, w, m.sidePanelWidth())
			}
		}
		plain := ansi.Strip(frame)
		if !strings.Contains(plain, "todos") || !strings.Contains(plain, "… ") {
			t.Fatalf("width %d: the open panel must carry the task list and say what it hides:\n%s", width, plain)
		}
		startY, _ := m.sideListGeometry()
		rows := strings.Split(plain, "\n")
		if startY >= len(rows) || !strings.Contains(rows[startY], "coding") {
			t.Fatalf("width %d: the list's first row (an agent header) should be at y=%d, found %q", width, startY, rows[min(startY, len(rows)-1)])
		}
	}
}

// Task rows are cut by rune, never inside a multi-byte character.
func TestTodoRowsStayValidUTF8(t *testing.T) {
	m := footerModel(80, 30)
	m.todos = []session.Todo{{ID: "t1", Content: strings.Repeat("宽", 60), Status: session.TaskStatusPending}}
	for _, line := range m.todoLines(4) {
		if !utf8.ValidString(ansi.Strip(line)) {
			t.Fatalf("todo row is not valid UTF-8: %q", line)
		}
	}
}

// The header and the status bar are one row each, however narrow the
// terminal and however long the banner.
func TestHeaderAndStatusBarStayOneRow(t *testing.T) {
	for _, width := range []int{30, 40, 60, 80} {
		m := footerModel(width, 24)
		m.banner, m.bannerKind = strings.Repeat("provider error: ", 20)+"\ndetails on a second line", "error"
		if h := strings.Count(m.viewHeader(), "\n") + 1; h != 1 {
			t.Errorf("width %d: header is %d rows", width, h)
		}
		if h := strings.Count(m.viewStatusBar(m.paneWidth()), "\n") + 1; h != 1 {
			t.Errorf("width %d: status bar is %d rows", width, h)
		}
		assertFrameFits(t, "banner", m.View().Content, width, 24)
	}
}

// The @mention palette and the slash-command overlay shrink to the terminal
// instead of pushing the input box off the bottom.
func TestPalettesFitShortTerminals(t *testing.T) {
	for _, size := range [][2]int{{40, 12}, {40, 15}, {80, 24}} {
		m := footerModel(size[0], size[1])
		footerTodos(&m, 10)
		m.mentionKind = mentionFile
		m.mentionItems = []string{"a.go", strings.Repeat("dir/", 60) + "b.go", "c", "d", "e", "f", "g", "h"}
		m.mentionCursor = 6
		m = m.recalcLayout()
		frame := m.View().Content
		assertFrameFits(t, "mention palette", frame, size[0], size[1])
		if size[1] >= 15 && !strings.Contains(ansi.Strip(frame), "› g") {
			t.Fatalf("%v: the selected completion is not on screen:\n%s", size, ansi.Strip(frame))
		}

		m = footerModel(size[0], size[1])
		for i := range 30 {
			m.cmdItems = append(m.cmdItems, commandDef{name: fmt.Sprintf("/skill-%d-%s", i, strings.Repeat("n", 30)), desc: strings.Repeat("description 宽 ", 40)})
		}
		m.cmdCursor = 17
		m = m.recalcLayout()
		frame = m.View().Content
		assertFrameFits(t, "command overlay", frame, size[0], size[1])
		if !strings.Contains(ansi.Strip(frame), "/skill-17-") {
			t.Fatalf("%v: the selected command is not on screen:\n%s", size, ansi.Strip(frame))
		}
	}
}

// BenchmarkRenderMessagesHugeTools measures a transcript refresh (the work
// done on every streamed token) with 200 tool calls that each carry a whole
// file. Cache hits must stay cheap: they still hash every message.
func BenchmarkRenderMessagesHugeTools(b *testing.B) {
	m := footerModel(120, 40)
	m.showTools = true
	file := hugeFile(1500)
	args := mustJSON(map[string]any{"path": "a.go", "content": file})
	for i := range 200 {
		m.messages = append(m.messages, ChatMessage{Role: RoleAssistant, Content: "ok", Tools: []ToolItem{
			{Name: "file-write", Status: "success", Args: args, Output: fmt.Sprint(i) + file},
		}})
	}
	m.renderMessages()
	b.Run("cached", func(b *testing.B) {
		for range b.N {
			m.renderMessages()
		}
	})
	b.Run("cold", func(b *testing.B) {
		for range b.N {
			m.renderCache = nil
			m.renderMessages()
		}
	})
}
