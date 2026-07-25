package tui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"

	"spettro/internal/agent"
)

func (m Model) updateShellApproval(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if m.pendingAuth == nil {
		return m, nil
	}
	if m.approvalCursor == 3 {
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
			m.approvalCursor = 0
			m.ta.Reset()
			return m, nil
		default:
			var taCmd tea.Cmd
			m.ta, taCmd = m.ta.Update(msg)
			return m, taCmd
		}
	}
	n := len(shellApprovalOptions)
	switch msg.String() {
	case "ctrl+o":
		if m.pendingAuth.request.Diff != "" {
			m.approvalDiffExpanded = !m.approvalDiffExpanded
		}
		return m, nil
	case "up":
		if m.approvalCursor > 0 {
			m.approvalCursor--
		}
		return m, nil
	case "down", "ctrl+n":
		if m.approvalCursor < n-1 {
			m.approvalCursor++
		}
		return m, nil
	case "enter":
		switch m.approvalCursor {
		case 0:
			return m.resolveShellApproval(agent.ShellApprovalAllowOnce, "command approved once"), nil
		case 1:
			return m.resolveShellApproval(agent.ShellApprovalAllowAlways, "command approved and saved"), nil
		case 2:
			m = m.resolveShellApproval(agent.ShellApprovalDeny, "command denied")
			m.interruptRun("Command denied by user.", true)
			return m, nil
		case 3:
			m.ta.Reset()
			m.showBanner("type what the agent should do instead, then press enter", "info")
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
	m.approvalCursor = 0
	m.approvalDiffExpanded = false
	m.ta.Reset()
	m.showBanner(banner, "info")
	m.refreshViewport()
	return m
}

func askUserOptions(req agent.AskUserRequest) []string {
	options := append([]string(nil), req.Options...)
	if req.AllowFreeResponse {
		options = append(options, askUserFreeResponseOption)
	}
	return options
}

// askUserRecommendedBadge marks the option the agent recommended
// (DefaultOption). It is a suffix rather than a cursor style so it survives
// the user moving the cursor elsewhere.
const askUserRecommendedBadge = "● recommended"

// askUserPickerRows annotates the answer list: the agent's recommended option
// keeps a persistent badge, and the free-text entry is separated from the
// agent's own options so it reads as a client affordance.
func askUserPickerRows(req agent.AskUserRequest) []pickerOption {
	def := strings.TrimSpace(req.DefaultOption)
	options := askUserOptions(req)
	rows := make([]pickerOption, 0, len(options))
	for i, option := range options {
		row := pickerOption{Label: option}
		switch {
		// The free-text entry is always the appended last row.
		case req.AllowFreeResponse && i == len(options)-1:
			row.Separated = len(req.Options) > 0
		case def != "" && strings.EqualFold(option, def):
			row.Badge = askUserRecommendedBadge
		}
		rows = append(rows, row)
	}
	return rows
}

func askUserDefaultCursor(req agent.AskUserRequest) int {
	def := strings.TrimSpace(req.DefaultOption)
	if def == "" {
		return 0
	}
	for i, option := range askUserOptions(req) {
		if strings.EqualFold(option, def) {
			return i
		}
	}
	return 0
}

func (m Model) updateAskUserQuestion(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if m.pendingQuestion == nil {
		return m, nil
	}
	req := m.pendingQuestion.request
	options := askUserOptions(req)
	if m.questionFreeform {
		switch msg.String() {
		case "enter":
			answer := strings.TrimSpace(m.ta.Value())
			if answer == "" {
				m.showBanner("type your answer, then press enter", "warn")
				return m, nil
			}
			return m.resolveAskUser(answer, "answer sent"), nil
		case "esc":
			if len(options) > 0 {
				m.questionFreeform = false
				m.ta.Reset()
				m.showBanner("choose an option or press esc again to decline", "info")
				return m, nil
			}
			return m.rejectAskUser("question declined"), nil
		default:
			var taCmd tea.Cmd
			m.ta, taCmd = m.ta.Update(msg)
			return m, taCmd
		}
	}
	if len(options) == 0 {
		m.questionFreeform = true
		m.ta.Reset()
		return m, nil
	}
	switch msg.String() {
	case "up":
		if m.questionCursor > 0 {
			m.questionCursor--
		}
		return m, nil
	case "down", "ctrl+n":
		if m.questionCursor < len(options)-1 {
			m.questionCursor++
		}
		return m, nil
	case "enter":
		choice := options[m.questionCursor]
		if choice == askUserFreeResponseOption {
			m.questionFreeform = true
			m.ta.Reset()
			m.showBanner("type your answer and press enter", "info")
			return m, nil
		}
		return m.resolveAskUser(choice, "answer sent"), nil
	case "esc":
		return m.rejectAskUser("question declined"), nil
	}
	return m, nil
}

// answerAskUser delivers a reply to the tool call blocked on this question.
// The response channel is buffered and read exactly once, so a non-blocking
// send is enough — and a second send (a race between, say, a Telegram answer
// and a keypress) is dropped rather than deadlocking the UI.
func answerAskUser(msg askUserRequestMsg, resp askUserResponse) {
	select {
	case msg.response <- resp:
	default:
	}
}

// presentQuestion makes a question the active one. Everything that targets
// "the question on screen" — the picker, the textarea, the desktop
// notification, the remote/Telegram answer expectation — is armed here, so a
// queued question stays invisible to those surfaces until its turn.
func (m Model) presentQuestion(msg askUserRequestMsg) Model {
	m.pendingQuestion = &msg
	m.questionCursor = askUserDefaultCursor(msg.request)
	m.questionFreeform = len(msg.request.Options) == 0
	m.ta.Reset()
	banner := "agent is waiting for your answer"
	if n := len(m.questionQueue); n > 0 {
		banner = fmt.Sprintf("%s (%d more after this)", banner, n)
	}
	m.showBanner(banner, "info")
	m.notifyIfUnfocused("Agent is waiting for your answer")
	m.publishRemote("ask_user", map[string]any{
		"question":            msg.request.Question,
		"options":             msg.request.Options,
		"context":             msg.request.Context,
		"default":             msg.request.DefaultOption,
		"allow_free_response": msg.request.AllowFreeResponse,
	})
	return m
}

// advanceQuestionQueue clears the question just answered and promotes the next
// one the agent asked while the user was busy.
func (m Model) advanceQuestionQueue() Model {
	m.pendingQuestion = nil
	m.questionCursor = 0
	m.questionFreeform = false
	m.ta.Reset()
	m.telegramClearAnswerExpectations()
	if len(m.questionQueue) == 0 {
		return m
	}
	next := m.questionQueue[0]
	m.questionQueue = m.questionQueue[1:]
	return m.presentQuestion(next)
}

// discardQuestionQueue answers every waiting question with err, so no tool
// call is left blocked when the run they belong to goes away.
func (m *Model) discardQuestionQueue(err error) {
	for _, queued := range m.questionQueue {
		answerAskUser(queued, askUserResponse{err: err})
	}
	m.questionQueue = nil
}

func (m Model) resolveAskUser(answer, banner string) Model {
	if m.pendingQuestion != nil {
		answerAskUser(*m.pendingQuestion, askUserResponse{answer: answer})
	}
	m.showBanner(banner, "info")
	m = m.advanceQuestionQueue()
	m.refreshViewport()
	return m
}

func (m Model) rejectAskUser(banner string) Model {
	if m.pendingQuestion != nil {
		answerAskUser(*m.pendingQuestion, askUserResponse{err: fmt.Errorf("user declined to answer")})
	}
	m.showBanner(banner, "warn")
	m = m.advanceQuestionQueue()
	m.refreshViewport()
	return m
}
