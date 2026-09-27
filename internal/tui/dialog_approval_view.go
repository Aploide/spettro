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
//	  $ cat <<'EOF' > big.txt …       summary: the call on one row
//	  why: non-whitelisted command    reason (and, with permission debug on,
//	                                  the segments that needed approval)
//	    …                             preview: the diff of a file change, or
//	    …                             the full text of a command or network
//	  lines 1-8 of 3002 · …           target too long for the summary row;
//	                                  scrollable
//	  allow this command?             picker (or the "instead" text field)
//	  › Allow once
//	    …
//
// Tool arguments have no size limit (a file-write carries a whole file, a
// bash call can be a 50k-character heredoc), while the dialog has to fit the
// terminal with every option still visible, at 40x15 as much as at 200x60.
// approvalLayout decides which of the optional rows get space; the preview
// is windowed and scrolled rather than wrapped past the bottom of the screen.
// The summary row and the picker are never dropped.

// Collapsed preview caps. ctrl+o lifts them (the preview then takes every
// row the terminal can spare); the cap only keeps a routine approval from
// hiding the transcript.
const (
	approvalDiffCollapsedLines    = 16
	approvalCommandCollapsedLines = 8
)

// approvalPreviewIndent prefixes each row of a command preview, under the
// "$" of the summary row.
const approvalPreviewIndent = "    "

// approvalPreviewCache memoises the fully rendered preview of the pending
// approval at one content width. View runs on every animation tick; wrapping
// a 50k-character command or rendering a 2000-line diff each time would make
// the dialog lag behind the keyboard. It lives on the pending request, so a
// new approval can never be shown with the previous one's preview.
type approvalPreviewCache struct {
	width int
	lines []string
}

// approvalPreviewLines returns every display row of the pending approval's
// preview, each at most width cells wide, or nil when the call has nothing
// to preview beyond its summary row.
func (m Model) approvalPreviewLines(width int) []string {
	req := m.pendingAuth
	if req == nil {
		return nil
	}
	if c := req.previewCache; c != nil && c.width == width {
		return c.lines
	}
	lines := buildApprovalPreview(req.request, width)
	req.previewCache = &approvalPreviewCache{width: width, lines: lines}
	return lines
}

// buildApprovalPreview renders the preview rows for one approval request:
// the diff when the request carries one (file-write, file-edit), otherwise
// the full text of the summary row when it does not fit that row: the whole
// command, or for a network call the whole sentence naming its target.
// Control characters are made visible, not interpreted
// (termtext.EscapeControls): the preview is what the user approves, so no
// character of it may be hidden.
func buildApprovalPreview(req agent.ShellApprovalRequest, width int) []string {
	if strings.TrimSpace(req.Diff) != "" {
		// Stay in the unified layout: the side-by-side one halves the room
		// each line gets, and a narrow dialog is where this is read most.
		rendered := diff.Render(req.Diff, diff.Options{
			Width:  min(width, diff.SideBySideMinWidth-1),
			Indent: "  ",
		})
		return strings.Split(rendered, "\n")
	}
	label := formatApprovalCommandLabel(req.Command)
	text := strings.TrimSpace(req.Command)
	if !strings.HasPrefix(label, "$ ") {
		// A network call: the label is one line (its target has no
		// newlines, see formatApprovalCommandLabel), previewed whole.
		text = label
	}
	if !strings.Contains(text, "\n") && approvalSummaryFits(label, width) {
		return nil
	}
	textW := max(width-len(approvalPreviewIndent), 8)
	var lines []string
	for _, raw := range strings.Split(text, "\n") {
		for _, part := range termtext.Wrap(termtext.EscapeControls(raw), textW) {
			lines = append(lines, styleMuted.Render(approvalPreviewIndent+part))
		}
	}
	return lines
}

// approvalSummaryFits reports whether a one-line summary label (see
// formatApprovalCommandLabel) is shown whole on the summary row, which is
// indented by two cells.
func approvalSummaryFits(label string, width int) bool {
	return ansi.StringWidth(approvalSummary(label))+2 <= width
}

// approvalLayout is the approval dialog cut to the rows the terminal has.
type approvalLayout struct {
	showLabel    bool // the agent label row
	showReason   bool // the "why:" row
	showSegments bool // the permission-debug segments row
	previewRows  int  // preview rows on screen; may be 0 when there is no room
	previewTotal int  // preview rows in all
	previewStart int  // index of the first preview row on screen
	showFooter   bool // the "lines a-b of n" row under the preview
	canExpand    bool // ctrl+o would show more of the preview
}

