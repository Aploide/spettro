package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/x/ansi"

	"spettro/internal/agent"
	"spettro/internal/diff"
	"spettro/internal/termtext"
	"spettro/internal/theme"
)

// The ask-first approval dialog replaces the text input while a tool call
// waits for the user's decision (see updateShellApproval for the keys). It
// shows, top to bottom:
//
//	◆ coding                          agent label
//	  $ cat <<'EOF' > big.txt … [cut] summary: the call on one row
//	  why: non-whitelisted command    reason
//	  remembers: go build · go test   what "Allow always" would remember, when
//	                                  that is not just the command (with
//	                                  permission debug on, always: the
//	                                  segments that needed approval)
//	    …                             preview: the diff of a file change, or
//	    …                             the full text of a command or network
//	  lines 1-8 of 3002 · v review …  target too long for the summary row;
//	                                  scrollable
//	  allow this command?             picker (or the "instead" text field)
//	  › Review full command (v)       only when something is not on screen,
//	    Allow once                    see dialog_approvals.go
//	    …
//
// Tool arguments have no size limit (a file-write carries a whole file, a
// bash call can be a 50k-character heredoc), while the dialog has to fit the
// terminal with every option still visible, at 40x15 as much as at 200x60.
// approvalLayout decides which of the optional rows get space; the preview
// is windowed and scrolled rather than wrapped past the bottom of the screen.
// The summary row and the picker are never dropped. Whatever the dialog cannot
// show (a preview capped, scrolled or hidden, a diff line cut, a summary row
// cut) is said on screen, and the full-screen review shows it: see
// dialog_approvals.go.

// Collapsed preview caps. ctrl+o lifts them (the preview then takes every
// row the terminal can spare); the cap only keeps a routine approval from
// hiding the transcript.
const (
	approvalDiffCollapsedLines    = 16
	approvalCommandCollapsedLines = 8
)

// approvalPreviewIndent prefixes the first row of each line of a command
// preview, under the "$" of the summary row. approvalPreviewWrapIndent
// prefixes the rows a line too long for the dialog continues on: the hook
// arrow says the row is the same line, not the command's next one (a line
// break separates two shell commands; a wrap separates nothing).
const (
	approvalPreviewIndent     = "    "
	approvalPreviewWrapIndent = "  ↪ "
)

// approvalPreviewCache memoises the fully rendered preview of the pending
// approval at one content width. View runs on every animation tick; wrapping
// a 50k-character command or rendering a 2000-line diff each time would make
// the dialog lag behind the keyboard. It lives on the pending request, so a
// new approval can never be shown with the previous one's preview.
type approvalPreviewCache struct {
	width int
	lines []string
	// overflow is set when a diff line is wider than the preview and was
	// cut with "…" (diff.Overflows).
	overflow bool
	// wrapped is set when a command line was hard-wrapped onto more than
	// one row, so the preview's rows are not its lines (see
	// approvalFooterText).
	wrapped bool
}

// approvalPreviewLines returns every display row of the pending approval's
// preview, each at most width cells wide, or nil when the call has nothing
// to preview beyond its summary row.
func (m Model) approvalPreviewLines(width int) []string {
	req := m.pendingAuth
	if req == nil {
		return nil
	}
	return m.approvalPreview(width).lines
}

// approvalPreview is the cached preview of the pending approval at width:
// its rows and whether any line was cut. The request must be pending.
func (m Model) approvalPreview(width int) *approvalPreviewCache {
	req := m.pendingAuth
	if c := req.previewCache; c != nil && c.width == width {
		return c
	}
	lines, overflow, wrapped := buildApprovalPreview(req.request, width)
	req.previewCache = &approvalPreviewCache{width: width, lines: lines, overflow: overflow, wrapped: wrapped}
	return req.previewCache
}

