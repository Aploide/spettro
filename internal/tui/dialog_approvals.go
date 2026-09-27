package tui

import (
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"

	"spettro/internal/agent"
)

// A person must be able to trust what they approve, so nothing an approval
// covers may be out of their reach. When the dialog cannot show all of it
// (the preview is capped, windowed or hidden for lack of rows, a diff line is
// cut at the dialog's width, or the summary row cuts a file's path), the
// picker gains a first option, "Review full diff" (or "command", "request"),
// and that option, not "Allow once", is selected until the user moves the
// cursor. It opens the review (dialog_approval_review.go): the whole call on
// the whole screen, scrollable to its last line. "v" opens it from anywhere
// in the picker. When everything is on screen the picker is the plain four
// options with "Allow once" selected, as before: a short command costs no
// extra keystroke.
//
// Whether the review is offered is latched on the request
// (shellApprovalRequestMsg.reviewOffered): once offered it stays for that
// request, whatever the terminal does next. Otherwise growing the terminal or
// expanding the preview would take the option away and move every option
// under the cursor. The cursor holds an action, not a row index, for the
// same reason: a row appearing above it never changes what Enter does.

// approvalAction is one choice of the approval picker.
type approvalAction int

const (
	// approvalActDefault is the cursor before the user moved it: the review
	// when it is offered, else "Allow once" (see approvalSelected).
	approvalActDefault approvalAction = iota
	approvalActReview
	approvalActAllowOnce
	approvalActAllowAlways
	approvalActDeny
	// approvalActInstead swaps the picker for a text field: what the agent
	// should do instead.
	approvalActInstead
)

// approvalBaseActions are the picker's rows when the dialog shows the whole
// call.
var approvalBaseActions = []approvalAction{approvalActAllowOnce, approvalActAllowAlways, approvalActDeny, approvalActInstead}

// shellApprovalOptions are the labels of approvalBaseActions, in order.
var shellApprovalOptions = []string{
	"Allow once",
	"Allow always  (remember this command)",
	"Deny",
	"Tell the agent what to do instead",
}

// approvalActionLabel is the picker row of action a for request req.
func approvalActionLabel(a approvalAction, req agent.ShellApprovalRequest) string {
	switch a {
	case approvalActReview:
		return approvalReviewLabel(req) + " (v)"
	case approvalActAllowOnce:
		return shellApprovalOptions[0]
	case approvalActAllowAlways:
		return shellApprovalOptions[1]
	case approvalActDeny:
		return shellApprovalOptions[2]
	case approvalActInstead:
		return shellApprovalOptions[3]
	}
	return ""
}

// approvalReviewLabel names the review after what it shows: a file change's
// diff, a network call's request, or a command.
func approvalReviewLabel(req agent.ShellApprovalRequest) string {
	switch {
	case approvalIsFileChange(req):
		return "Review full diff"
	case !strings.HasPrefix(formatApprovalCommandLabel(req.Command), "$ "):
		return "Review full request"
	default:
		return "Review full command"
	}
}

// approvalIsFileChange reports whether req approves a change to a file (a
// file-write or file-edit, which carry a diff), rather than a command or a
// network call.
func approvalIsFileChange(req agent.ShellApprovalRequest) bool {
	return req.Change != nil || strings.TrimSpace(req.Diff) != ""
}

// approvalOffersReview reports whether the picker offers the review for a
// dialog contentW cells wide, latching the answer on the request the first
// time it is yes (see the comment at the top of this file). Like
// approvalPreviewLines it may run from View: the latch lives on the pending
// request, which every copy of the model shares.
func (m Model) approvalOffersReview(contentW int) bool {
	req := m.pendingAuth
	if req == nil {
		return false
	}
	if req.reviewOffered {
		return true
	}
	if m.width <= 0 || m.height <= 0 {
		// No WindowSizeMsg yet: nothing is drawn, so nothing is hidden.
		return false
	}
	if m.approvalHidesContent(contentW) {
		req.reviewOffered = true
	}
	return req.reviewOffered
}

// approvalActions are the picker's rows, top to bottom.
func (m Model) approvalActions(contentW int) []approvalAction {
	if m.approvalOffersReview(contentW) {
		return append([]approvalAction{approvalActReview}, approvalBaseActions...)
	}
	return approvalBaseActions
}

