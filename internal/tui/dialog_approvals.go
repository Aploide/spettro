package tui

import (
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