// buildApprovalPreview renders the preview rows for one approval request:
// the diff when the request carries one (file-write, file-edit), otherwise
// the full text of the summary row when it does not fit that row: the whole
// command, or for a network call the whole sentence naming its target.
// Control and invisible characters are made visible, not interpreted: the
// preview is what the user approves, so no character of it may be hidden. A
// command is escaped exactly (termtext.EscapeExact: a tab is not shown as
// spaces) and hard-wrapped (termtext.HardWrap: no space is dropped at a
// break, and none ends a row where it could not be seen), each continuation
// row marked as one. A diff is escaped exactly too (diff.Options.Exact). A
// diff line wider than the dialog is cut with "…",
// which overflow reports so the dialog can offer the review; wrapped reports
// that a command line took more than one row.
func buildApprovalPreview(req agent.ShellApprovalRequest, width int) (lines []string, overflow, wrapped bool) {
	if strings.TrimSpace(req.Diff) != "" {
		// Stay in the unified layout: the side-by-side one halves the room
		// each line gets, and a narrow dialog is where this is read most.
		// Exact, like the review: a script whose "<<-EOF" ends at a
		// tab-indented "EOF" and one whose terminator is indented with
		// spaces (so the lines after it are still heredoc data) must not
		// look the same. A tab mark keeps the tab's width, so the preview
		// reads as before.
		opts := diff.Options{
			Width:  min(width, diff.SideBySideMinWidth-1),
			Indent: "  ",
			Exact:  true,
		}
		return strings.Split(diff.Render(req.Diff, opts), "\n"), diff.Overflows(req.Diff, opts), false
	}
	label := formatApprovalCommandLabel(req.Command)
	text := trimShellBlanks(req.Command)
	if !strings.HasPrefix(label, "$ ") {
		// A network call: the label is one line (its target has no
		// newlines, see formatApprovalCommandLabel), previewed whole.
		text = label
	}
	if !strings.Contains(text, "\n") && approvalSummaryFits(label, width) {
		return nil, false, false
	}
	textW := max(width-len(approvalPreviewIndent), 8)
	for _, raw := range strings.Split(text, "\n") {
		for i, part := range termtext.HardWrap(termtext.EscapeExact(raw), textW) {
			indent := approvalPreviewIndent
			if i > 0 {
				indent = approvalPreviewWrapIndent
				wrapped = true
			}
			lines = append(lines, styleMuted.Render(indent+part))
		}
	}
	return lines, false, wrapped
}

// approvalSummaryFits reports whether a one-line summary label (see
// formatApprovalCommandLabel) is shown whole on the summary row, which is
// indented by two cells.
func approvalSummaryFits(label string, width int) bool {
	return ansi.StringWidth(approvalSummary(label))+2 <= width
}

// approvalLayout is the approval dialog cut to the rows the terminal has.
type approvalLayout struct {
	showReason   bool // the "why:" row
	showSegments bool // the "remembers:" (or permission-debug "segments:") row
	previewRows  int  // preview rows on screen; may be 0 when there is no room
	previewTotal int  // preview rows in all
	wrapped      bool // a command line takes more than one preview row
	previewStart int  // index of the first preview row on screen
	showFooter   bool // the "lines a-b of n" row under the preview
	canExpand    bool // ctrl+o would show more of the preview
	overflow     bool // a diff line in the preview is cut at the dialog's width
}

// approvalLayout decides which optional rows of the dialog get space, for a
// dialog whose content area is contentW cells wide, with the picker (or the
// "instead" field) as it is now.
func (m Model) approvalLayout(contentW int) approvalLayout {
	return m.approvalLayoutFor(contentW, m.approvalControlRows(contentW))
}

