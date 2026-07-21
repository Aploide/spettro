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

func (m Model) resolveAskUser(answer, banner string) Model {
	if m.pendingQuestion != nil {
		select {
		case m.pendingQuestion.response <- askUserResponse{answer: answer}:
		default:
		}
	}
	m.pendingQuestion = nil
	m.questionCursor = 0
	m.questionFreeform = false
	m.ta.Reset()
	m.telegramClearAnswerExpectations()
	m.showBanner(banner, "info")
	m.refreshViewport()
	return m
}

func (m Model) rejectAskUser(banner string) Model {
	if m.pendingQuestion != nil {
		select {
		case m.pendingQuestion.response <- askUserResponse{err: fmt.Errorf("user declined to answer")}:
		default:
		}
	}
	m.pendingQuestion = nil
	m.questionCursor = 0
	m.questionFreeform = false
	m.ta.Reset()
	m.telegramClearAnswerExpectations()
	m.showBanner(banner, "warn")
	m.refreshViewport()
	return m
}