// approvalSelected is the action under the cursor, the default resolved.
func (m Model) approvalSelected(contentW int) approvalAction {
	if m.approvalChoice != approvalActDefault {
		return m.approvalChoice
	}
	if m.approvalOffersReview(contentW) {
		return approvalActReview
	}
	return approvalActAllowOnce
}

// syncApprovalReview evaluates, and latches, whether the pending approval
// offers the review at the current size. The picker computes it anyway when
// drawn; calling this from Update (when a request arrives, when the terminal
// is resized) makes the first frame already show the option.
func (m Model) syncApprovalReview() {
	if m.pendingAuth != nil {
		m.approvalOffersReview(m.approvalContentWidth())
	}
}

func (m Model) updateShellApproval(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if m.pendingAuth == nil {
		return m, nil
	}
	contentW := m.approvalContentWidth()
	actions := m.approvalActions(contentW)
	selected := m.approvalSelected(contentW)
	if selected == approvalActInstead {
		switch msg.String() {
		case "enter":
			raw := strings.TrimSpace(m.ta.Value())
			if raw == "" {
				m.showBanner("type what the agent should do instead, then press enter", "warn")
				return m, nil
			}
			m = m.resolveShellApproval(agent.ShellApprovalDeny, "command denied")
			m.interruptRun("Command denied by user.", true)
			m.ta.SetValue(raw)
			return m, nil
		case "esc":
			m.approvalChoice = approvalActDefault
			m.ta.Reset()
			return m, nil
		default:
			var taCmd tea.Cmd
			m.ta, taCmd = m.ta.Update(msg)
			return m, taCmd
		}
	}
	switch msg.String() {
	case "v":
		return m.openApprovalReview(), nil
	case "ctrl+o":
		// Expand or collapse the preview (a file change's diff or a long
		// command); the scroll position is kept and re-clamped on render.
		if len(m.approvalPreviewLines(contentW)) > 0 {
			m.approvalPreviewExpanded = !m.approvalPreviewExpanded
		}
		return m, nil
	case "pgdown":
		lay := m.approvalLayout(contentW)
		return m.scrollApprovalPreview(max(lay.previewRows-1, 1)), nil
	case "pgup":
		lay := m.approvalLayout(contentW)
		return m.scrollApprovalPreview(-max(lay.previewRows-1, 1)), nil
	case "up":
		if i := slices.Index(actions, selected); i > 0 {
			m.approvalChoice = actions[i-1]
		}
		return m, nil
	case "down", "ctrl+n":
		if i := slices.Index(actions, selected); i >= 0 && i < len(actions)-1 {
			m.approvalChoice = actions[i+1]
		}
		return m, nil
	case "enter":
		switch selected {
		case approvalActReview:
			return m.openApprovalReview(), nil
		case approvalActAllowOnce:
			return m.resolveShellApproval(agent.ShellApprovalAllowOnce, "command approved once"), nil
		case approvalActAllowAlways:
			return m.resolveShellApproval(agent.ShellApprovalAllowAlways, "command approved and saved"), nil
		case approvalActDeny:
			m = m.resolveShellApproval(agent.ShellApprovalDeny, "command denied")
			m.interruptRun("Command denied by user.", true)
			return m, nil
		}
	case "esc":
		m = m.resolveShellApproval(agent.ShellApprovalDeny, "command denied")
		m.interruptRun("Command denied by user.", true)
		return m, nil
	}
	return m, nil
}

func (m Model) resolveShellApproval(decision agent.ShellApprovalDecision, banner string) Model {
	if m.pendingAuth != nil {
		select {
		case m.pendingAuth.response <- shellApprovalResponse{decision: decision}:
		default:
		}
	}
	m.pendingAuth = nil
	m = m.resetApprovalUI()
	m.ta.Reset()
	m.showBanner(banner, "info")
	m.refreshViewport()
	return m
}

// resetApprovalUI clears the dialog state that belongs to one approval (the
// cursor, the preview's scroll and expansion, the review), for the next one.
func (m Model) resetApprovalUI() Model {
	m.approvalChoice = approvalActDefault
	m.approvalPreviewExpanded = false
	m.approvalScroll = 0
	m.approvalReviewOpen = false
	m.approvalReviewScroll = 0
	return m
}