// approvalLayoutFor is approvalLayout with controlRows rows under the
// preview.
//
// The rows available to the dialog are the terminal height minus the header,
// the two separators, the status bar, the working indicator, the footer
// block (which yields to the dialog, see parallelFooterBudget), the input box
// border and a minimum of transcript. The summary row and the picker always
// get theirs. What is left goes, in order, to: the preview footer (so a
// preview is never hidden without saying so), the reason, the preview itself (up to its collapsed cap unless expanded), and the
// segments row. That row is shown when "Allow always" would remember more
// than the command (approvalRemembersOther) or permission debug is on; when
// it is needed and gets no room, the review offers what it would say.
func (m Model) approvalLayoutFor(contentW, controlRows int) approvalLayout {
	req := m.pendingAuth
	var lay approvalLayout
	if req == nil {
		return lay
	}
	preview := m.approvalPreview(contentW)
	lay.previewTotal = len(preview.lines)
	lay.overflow = preview.overflow
	lay.wrapped = preview.wrapped

	fixedChrome := 1 + 2 + 1 + m.workingIndicatorHeight() + m.parallelFooterHeight() +
		dialogMinTranscriptRows(m.height) + 2
	rows := m.height - fixedChrome - 1 - controlRows
	take := func() bool {
		if rows < 1 {
			return false
		}
		rows--
		return true
	}

	footerReserved := lay.previewTotal > 0 && take()
	lay.showReason = strings.TrimSpace(req.request.Reason) != "" && take()

	if lay.previewTotal > 0 {
		limit := approvalCommandCollapsedLines
		if req.request.Diff != "" {
			limit = approvalDiffCollapsedLines
		}
		room := max(rows, 0)
		if m.approvalPreviewExpanded {
			lay.previewRows = min(lay.previewTotal, room)
		} else {
			lay.previewRows = min(lay.previewTotal, room, limit)
			lay.canExpand = lay.previewRows == limit && lay.previewTotal > limit && room > limit
		}
		rows -= lay.previewRows
		lay.previewStart = clampOffset(m.approvalScroll, 0, lay.previewTotal-lay.previewRows)
		// The footer only has something to say when part of the preview is
		// off screen or cut; otherwise its row goes back to the pool.
		lay.showFooter = footerReserved && (lay.previewRows < lay.previewTotal || lay.overflow || m.approvalPreviewExpanded)
		if footerReserved && !lay.showFooter {
			rows++
		}
	}
	lay.showSegments = len(req.request.Segments) > 0 &&
		(m.cfg.ShowPermissionDebug || approvalRemembersOther(req.request)) && take()
	return lay
}

// approvalControlRows is the height of what sits under the preview: the
// picker (a title plus one row per option), or, once the user chose to tell
// the agent what to do instead, a prompt row and the text field.
func (m Model) approvalControlRows(contentW int) int {
	if m.approvalChoice == approvalActInstead {
		return 1 + m.ta.Height()
	}
	return 1 + len(m.approvalActions(contentW))
}

// approvalLatchedControlRows is approvalControlRows without deciding whether
// the review is offered: it reads the latch alone. The footer's row budget
// (dialogMinInputRows) is computed while the dialog is being laid out, and
// deciding would lay the dialog out again, so it takes the latch as it
// stands; once a request is on screen the latch is settled
// (syncApprovalReview).
func (m Model) approvalLatchedControlRows() int {
	if m.approvalChoice == approvalActInstead {
		return 1 + m.ta.Height()
	}
	n := len(approvalBaseActions)
	if m.pendingAuth != nil && m.pendingAuth.reviewOffered {
		n++
	}
	return 1 + n
}

// approvalHidesContent reports whether the dialog, drawn contentW cells wide
// with the plain four-option picker, leaves any part of the pending call off
// the screen: preview rows capped, scrolled away or hidden for lack of room,
// a diff line cut at the dialog's width, or a summary row cut where nothing
// else shows the text whole. A file's path cut on the summary row counts; a
// command or network target cut there does not when the preview under it is
// all on screen, because the preview is that same text, wrapped. The reason
// row is commentary, not what is approved, and does not count. What "Allow
// always" would remember does count when it is more than the command: it is
// part of what that choice approves, so a "remembers:" row that got no room,
// is cut, or cannot show its commands apart offers the review.
//
// It is asked with four options because the review option is what it
// decides: with the fifth row the dialog only has less room, so the answer
// cannot flip back.
func (m Model) approvalHidesContent(contentW int) bool {
	req := m.pendingAuth
	if req == nil {
		return false
	}
	lay := m.approvalLayoutFor(contentW, 1+len(approvalBaseActions))
	if lay.previewRows < lay.previewTotal || lay.overflow {
		return true
	}
	if approvalRemembersOther(req.request) && (!lay.showSegments || !approvalRememberRowWhole(req.request, contentW)) {
		return true
	}
	if !approvalSummaryCut(req.request, contentW) {
		return false
	}
	return approvalIsFileChange(req.request) || lay.previewTotal == 0
}

