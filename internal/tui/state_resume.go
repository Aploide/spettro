package tui

import (
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"spettro/internal/config"
	"spettro/internal/session"
)

func (m Model) loadSessionSummary(sel session.Summary) (session.State, error) {
	return session.Load(m.store.GlobalDir, sel.ID)
}

func (m *Model) rebuildActivitiesFromEvents(events []session.AgentEvent) {
	m.activityFeed = nil
	m.parallelAgents = nil
	m.recentApprovals = nil
	for i, ev := range events {
		at := ev.At
		if at.IsZero() {
			at = time.Now()
		}
		kind := strings.TrimSpace(ev.Kind)
		if kind == "" {
			kind = "agent"
		}
		switch kind {
		case "approval":
			decision := strings.TrimSpace(ev.Decision)
			if decision == "" {
				decision = strings.TrimSpace(ev.Status)
			}
			source := strings.TrimSpace(ev.DecisionSource)
			if source == "" {
				source = "unknown"
			}
			toolID := strings.TrimSpace(ev.ToolID)
			if toolID == "" {
				toolID = "shell-exec"
			}
			segment := strings.TrimSpace(ev.CommandSegment)
			if segment == "" {
				segment = strings.TrimSpace(ev.Task)
			}
			if segment == "" {
				segment = "(unspecified)"
			}
			reason := strings.TrimSpace(ev.Reason)
			if reason == "" {
				reason = strings.TrimSpace(ev.Summary)
			}
			m.recentApprovals = append(m.recentApprovals, ev)
			m.upsertActivity(activityItem{
				Key:     fmt.Sprintf("resume:approval:%d", i),
				Kind:    "approval",
				ID:      toolID,
				AgentID: ev.AgentID,
				Title:   fmt.Sprintf("approval %s (%s)", decision, source),
				Detail:  truncateLabel(segment, 120),
				Body:    reason,
				Status:  decision,
				At:      at,
			})
		case "tool":
			name := strings.TrimSpace(ev.ToolName)
			if name == "" {
				name = "tool"
			}
			title := formatToolLabel(name, ev.ToolArgs)
			if ev.Status == "running" {
				title = formatRunningLabel(name, ev.ToolArgs)
			}
			bodyParts := []string{}
			if summary := summarizeToolArgs(name, ev.ToolArgs); summary != "" {
				bodyParts = append(bodyParts, summary)
			}
			if out := sanitizeToolOutput(ev.ToolOutput, 24); out != "" {
				bodyParts = append(bodyParts, out)
			}
			m.upsertActivity(activityItem{
				Key:     fmt.Sprintf("resume:tool:%d:%s", i, name),
				Kind:    "tool",
				ID:      name,
				AgentID: ev.AgentID,
				Title:   title,
				Detail:  summarizeToolArgs(name, ev.ToolArgs),
				Body:    strings.Join(bodyParts, "\n\n"),
				Status:  ev.Status,
				At:      at,
			})
		case "command":
			command := strings.TrimSpace(ev.Task)
			if command == "" {
				command = ev.Summary
			}
			m.upsertActivity(activityItem{
				Key:     fmt.Sprintf("resume:command:%d", i),
				Kind:    "command",
				ID:      command,
				AgentID: ev.AgentID,
				Title:   command,
				Detail:  "command",
				Body:    command,
				Status:  ev.Status,
				At:      at,
			})
		default:
			title := "agent event"
			if strings.TrimSpace(ev.AgentID) != "" {
				title = fmt.Sprintf("%s session", ev.AgentID)
			}
			m.upsertActivity(activityItem{
				Key:     fmt.Sprintf("resume:agent:%d:%s", i, ev.AgentID),
				Kind:    "agent",
				ID:      ev.AgentID,
				AgentID: ev.AgentID,
				Title:   title,
				Detail:  truncateLabel(strings.TrimSpace(ev.Task), 120),
				Body:    strings.TrimSpace(ev.Summary),
				Status:  ev.Status,
				At:      at,
			})
		}
	}
}

