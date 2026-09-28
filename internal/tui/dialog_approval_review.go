package tui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"spettro/internal/agent"
	"spettro/internal/diff"
	"spettro/internal/termtext"
)

// The approval review shows the whole of a pending approval on the whole
// screen: what the dialog under the input box (dialog_approval_view.go) may
// only be able to show part of on a small terminal. It opens from the
// dialog's "Review full ..." option or its "v" key, and Esc, q or v bring the
// dialog back with the review marked as seen. It never approves or denies
// anything itself.
//
//	Review full diff · file-write              title
//	────────────────────────────────────────
//	tool     file-write                        what is approved, every
//	path     src/very/long/…/name.go           field wrapped, never cut
//	reason   file modification requires…
//	always   same as once: every file change…  what "Allow always" remembers
//	── diff · 905 lines ─────────────────────
//	 1     + package main                      the complete diff (for a new
//	 2     + …                                 file, its whole content) or
//	── end of diff ──────────────────────────   the numbered command (and
//	                                           the commands "Allow always"
//	                                           remembers, when not just it)
//	────────────────────────────────────────
//	rows 1-9 of 912 · 1%                       position
//	↑↓ pgup/pgdn home/end · esc back           keys
//
// Nothing is cut: a line wider than the screen is wrapped (hard-wrapped, so
// no space goes missing at a break or ends a row unseen) under a blank
// gutter, and every row of the document can be scrolled to. Control, bidi,
// zero-width and other invisible characters are made visible
// (termtext.EscapeExact, which the diff renderer applies to every diff line
// too with diff.Options.Exact). Nothing is shown as something else either:
// a tab is drawn as "⇥" and a carriage return ending a line as "^M", as in
// the dialog's preview.
//
// The document is rendered once per width and cached on the pending request
// (approvalReviewCache), like the dialog's preview: a 60k-line file must not
// be re-rendered on every animation tick.

// approvalReviewCache holds the review's rows for one width.
type approvalReviewCache struct {
	width int
	lines []string
}

// approvalReviewLabelW is the width of the field names column ("reason   ").
const approvalReviewLabelW = 9

// openApprovalReview shows the review of the pending approval.
func (m Model) openApprovalReview() Model {
	if m.pendingAuth != nil {
		m.approvalReviewOpen = true
	}
	return m
}

// closeApprovalReview returns to the dialog with the review marked as seen.
// A cursor still on the review (or on the default, which was the review)
// moves to "Allow once", the next step for someone who has read the change;
// a cursor the user moved elsewhere stays where it is.
func (m Model) closeApprovalReview() Model {
	m.approvalReviewOpen = false
	if m.pendingAuth == nil {
		return m
	}
	m.pendingAuth.reviewed = true
	if m.approvalChoice == approvalActDefault || m.approvalChoice == approvalActReview {
		m.approvalChoice = approvalActAllowOnce
	}
	return m
}

func (m Model) updateApprovalReview(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	page := max(m.approvalReviewBodyRows()-1, 1)
	switch msg.String() {
	case "esc", "q", "v", "enter":
		return m.closeApprovalReview(), nil
	case "up", "k":
		return m.scrollApprovalReview(-1), nil
	case "down", "j":
		return m.scrollApprovalReview(1), nil
	case "pgup", "b", "shift+space":
		return m.scrollApprovalReview(-page), nil
	case "pgdown", "space", "f":
		return m.scrollApprovalReview(page), nil
	case "home", "g":
		m.approvalReviewScroll = 0
		return m, nil
	case "end", "G":
		return m.scrollApprovalReview(len(m.approvalReviewDoc(approvalReviewContentWidth(m.width)))), nil
	}
	return m, nil
}

// scrollApprovalReview moves the review by delta rows, clamped to the
// document.
func (m Model) scrollApprovalReview(delta int) Model {
	if m.pendingAuth == nil {
		return m
	}
	doc := m.approvalReviewDoc(approvalReviewContentWidth(m.width))
	maxStart := max(len(doc)-m.approvalReviewBodyRows(), 0)
	m.approvalReviewScroll = clampOffset(clampOffset(m.approvalReviewScroll, 0, maxStart)+delta, 0, maxStart)
	return m
}

// approvalReviewContentWidth is the width of the review's rows on a terminal
// width cells wide: one cell of margin on each side.
func approvalReviewContentWidth(width int) int {
	return max(width-2, 1)
}