// approvalLayout decides which optional rows of the dialog get space, for a
// dialog whose content area is contentW cells wide.
//
// The rows available to the dialog are the terminal height minus the header,
// the two separators, the status bar, the working indicator, the footer
// block (which yields to the dialog, see parallelFooterBudget), the input box
// border and a minimum of transcript. The summary row and the picker always
// get theirs. What is left goes, in order, to: the preview footer (so a
// preview is never hidden without saying so), the reason, the agent label,
// the preview itself (up to its collapsed cap unless expanded), and the
// permission-debug segments.
func (m Model) approvalLayout(contentW int) approvalLayout {
	req := m.pendingAuth
	var lay approvalLayout
	if req == nil {
		return lay
	}
	preview := m.approvalPreviewLines(contentW)
	lay.previewTotal = len(preview)

	fixedChrome := 1 + 2 + 1 + m.workingIndicatorHeight() + m.parallelFooterHeight() +
		dialogMinTranscriptRows(m.height) + 2
	rows := m.height - fixedChrome - 1 - m.approvalControlRows()
	take := func() bool {
		if rows < 1 {
			return false
		}
		rows--
		return true
	}

	footerReserved := lay.previewTotal > 0 && take()
	lay.showReason = strings.TrimSpace(req.request.Reason) != "" && take()
	lay.showLabel = take()

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
		// off screen; otherwise its row goes back to the pool.
		lay.showFooter = footerReserved && (lay.previewRows < lay.previewTotal || m.approvalPreviewExpanded)
		if footerReserved && !lay.showFooter {
			rows++
		}
	}
	lay.showSegments = len(req.request.Segments) > 0 && m.cfg.ShowPermissionDebug && take()
	return lay
}

// approvalControlRows is the height of what sits under the preview: the
// picker (a title plus one row per option), or, once the user chose to tell
// the agent what to do instead, a prompt row and the text field.
func (m Model) approvalControlRows() int {
	if m.approvalCursor == approvalInsteadOption {
		return 1 + m.ta.Height()
	}
	return 1 + len(shellApprovalOptions)
}

// approvalInsteadOption is the picker index of "Tell the agent what to do
// instead", which swaps the picker for a text field.
const approvalInsteadOption = 3

// approvalDialogLines renders the dialog's rows, label included, for a
// content area contentW cells wide. Every row is at most contentW cells, so
// the input box never has to wrap one (which would break the height budget).
func (m Model) approvalDialogLines(label string, contentW int) []string {
	req := m.pendingAuth.request
	lay := m.approvalLayout(contentW)
	var lines []string
	if lay.showLabel {
		lines = append(lines, label)
	}
	summary := approvalSummary(formatApprovalCommandLabel(req.Command))
	lines = append(lines, styleWarn.Render(termtext.Fit("  "+summary, contentW)))
	if lay.showReason {
		lines = append(lines, styleMuted.Render(termtext.Fit("  why: "+termtext.SingleLine(req.Reason), contentW)))
	}
	if lay.showSegments {
		lines = append(lines, styleMuted.Render(termtext.Fit("  segments: "+termtext.SingleLine(strings.Join(req.Segments, " | ")), contentW)))
	}
	if lay.previewRows > 0 {
		preview := m.approvalPreviewLines(contentW)
		lines = append(lines, preview[lay.previewStart:lay.previewStart+lay.previewRows]...)
	}
	if lay.showFooter {
		lines = append(lines, styleMuted.Render(termtext.Fit("  "+approvalFooterText(lay, m.approvalPreviewExpanded), contentW)))
	}
	if m.approvalCursor == approvalInsteadOption {
		lines = append(lines, styleMuted.Render(termtext.Fit("  type what to do instead, then press enter:", contentW)))
		lines = append(lines, m.inputTextareaView())
		return lines
	}
	picker := m.renderApprovalPicker("allow this command?", shellApprovalOptions, m.approvalCursor, theme.Current().Warning)
	for _, row := range strings.Split(picker, "\n") {
		lines = append(lines, termtext.Fit(row, contentW))
	}
	return lines
}

// approvalSummary puts a command label on the dialog's one summary row. The
// lines of a multi-line command are joined with a space; nothing else is
// changed. In particular runs of spaces are kept and control characters are
// shown (termtext.EscapeControls), because for a one-line command that fits,
// this row is the only place the user sees what they are approving.
func approvalSummary(label string) string {
	lines := strings.Split(label, "\n")
	for i, line := range lines {
		lines[i] = termtext.EscapeControls(line)
	}
	return strings.Join(lines, " ")
}

// approvalFooterText says which part of the preview is on screen and which
// keys show the rest.
func approvalFooterText(lay approvalLayout, expanded bool) string {
	if lay.previewRows == 0 {
		return fmt.Sprintf("%d-line preview hidden (no room)", lay.previewTotal)
	}
	text := fmt.Sprintf("lines %d-%d of %d", lay.previewStart+1, lay.previewStart+lay.previewRows, lay.previewTotal)
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