func (m Model) updateResume(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc", "ctrl+c":
		m.showResume = false
	case "up", "shift+tab":
		if m.resumeCursor > 0 {
			m.resumeCursor--
		}
		m.ensureResumeWindow()
	case "down", "ctrl+n", "tab":
		if m.resumeCursor < len(m.resumeItems)-1 {
			m.resumeCursor++
		}
		m.ensureResumeWindow()
	case "pgup":
		step := max(1, m.resumeMaxRows()-2)
		m.resumeCursor = max(0, m.resumeCursor-step)
		m.ensureResumeWindow()
	case "pgdown":
		step := max(1, m.resumeMaxRows()-2)
		m.resumeCursor = min(len(m.resumeItems)-1, m.resumeCursor+step)
		m.ensureResumeWindow()
	case "home":
		m.resumeCursor = 0
		m.ensureResumeWindow()
	case "end":
		if len(m.resumeItems) > 0 {
			m.resumeCursor = len(m.resumeItems) - 1
		}
		m.ensureResumeWindow()
	case "enter":
		if len(m.resumeItems) > 0 {
			sel := m.resumeItems[m.resumeCursor]
			state, err := m.loadSessionSummary(sel)
			if err != nil {
				m.showResume = false
				m.showBanner("failed to load conversation: "+err.Error(), "error")
				return m, nil
			}
			m.sessionID = state.Metadata.ID
			m.todos = state.Todos
			m.parallelAgents = nil
			m.activityFeed = nil
			// The structured carried history belongs to the previous in-memory
			// conversation; drop it so the resumed session's first turn rebuilds
			// context from the loaded transcript instead.
			m.convHistory = nil
			m.messages = make([]ChatMessage, 0, len(state.Messages))
			for _, cm := range state.Messages {
				m.messages = append(m.messages, ChatMessage{
					Role:     Role(cm.Role),
					Content:  cm.Content,
					Thinking: cm.Thinking,
					Meta:     cm.Meta,
					At:       cm.At,
				})
			}
			m.rebuildActivitiesFromEvents(state.Events)
			// Restore usage counters so /stats continues from the saved session.
			if state.Metadata.Stats != nil {
				m.providers.RestoreUsage(*state.Metadata.Stats)
			} else {
				m.providers.ResetUsage()
			}
			// Restore unfinished goal (step 05): surface it but do NOT auto-start.
			m.pendingGoalResume = nil
			if state.Metadata.Goal != nil && state.Metadata.Goal.Active {
				m.pendingGoalResume = state.Metadata.Goal
			}
			m.showResume = false
			m.refreshViewport()
			m.showBanner(fmt.Sprintf("resumed conversation from %s", state.Metadata.StartedAt.Format("2006-01-02 15:04")), "success")
			if m.pendingGoalResume != nil {
				gr := m.pendingGoalResume
				m.pushSystemMsg(fmt.Sprintf(
					"Unfinished goal found: %q (iteration %d). Type /goal resume to continue, or /goal stop to discard.",
					gr.Objective, gr.Iteration))
			}
		}
	}
	return m, nil
}

func (m *Model) ensureResumeWindow() {
	if len(m.resumeItems) == 0 {
		m.resumeCursor = 0
		m.resumeScroll = 0
		return
	}
	if m.resumeCursor < 0 {
		m.resumeCursor = 0
	}
	if m.resumeCursor >= len(m.resumeItems) {
		m.resumeCursor = len(m.resumeItems) - 1
	}
	maxRows := m.resumeMaxRows()
	maxStart := max(0, len(m.resumeItems)-maxRows)
	if m.resumeScroll < 0 {
		m.resumeScroll = 0
	}
	if m.resumeScroll > maxStart {
		m.resumeScroll = maxStart
	}
	if m.resumeCursor < m.resumeScroll {
		m.resumeScroll = m.resumeCursor
	}
	if m.resumeCursor >= m.resumeScroll+maxRows {
		m.resumeScroll = m.resumeCursor - maxRows + 1
	}
}

func (m Model) resumeMaxRows() int {
	maxRows := max(m.height-12, 4)
	return maxRows
}

