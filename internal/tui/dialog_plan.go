package tui

import (
	"time"

	tea "charm.land/bubbletea/v2"
)

var planApprovalOptions = []string{
	"Execute plan  (switch to coding agent)",
	"Don't execute",
	"Edit — tell me what to change",
}

func (m Model) updatePlanApproval(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	n := len(planApprovalOptions)
	switch msg.String() {
	case "up":
		if m.planApprovalCursor > 0 {
			m.planApprovalCursor--
		}
		return m, nil
	case "down", "ctrl+n":
		if m.planApprovalCursor < n-1 {
			m.planApprovalCursor++
		}
		return m, nil
	case "enter":
		choice := m.planApprovalCursor
		m.showPlanApproval = false
		m.planApprovalCursor = 0
		switch choice {
		case 0:
			spec, ok := m.manifest.AgentByID("coding")
			if !ok {
				m.showBanner("coding agent not found", "error")
				return m, nil
			}
			m.mode = "coding"
			m.persistUIState()
			plan := m.pendingPlan
			m.pendingPlan = ""
			return m.runAgentApproved(spec, plan, nil, nil, true)
		case 1:
			m.pendingPlan = ""
			m.showBanner("plan saved to .spettro/PLAN.md — use /approve later to execute", "info")
			return m, nil
		case 2:
			m.showBanner("describe your changes and press enter", "info")
			m.ta.Focus()
			return m, nil
		}
		return m, nil
	case "esc":
		m.showPlanApproval = false
		m.planApprovalCursor = 0
		m.showBanner("plan saved — use /approve to execute later", "info")
		return m, nil
	}
	return m, nil
}

var steerChoiceOptions = []string{
	"Steer now  (inject guidance into the current run)",
	"Queue for after the run",
	"Discard",
}

// updateSteerChoice drives the "steer now or queue?" picker shown when the
// user submits input while an agent run is active (mid-run model steering).
func (m Model) updateSteerChoice(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	n := len(steerChoiceOptions)
	switch msg.String() {
	case "up":
		if m.steerCursor > 0 {
			m.steerCursor--
		}
		return m, nil
	case "down", "ctrl+n":
		if m.steerCursor < n-1 {
			m.steerCursor++
		}
		return m, nil
	case "enter":
		choice := m.steerCursor
		text := m.steerPending
		m.showSteerChoice = false
		m.steerCursor = 0
		m.steerPending = ""
		switch choice {
		case 0: // steer the current run
			if !m.thinking || m.steering == nil {
				// The run finished while the picker was open — fall back to
				// the normal prompt path (starts a fresh run).
				return m.handlePrompt(text)
			}
			m.steering.Push(text)
			m.messages = append(m.messages, ChatMessage{
				Role:    RoleUser,
				Content: text,
				At:      time.Now(),
			})
			m.pushSystemMsg("steering message will be delivered at the agent's next step")
			m.showBanner("steering queued — delivered at the next step boundary", "info")
			m.autoSave()
			m.refreshViewport()
			return m, nil
		case 1: // queue for after the run
			return m.handlePrompt(text)
		}
		// discard: restore the text so nothing typed is lost silently
		m.ta.SetValue(text)
		m.refreshViewport()
		return m, nil
	case "esc":
		m.showSteerChoice = false
		m.steerCursor = 0
		m.ta.SetValue(m.steerPending)
		m.steerPending = ""
		m.refreshViewport()
		return m, nil
	}
	return m, nil
}

func (m Model) handlePlanEdit(editInstruction string) (tea.Model, tea.Cmd) {
	if m.pendingPlan == "" {
		m.showBanner("no pending plan to edit", "warn")
		return m, nil
	}
	spec, ok := m.manifest.AgentByID("plan")
	if !ok {
		m.showBanner("plan agent not found", "error")
		return m, nil
	}
	task := m.pendingPlan + "\n\n---\nUser requested the following changes to the plan:\n" + editInstruction
	m.pendingPlan = ""
	return m.runAgent(spec, task, nil, nil)
}

var shellApprovalOptions = []string{
	"Allow once",
	"Allow always  (remember this command)",
	"Deny",
	"Tell the agent what to do instead",
}

const askUserFreeResponseOption = "Type my own answer"