// approvalReviewRules reports whether the review draws the rules under its
// title and above its footer; below 20 rows they go to the document.
func approvalReviewRules(height int) bool {
	return height >= 20
}

// approvalReviewFooter is the review's bottom rows: where the view is in the
// document and the keys. They share a row when it is wide enough.
func approvalReviewFooter(start, rows, total, width int) []string {
	end := min(start+rows, total)
	pos := fmt.Sprintf("rows %d-%d of %d", min(start+1, total), end, total)
	switch {
	case end >= total:
		pos += " · end"
	case start == 0:
		pos += " · top"
	default:
		pos += fmt.Sprintf(" · %d%%", end*100/max(total, 1))
	}
	keys := "↑↓ pgup/pgdn home/end · esc back"
	if ansi.StringWidth(pos)+3+ansi.StringWidth(keys) <= width {
		return []string{pos + "   " + keys}
	}
	return []string{pos, keys}
}

// approvalReviewChromeRows is how many rows the title, the rules and the
// footer take on a terminal width x height.
func approvalReviewChromeRows(width, height int) int {
	rows := 1 + len(approvalReviewFooter(0, 1, 1000000, approvalReviewContentWidth(width)))
	if approvalReviewRules(height) {
		rows += 2
	}
	return rows
}

// approvalReviewBodyRows is how many document rows the review shows at once.
func (m Model) approvalReviewBodyRows() int {
	return max(m.height-approvalReviewChromeRows(m.width, m.height), 1)
}

// viewApprovalReview draws the review over the whole terminal.
func (m Model) viewApprovalReview() string {
	if m.pendingAuth == nil {
		return ""
	}
	req := m.pendingAuth.request
	contentW := approvalReviewContentWidth(m.width)
	doc := m.approvalReviewDoc(contentW)
	bodyRows := m.approvalReviewBodyRows()
	start := clampOffset(m.approvalReviewScroll, 0, max(len(doc)-bodyRows, 0))

	title := approvalReviewLabel(req) + " · " + approvalReviewToolName(req)
	rows := []string{" " + styleWarn.Bold(true).Render(termtext.Fit(title, contentW))}
	rule := " " + styleRule.Render(strings.Repeat("─", contentW))
	if approvalReviewRules(m.height) {
		rows = append(rows, rule)
	}
	end := min(start+bodyRows, len(doc))
	for _, line := range doc[start:end] {
		rows = append(rows, " "+line)
	}
	for range bodyRows - (end - start) {
		rows = append(rows, "")
	}
	if approvalReviewRules(m.height) {
		rows = append(rows, rule)
	}
	for _, f := range approvalReviewFooter(start, bodyRows, len(doc), contentW) {
		rows = append(rows, " "+styleMuted.Render(termtext.Fit(f, contentW)))
	}
	return strings.Join(rows, "\n")
}

// approvalReviewDoc returns every row of the pending approval's review at
// width, rendering it the first time and caching it on the request.
func (m Model) approvalReviewDoc(width int) []string {
	req := m.pendingAuth
	if req == nil {
		return nil
	}
	if c := req.reviewCache; c != nil && c.width == width {
		return c.lines
	}
	lines := buildApprovalReview(req.request, width)
	req.reviewCache = &approvalReviewCache{width: width, lines: lines}
	return lines
}

// approvalReviewToolName is the tool asking. A network approval arrives with
// no ToolID and the command "network <tool> <target>".
func approvalReviewToolName(req agent.ShellApprovalRequest) string {
	if req.ToolID != "" {
		return req.ToolID
	}
	if fields := strings.Fields(req.Command); len(fields) >= 2 && fields[0] == "network" {
		return fields[1]
	}
	return "tool"
}