func (m Model) viewResume() string {
	mc := m.currentColor()
	title := lipgloss.NewStyle().Bold(true).Foreground(mc).Render("◈ resume conversation")
	dialogWidth := 72
	if m.width < dialogWidth+4 {
		dialogWidth = m.width - 4
	}
	if dialogWidth < 30 {
		dialogWidth = 30
	}

	var rows []string
	for i, s := range m.resumeItems {
		isSelected := i == m.resumeCursor
		timeStr := s.StartedAt.Format("2006-01-02 15:04")
		preview := s.Preview
		if preview == "" {
			preview = "(empty)"
		}
		preview = strings.ReplaceAll(preview, "\n", " ")
		var prefix string
		var timeStyle, previewStyle lipgloss.Style
		if isSelected {
			prefix = lipgloss.NewStyle().Foreground(mc).Bold(true).Render("› ")
			timeStyle = lipgloss.NewStyle().Foreground(colorText).Bold(true)
			previewStyle = lipgloss.NewStyle().Foreground(colorMuted)
		} else {
			prefix = "  "
			timeStyle = lipgloss.NewStyle().Foreground(colorMuted)
			previewStyle = lipgloss.NewStyle().Foreground(colorDim)
		}
		prefixWidth := lipgloss.Width(prefix)
		timeWidth := lipgloss.Width(timeStr) + 2
		previewBudget := max(8, dialogWidth-prefixWidth-timeWidth-6)
		rows = append(rows, prefix+timeStyle.Render(timeStr)+"  "+previewStyle.Render(truncateLabel(preview, previewBudget)))
	}
	if len(rows) == 0 {
		rows = append(rows, styleMuted.Render("  no saved conversations"))
	}

	hint := styleMuted.Render("↑↓ navigate  pgup/pgdn jump  enter load  esc close")
	maxRows := m.resumeMaxRows()
	if len(rows) > maxRows {
		start := max(m.resumeScroll, 0)
		if start+maxRows > len(rows) {
			start = len(rows) - maxRows
		}
		if start < 0 {
			start = 0
		}
		rows = rows[start:min(len(rows), start+maxRows)]
	}

	dialog := lipgloss.NewStyle().
		BorderStyle(lipgloss.RoundedBorder()).
		BorderForeground(mc).
		Width(dialogWidth+2).
		Padding(1, 2).
		Render(lipgloss.JoinVertical(lipgloss.Left,
			title, "",
			strings.Join(rows, "\n"),
			"",
			hint,
		))

	return lipgloss.Place(m.width, m.height,
		lipgloss.Center, lipgloss.Center,
		dialog,
		lipgloss.WithWhitespaceChars(" "),
		lipgloss.WithWhitespaceStyle(lipgloss.NewStyle().Foreground(colorDim)),
	)
}

func (m Model) updateTrust(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "up", "shift+tab":
		if m.trustCursor > 0 {
			m.trustCursor--
		}
	case "down", "ctrl+n", "tab":
		if m.trustCursor < 2 {
			m.trustCursor++
		}
	case "enter":
		switch m.trustCursor {
		case 0:
			m.showTrust = false
			m.pushSystemMsg("spettro ready — /help for commands, shift+tab to switch mode")
			m.refreshViewport()
		case 1:
			_ = config.AddTrusted(m.cwd)
			m.showTrust = false
			m.pushSystemMsg("spettro ready — /help for commands, shift+tab to switch mode")
			m.refreshViewport()
		case 2:
			return m, tea.Quit
		}
	case "1", "y", "Y":
		m.showTrust = false
		m.pushSystemMsg("spettro ready — /help for commands, shift+tab to switch mode")
		m.refreshViewport()
	case "2":
		_ = config.AddTrusted(m.cwd)
		m.showTrust = false
		m.pushSystemMsg("spettro ready — /help for commands, shift+tab to switch mode")
		m.refreshViewport()
	case "3", "n", "N", "esc", "ctrl+c":
		return m, tea.Quit
	}
	return m, nil
}

func (m Model) viewTrust() string {
	mc := m.currentColor()
	title := lipgloss.NewStyle().Bold(true).Foreground(mc).Render("◈ confirm folder trust")
	pathStyle := lipgloss.NewStyle().Foreground(colorText).Bold(true)
	warnStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#FBBF24"))

	options := []string{
		"Yes, trust this session",
		"Yes, and remember this folder",
		"No, exit",
	}

	var optLines []string
	for i, opt := range options {
		var prefix string
		var style lipgloss.Style
		if i == m.trustCursor {
			prefix = lipgloss.NewStyle().Foreground(mc).Bold(true).Render("› ")
			style = lipgloss.NewStyle().Foreground(colorText).Bold(true)
		} else {
			prefix = "  "
			style = lipgloss.NewStyle().Foreground(colorMuted)
		}
		optLines = append(optLines, prefix+style.Render(fmt.Sprintf("%d  %s", i+1, opt)))
	}

	inner := lipgloss.JoinVertical(lipgloss.Left,
		title, "",
		pathStyle.Render("  "+m.cwd),
		"",
		warnStyle.Render("  Spettro may read files and run commands in this folder."),
		styleMuted.Render("  Only trust folders you own and control."),
		"",
		strings.Join(optLines, "\n"),
		"",
		styleMuted.Render("  ↑↓ navigate  enter confirm  1/2/3 direct select"),
	)

	dialogWidth := 64
	if m.width < dialogWidth+4 {
		dialogWidth = m.width - 4
	}
	if dialogWidth < 30 {
		dialogWidth = 30
	}

	dialog := lipgloss.NewStyle().
		BorderStyle(lipgloss.RoundedBorder()).
		BorderForeground(mc).
		Width(dialogWidth+2).
		Padding(1, 2).
		Render(inner)

	return lipgloss.Place(m.width, m.height,
		lipgloss.Center, lipgloss.Center,
		dialog,
		lipgloss.WithWhitespaceChars(" "),
		lipgloss.WithWhitespaceStyle(lipgloss.NewStyle().Foreground(colorDim)),
	)
}