// approvalDialogLines renders the dialog's rows for a content area contentW
// cells wide. Every row is at most contentW cells, so the input box never has
// to wrap one (which would break the height budget).
func (m Model) approvalDialogLines(contentW int) []string {
	req := m.pendingAuth.request
	lay := m.approvalLayout(contentW)
	var lines []string
	lines = append(lines, styleWarn.Render(approvalSummaryRow(req, contentW)))
	if lay.showReason {
		lines = append(lines, styleMuted.Render(termtext.Fit("  why: "+termtext.SingleLine(req.Reason), contentW)))
	}
	if lay.showSegments {
		// Permission debug: the segments that needed approval, each written
		// out like the summary row (a newline inside quotes as "^J").
		escaped := make([]string, len(req.Segments))
		for i, seg := range req.Segments {
			escaped[i] = termtext.EscapeExact(seg)
		}
		row := termtext.Fit("  segments: "+strings.Join(escaped, " | "), contentW)
		if approvalRemembersOther(req) {
			// What "Allow always" approves: a cut is marked like the
			// summary row's (the review lists every command whole).
			row = approvalRememberRow(req)
			if ansi.StringWidth(row) > contentW {
				row = termtext.Fit(row, contentW-ansi.StringWidth(approvalCutMarker)) + approvalCutMarker
			}
		}
		lines = append(lines, styleMuted.Render(termtext.Fit(row, contentW)))
	}
	if lay.previewRows > 0 {
		preview := m.approvalPreviewLines(contentW)
		lines = append(lines, preview[lay.previewStart:lay.previewStart+lay.previewRows]...)
	}
	if lay.showFooter {
		lines = append(lines, styleMuted.Render(termtext.Fit("  "+approvalFooterText(lay, m.approvalPreviewExpanded, contentW-2), contentW)))
	}
	if m.approvalChoice == approvalActInstead {
		lines = append(lines, styleMuted.Render(termtext.Fit("  type what to do instead, then press enter:", contentW)))
		lines = append(lines, m.inputTextareaView())
		return lines
	}
	actions := m.approvalActions(contentW)
	selected := m.approvalSelected(contentW)
	rows := make([]pickerOption, len(actions))
	cursor := 0
	for i, a := range actions {
		rows[i] = pickerOption{Label: approvalActionLabel(a, req)}
		if a == approvalActReview && m.pendingAuth.reviewed {
			rows[i].Badge = "seen"
		}
		if a == selected {
			cursor = i
		}
	}
	title := "allow this command?"
	if n := len(m.approvalQueue); n > 0 {
		// Parallel agents: say that answering this one brings up another,
		// so a second Enter is not pressed on the assumption it is over.
		title = fmt.Sprintf("allow this command? (%d more waiting)", n)
	}
	if lay.previewTotal > 0 && !lay.showFooter && lay.previewRows < lay.previewTotal {
		// No row was left for the preview's footer (a 40x15 terminal during
		// a run): the picker's title says what is not shown instead, so the
		// preview is never hidden in silence.
		title = approvalFooterText(lay, m.approvalPreviewExpanded, contentW-2)
	}
	picker := m.renderAnnotatedPicker(title, rows, cursor, theme.Current().Warning)
	for _, row := range strings.Split(picker, "\n") {
		lines = append(lines, termtext.Fit(row, contentW))
	}
	return lines
}

// approvalSummaryRow is the dialog's summary row, at most contentW cells,
// indented by two. A command too long for it is cut at the end (the preview
// shows it whole). A file change ("$ file-write <path>") keeps the tool
// name and cuts the start of its path instead ("$ file-write …/dir/name.go"),
// so the file name survives: cut at the end, a deep path lost its file name,
// and on a terminal too short for the diff preview (40x15) the dialog asked
// to approve a write without naming the file.
//
// A cut row ends with approvalCutMarker, so a cut is never mistaken for the
// whole call: an ellipsis alone is easy to miss in a long path.
func approvalSummaryRow(req agent.ShellApprovalRequest, contentW int) string {
	summary := approvalSummary(formatApprovalCommandLabel(req.Command))
	if ansi.StringWidth(summary)+2 <= contentW {
		return "  " + summary
	}
	markerW := ansi.StringWidth(approvalCutMarker)
	if contentW < 2+minApprovalPathCells+markerW {
		// Too narrow for a marker to leave anything worth reading.
		return termtext.Fit("  "+summary, contentW)
	}
	room := contentW - markerW
	head := "  $ " + req.ToolID + " "
	path, ok := strings.CutPrefix(summary, "$ "+req.ToolID+" ")
	if !approvalIsFileChange(req) || req.ToolID == "" || !ok || ansi.StringWidth(head)+minApprovalPathCells > room {
		return termtext.Fit("  "+summary, room) + approvalCutMarker
	}
	return head + termtext.FitLeft(path, room-ansi.StringWidth(head)) + approvalCutMarker
}

// approvalRememberSep separates the commands of the "remembers:" row.
const approvalRememberSep = " · "

