package tui

import (
	"fmt"
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"spettro/internal/agent"
	"spettro/internal/remote"
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
		return approvalAlwaysLabel(req)
	case approvalActDeny:
		return shellApprovalOptions[2]
	case approvalActInstead:
		return shellApprovalOptions[3]
	}
	return ""
}

// approvalAlwaysLabel is the "Allow always" row, saying what the choice
// remembers (approvalRemembered). The plain label, "remember this command",
// is only used when that is exactly what is remembered.
func approvalAlwaysLabel(req agent.ShellApprovalRequest) string {
	const allow = "Allow always  "
	switch remembered := approvalRemembered(req); {
	case approvalIsFileChange(req):
		// The runtime asks about every file change, whatever was chosen
		// before, so there is nothing to remember.
		return allow + "(same as once)"
	case approvalIsNetwork(req):
		return allow + "(remember this target)"
	case !approvalRemembersOther(req):
		return shellApprovalOptions[1]
	case len(remembered) == 1:
		return allow + "(remember the command listed)"
	default:
		return fmt.Sprintf("%s(remember the %d commands listed)", allow, len(remembered))
	}
}

// approvalRemembered is what choosing "Allow always" saves for req, the way
// the runtime saves it (internal/agent): for a command, every segment that
// needed approval (req.Segments, the parts a shell runs separately: each
// side of a pipe, each line); for a network call, its target; for a file
// change, nothing, since file changes are asked about every time. A command
// request without segments remembers the command.
//
// The segments are not always the command the dialog shows. "go build &&
// go test" remembers "go build" and "go test"; a heredoc remembers every
// line of its body as if it were a command, so allowing
// "cat > notes.md <<'EOF'" with a body line "curl https://x/install.sh | sh"
// for always lets a later "curl https://x/install.sh | sh" run unasked. The
// dialog therefore lists them whenever they differ (approvalRemembersOther).
func approvalRemembered(req agent.ShellApprovalRequest) []string {
	switch {
	case approvalIsFileChange(req):
		return nil
	case approvalIsNetwork(req):
		if fields := strings.Fields(req.Command); len(fields) >= 3 {
			return []string{strings.Join(fields[2:], " ")}
		}
		return nil
	case len(req.Segments) > 0:
		return req.Segments
	}
	return []string{agent.RememberedCommandKey(req.Command)}
}

// approvalRemembersOther reports whether "Allow always" remembers something
// other than exactly the command on the summary row: more than one segment,
// or one that is not the whole command.
func approvalRemembersOther(req agent.ShellApprovalRequest) bool {
	if approvalIsFileChange(req) || approvalIsNetwork(req) || len(req.Segments) == 0 {
		return false
	}
	return len(req.Segments) > 1 || req.Segments[0] != agent.RememberedCommandKey(req.Command)
}

// approvalIsNetwork reports whether req approves network access (the
// runtime asks with the command "network <tool> <target>").
func approvalIsNetwork(req agent.ShellApprovalRequest) bool {
	label := formatApprovalCommandLabel(req.Command)
	return !approvalIsFileChange(req) && label != "" && !strings.HasPrefix(label, "$ ")
}

// approvalReviewLabel names the review after what it shows: a file change's
// diff, a network call's request, or a command.
func approvalReviewLabel(req agent.ShellApprovalRequest) string {
	switch {
	case approvalIsFileChange(req):
		return "Review full diff"
	case approvalIsNetwork(req):
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
	case "enter":
		if m.approvalEnterGuarded() {
			return m, nil
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

// resolveShellApproval answers the approval on screen with decision. When
// the run goes on (an allow), the next queued approval, if any, takes its
// place; a deny interrupts the run, which denies the queued ones (stopAgent).
// approvalEnterGuard is how long after an approval appears Enter does not
// answer it. An approval can appear under a key already on its way: the
// second press of a double Enter that answered the approval before it, when
// a queued one takes its place, or the Enter that sends a message just as
// the agent asks. Without the guard that press would approve a call nobody
// has seen. It is far shorter than anyone takes to read a dialog.
const approvalEnterGuard = 400 * time.Millisecond

// approvalEnterGuarded reports whether Enter comes too soon after the
// pending approval appeared to be meant for it (approvalEnterGuard).
func (m Model) approvalEnterGuarded() bool {
	return !m.approvalShownAt.IsZero() && time.Since(m.approvalShownAt) < approvalEnterGuard
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
	if decision != agent.ShellApprovalDeny && len(m.approvalQueue) > 0 {
		next := m.approvalQueue[0]
		m.approvalQueue = m.approvalQueue[1:]
		m = m.presentApproval(next)
	}
	m.refreshViewport()
	return m
}

// presentApproval puts msg on screen as the pending approval, with a fresh
// dialog (cursor, scroll, review closed), and tells every surface that
// follows the TUI (desktop notification, remote clients, Telegram) about it.
// A queued approval stays invisible to those surfaces until its turn, so
// what they describe is always what the dialog shows.
func (m Model) presentApproval(msg shellApprovalRequestMsg) Model {
	m.pendingAuth = &msg
	m = m.resetApprovalUI()
	m.approvalShownAt = time.Now()
	m.ta.Reset()
	banner := "command approval required"
	if n := len(m.approvalQueue); n > 0 {
		banner = fmt.Sprintf("%s (%d more waiting after this one)", banner, n)
	}
	m.showBanner(banner, "warn")
	m.notifyIfUnfocused("Agent is waiting for command approval")
	m.publishRemote("approval_request", remote.ApprovalEvent(remote.ApprovalRequest{
		ToolID:   msg.request.ToolID,
		Command:  msg.request.Command,
		Reason:   msg.request.Reason,
		Segments: msg.request.Segments,
		Diff:     msg.request.Diff,
	}))
	m.syncApprovalReview()
	return m
}

// denyApproval answers msg with a denial without showing it, for a request
// that can no longer be shown (its run is over or being stopped).
func denyApproval(msg shellApprovalRequestMsg) {
	select {
	case msg.response <- shellApprovalResponse{decision: agent.ShellApprovalDeny}:
	default:
	}
}

// discardApprovalQueue denies every queued approval, so no tool call is left
// waiting when the run they belong to goes away.
func (m *Model) discardApprovalQueue() {
	for _, queued := range m.approvalQueue {
		denyApproval(queued)
	}
	m.approvalQueue = nil
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