// buildApprovalReview renders the review document: the fields that say what
// is approved, then the diff or the command in full. Every row is at most
// width cells.
func buildApprovalReview(req agent.ShellApprovalRequest, width int) []string {
	var out []string
	field := func(name, value string) {
		out = append(out, approvalReviewField(name, value, width)...)
	}
	label := formatApprovalCommandLabel(req.Command)
	isFile := approvalIsFileChange(req)
	network := approvalIsNetwork(req)

	field("tool", approvalReviewToolName(req))
	switch {
	case isFile:
		field("path", strings.TrimPrefix(trimShellBlanks(req.Command), req.ToolID+" "))
		if c := req.Change; c != nil {
			if c.Path != "" {
				field("file", c.Path)
			}
			if c.Created {
				field("change", "new file")
			} else {
				field("change", "edit of an existing file")
			}
		}
	case network:
		fields := strings.Fields(req.Command)
		if len(fields) >= 3 {
			field("target", strings.Join(fields[2:], " "))
		}
		field("request", label)
	}
	if req.AgentID != "" {
		field("agent", req.AgentID)
	}
	if req.CWD != "" {
		field("dir", req.CWD)
	}
	if strings.TrimSpace(req.Reason) != "" {
		field("reason", req.Reason)
	}
	switch {
	case isFile:
		field("always", "same as once: every file change is asked about")
	case network:
		for _, target := range approvalRemembered(req) {
			field("always", "remembers "+target)
		}
	case !approvalRemembersOther(req):
		field("always", "remembers this command")
	default:
		// Listed in full after the command: see the end of this function.
		field("always", "remembers "+plural(len(req.Segments), "command")+", listed below the command")
	}

	switch {
	case isFile && strings.TrimSpace(req.Diff) != "":
		n := strings.Count(strings.TrimRight(req.Diff, "\n"), "\n") + 1
		out = append(out, approvalReviewRule("diff · "+plural(n, "line"), width))
		rendered := diff.Render(req.Diff, diff.Options{Width: width, Wrap: true, Exact: true})
		out = append(out, strings.Split(rendered, "\n")...)
		out = append(out, approvalReviewRule("end of diff", width))
	case network:
		// The fields above hold all of it.
	default:
		command := trimShellBlanks(req.Command)
		lines := strings.Split(command, "\n")
		out = append(out, approvalReviewRule("command · "+plural(len(lines), "line"), width))
		out = append(out, approvalReviewNumbered(lines, width)...)
		out = append(out, approvalReviewRule("end of command", width))
		// Every command "Allow always" would remember, when that is not just
		// the command above: the choice approves each of them for good, so
		// none is left to a count.
		if approvalRemembersOther(req) {
			out = append(out, approvalReviewRule("allow always remembers · "+plural(len(req.Segments), "command"), width))
			out = append(out, approvalReviewNumbered(req.Segments, width)...)
			out = append(out, approvalReviewRule("end of list", width))
		}
	}
	return out
}

// approvalReviewField renders one "name  value" field: the value escaped
// (termtext.EscapeExact, line by line) and hard-wrapped under itself, or,
// on a screen too narrow for a names column, under the name.
func approvalReviewField(name, value string, width int) []string {
	labelW := approvalReviewLabelW
	valueW := width - labelW
	if valueW < 12 {
		labelW, valueW = 0, width
	}
	var rows []string
	if labelW == 0 {
		rows = append(rows, styleMuted.Render(termtext.Fit(name, width)))
	}
	first := true
	for _, line := range strings.Split(value, "\n") {
		for _, part := range termtext.HardWrap(termtext.EscapeExact(line), valueW) {
			prefix := strings.Repeat(" ", labelW)
			if first && labelW > 0 {
				prefix = styleMuted.Render(fmt.Sprintf("%-*s", labelW, name))
			}
			first = false
			rows = append(rows, prefix+styleText.Render(part))
		}
	}
	return rows
}

// approvalReviewNumbered renders the lines of a command under a line-number
// gutter, escaped and hard-wrapped; continuation rows leave the gutter blank.
func approvalReviewNumbered(lines []string, width int) []string {
	numW := len(fmt.Sprint(len(lines)))
	gutter := numW + 2
	textW := max(width-gutter, 1)
	if width-gutter < 8 {
		gutter, textW = 0, width
	}
	var out []string
	for i, line := range lines {
		for r, part := range termtext.HardWrap(termtext.EscapeExact(line), textW) {
			prefix := ""
			switch {
			case gutter == 0:
			case r == 0:
				prefix = styleMuted.Render(fmt.Sprintf("%*d  ", numW, i+1))
			default:
				prefix = strings.Repeat(" ", gutter)
			}
			out = append(out, prefix+styleText.Render(part))
		}
	}
	return out
}

// approvalReviewRule is a section rule ("── diff · 905 lines ────"), width
// cells wide. The title is drawn in the muted text colour rather than the
// rule's, which is too faint to read in either theme.
func approvalReviewRule(title string, width int) string {
	if width < 8 {
		return styleMuted.Render(termtext.Fit(title, width))
	}
	text := termtext.Fit(title, width-4)
	fill := max(width-4-ansi.StringWidth(text), 0)
	return styleRule.Render("── ") + styleMuted.Render(text) + styleRule.Render(" "+strings.Repeat("─", fill))
}