// approvalRememberRow is the dialog's "remembers:" row, uncut: every command
// "Allow always" would remember (approvalRemembered), escaped exactly like
// the summary row.
func approvalRememberRow(req agent.ShellApprovalRequest) string {
	remembered := approvalRemembered(req)
	parts := make([]string, len(remembered))
	for i, r := range remembered {
		parts[i] = termtext.EscapeExact(r)
	}
	return "  remembers: " + strings.Join(parts, approvalRememberSep)
}

// approvalRememberRowWhole reports whether the "remembers:" row shows every
// command whole at contentW, and apart: a command that itself contains the
// separator would read as two.
func approvalRememberRowWhole(req agent.ShellApprovalRequest, contentW int) bool {
	for _, r := range approvalRemembered(req) {
		if strings.Contains(termtext.EscapeExact(r), strings.TrimSpace(approvalRememberSep)) {
			return false
		}
	}
	return ansi.StringWidth(approvalRememberRow(req)) <= contentW
}

// approvalCutMarker ends a summary row that could not hold the whole call.
const approvalCutMarker = " [cut]"

// approvalSummaryCut reports whether approvalSummaryRow cuts the call at
// contentW.
func approvalSummaryCut(req agent.ShellApprovalRequest, contentW int) bool {
	return !approvalSummaryFits(formatApprovalCommandLabel(req.Command), contentW)
}

// minApprovalPathCells is the fewest cells approvalSummaryRow keeps for a
// path cut from the left; with less, the row is simply cut at the end.
const minApprovalPathCells = 8

// approvalSummary puts a command label on the dialog's one summary row. The
// lines of a multi-line command are joined with a space (the preview under
// the row shows them apart); nothing else is changed. In particular runs of
// spaces are kept and control and invisible characters are shown
// (termtext.EscapeExact), because for a one-line command that fits, this row
// is the only place the user sees what they are approving.
func approvalSummary(label string) string {
	lines := strings.Split(label, "\n")
	for i, line := range lines {
		lines[i] = termtext.EscapeExact(line)
	}
	return strings.Join(lines, " ")
}

// approvalFooterText says which part of the preview is on screen and which
// keys show the rest, in at most width cells where it can. The review key
// comes before the keys that only move the preview: on a narrow dialog the
// end of the row is what gets cut. A preview with no room at all says how
// much is not shown, in the longest wording that fits.
//
// The counts are preview rows. A diff row is one line of the diff, and a
// command row one line of the command unless a line was hard-wrapped; then
// the counts say "rows", as the review does, so an 83-line command wrapped
// onto 243 rows of a narrow dialog is not reported as 243 lines.
func approvalFooterText(lay approvalLayout, expanded bool, width int) string {
	unit := "lines"
	if lay.wrapped {
		unit = "rows"
	}
	if lay.previewRows == 0 {
		var text string
		for _, format := range []string{
			"%d %s not shown - press v to review",
			"%d %s not shown - v to review",
			"%d %s hidden - v to review",
		} {
			text = fmt.Sprintf(format, lay.previewTotal, unit)
			if ansi.StringWidth(text) <= width {
				break
			}
		}
		return text
	}
	text := fmt.Sprintf("%s %d-%d of %d", unit, lay.previewStart+1, lay.previewStart+lay.previewRows, lay.previewTotal)
	if lay.overflow {
		text += ", long lines cut"
	}
	if lay.previewRows < lay.previewTotal || lay.overflow {
		text += " · v review all"
	}
	if lay.previewRows < lay.previewTotal {
		text += " · pgup/pgdn scroll"
	}
	switch {
	case expanded:
		text += " · ctrl+o collapse"
	case lay.canExpand:
		text += " · ctrl+o expand"
	}
	return text
}

// scrollApprovalPreview moves the preview window by delta rows, clamped so
// the window stays inside the preview.
func (m Model) scrollApprovalPreview(delta int) Model {
	lay := m.approvalLayout(m.approvalContentWidth())
	m.approvalScroll = clampOffset(lay.previewStart+delta, 0, max(lay.previewTotal-lay.previewRows, 0))
	return m
}

// approvalContentWidth is the content width of the input box the dialog is
// drawn in (viewInput draws the box across the whole pane).
func (m Model) approvalContentWidth() int {
	return boxContentWidth(m.paneWidth())
}
