package tui

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"spettro/internal/agent"
	"spettro/internal/session"
)

func (m *Model) hasSwarmAgents() bool {
	for _, a := range m.parallelAgents {
		if a.Kind == "swarm" {
			return true
		}
	}
	return false
}

func (m *Model) applyToolTraceToObservability(t agent.ToolTrace) {
	if t.Name == "comment" {
		return
	}
	if t.Name == "approval" {
		m.recordApprovalTrace(t)
		return
	}
	m.recordToolActivity(t)
	if t.Name != "agent" {
		return
	}
	var args struct {
		Agent         string `json:"agent"`
		Target        string `json:"target"`
		ID            string `json:"id"`
		Task          string `json:"task"`
		ParentAgentID string `json:"parent_agent_id"`
		Swarm         bool   `json:"swarm"`
	}
	_ = json.Unmarshal([]byte(t.Args), &args)
	agentID := args.Agent
	if agentID == "" {
		agentID = args.Target
	}
	if agentID == "" {
		agentID = args.ID
	}
	if agentID == "" {
		return
	}
	task := args.Task
	if t.Status == "running" {
		instance := 0
		for _, a := range m.parallelAgents {
			if a.ID == agentID {
				instance++
			}
		}
		kind := "worker"
		if args.Swarm {
			kind = "swarm"
			if !m.showSidePanel && !m.hasSwarmAgents() {
				m.showBanner("ultra swarm started — press ctrl+b to watch each agent", "info")
			}
		} else if parent, ok := m.manifest.AgentByID(args.ParentAgentID); ok && parent.Mode == "worker" {
			kind = "microagent"
		}
		entry := parallelAgentEntry{
			ID:       agentID,
			Label:    agentID,
			Kind:     kind,
			Instance: instance + 1,
			Task:     task,
			Status:   "running",
			At:       time.Now(),
		}
		m.parallelAgents = append(m.parallelAgents, entry)
		m.ensureSession()
		_ = session.AppendEvent(m.store.GlobalDir, m.sessionID, session.AgentEvent{
			Kind:          "agent",
			AgentID:       agentID,
			AgentType:     kind,
			ParentAgentID: args.ParentAgentID,
			Task:          task,
			Status:        "running",
		})
		return
	}
	status := "done"
	if t.Status == "error" {
		status = "failed"
	}
	agentType := "worker"
	for i, a := range m.parallelAgents {
		if a.ID == agentID && a.Status == "running" {
			if a.Kind == "swarm" {
				// Swarm members stay listed with their outcome so the side
				// panel shows the whole fan-out, not just what's still running.
				m.parallelAgents[i].Status = status
				agentType = "swarm"
			} else {
				m.parallelAgents = append(m.parallelAgents[:i], m.parallelAgents[i+1:]...)
			}
			break
		}
	}
	m.ensureSession()
	_ = session.AppendEvent(m.store.GlobalDir, m.sessionID, session.AgentEvent{
		Kind:          "agent",
		AgentID:       agentID,
		AgentType:     agentType,
		ParentAgentID: args.ParentAgentID,
		Task:          task,
		Status:        status,
		Summary:       summarizeAgentToolOutput(t.Output),
	})
}

func (m *Model) recordApprovalTrace(t agent.ToolTrace) {
	type approvalPayload struct {
		Decision string `json:"decision"`
		Source   string `json:"source"`
		ToolID   string `json:"tool_id"`
		Segment  string `json:"segment"`
		Reason   string `json:"reason"`
	}
	var payload approvalPayload
	_ = json.Unmarshal([]byte(t.Args), &payload)
	decision := strings.TrimSpace(payload.Decision)
	if decision == "" {
		decision = "unknown"
	}
	toolID := strings.TrimSpace(payload.ToolID)
	if toolID == "" {
		toolID = "shell-exec"
	}
	segment := strings.TrimSpace(payload.Segment)
	if segment == "" {
		segment = "(unspecified)"
	}
	source := strings.TrimSpace(payload.Source)
	if source == "" {
		source = "unknown"
	}
	reason := strings.TrimSpace(payload.Reason)
	if reason == "" {
		reason = strings.TrimSpace(t.Output)
	}
	if reason == "" {
		reason = "n/a"
	}
	ev := session.AgentEvent{
		At:             time.Now(),
		Kind:           "approval",
		AgentID:        m.mode,
		Status:         decision,
		Task:           segment,
		Summary:        reason,
		ToolID:         toolID,
		CommandSegment: segment,
		Decision:       decision,
		DecisionSource: source,
		Reason:         reason,
	}
	m.recentApprovals = append(m.recentApprovals, ev)
	m.upsertActivity(activityItem{
		Key:     fmt.Sprintf("approval:%d", time.Now().UnixNano()),
		Kind:    "approval",
		ID:      toolID,
		AgentID: m.mode,
		Title:   fmt.Sprintf("approval %s (%s)", decision, source),
		Detail:  truncateLabel(segment, 120),
		Body:    reason,
		Status:  decision,
		At:      ev.At,
	})
}

func summarizeAgentToolOutput(output string) string {
	if pretty, ok := formatSubagentEnvelope(strings.TrimSpace(output)); ok {
		return truncateLabel(strings.ReplaceAll(pretty, "\n", " "), 200)
	}
	return truncateLabel(strings.TrimSpace(output), 200)
}

func (m *Model) startAgentActivity(agentID, task string) {
	m.ensureSession()
	m.currentRunKey = fmt.Sprintf("run:%s:%d", agentID, time.Now().UnixNano())
	m.upsertActivity(activityItem{
		Key:     m.currentRunKey,
		Kind:    "agent",
		ID:      agentID,
		AgentID: agentID,
		Title:   fmt.Sprintf("%s session", agentID),
		Detail:  truncateLabel(strings.TrimSpace(task), 120),
		Body:    strings.TrimSpace(task),
		Status:  "running",
		At:      time.Now(),
	})
	_ = session.AppendEvent(m.store.GlobalDir, m.sessionID, session.AgentEvent{
		Kind:      "agent",
		AgentID:   agentID,
		AgentType: "orchestrator",
		Task:      task,
		Status:    "running",
	})
}

func (m *Model) finishAgentActivity(agentID, status, content, thinking string) {
	if m.currentRunKey == "" {
		return
	}
	bodyParts := []string{}
	if strings.TrimSpace(content) != "" {
		bodyParts = append(bodyParts, strings.TrimSpace(content))
	}
	if strings.TrimSpace(thinking) != "" {
		bodyParts = append(bodyParts, "Reasoning\n"+strings.TrimSpace(thinking))
	}
	m.upsertActivity(activityItem{
		Key:     m.currentRunKey,
		Kind:    "agent",
		ID:      agentID,
		AgentID: agentID,
		Title:   fmt.Sprintf("%s session", agentID),
		Detail:  truncateLabel(strings.TrimSpace(content), 120),
		Body:    strings.Join(bodyParts, "\n\n"),
		Status:  status,
		At:      time.Now(),
	})
	_ = session.AppendEvent(m.store.GlobalDir, m.sessionID, session.AgentEvent{
		Kind:      "agent",
		AgentID:   agentID,
		AgentType: "orchestrator",
		Status:    status,
		Summary:   truncateLabel(strings.TrimSpace(content), 200),
	})
	m.currentRunKey = ""
}

func (m *Model) recordAssistantActivity(agentID, content, thinking string, isPlan bool) {
	title := "Assistant response"
	if isPlan {
		title = "Plan output"
	}
	bodyParts := []string{}
	if strings.TrimSpace(content) != "" {
		bodyParts = append(bodyParts, strings.TrimSpace(content))
	}
	if strings.TrimSpace(thinking) != "" {
		bodyParts = append(bodyParts, "Reasoning\n"+strings.TrimSpace(thinking))
	}
	m.upsertActivity(activityItem{
		Key:     fmt.Sprintf("message:%d", time.Now().UnixNano()),
		Kind:    "message",
		ID:      title,
		AgentID: agentID,
		Title:   title,
		Detail:  truncateLabel(strings.TrimSpace(content), 120),
		Body:    strings.Join(bodyParts, "\n\n"),
		Status:  "done",
		At:      time.Now(),
	})
}

func (m *Model) recordToolActivity(t agent.ToolTrace) {
	if t.Name == "comment" {
		return
	}
	agentID := strings.TrimSpace(t.AgentID)
	if agentID == "" {
		agentID = m.mode
	}
	key := fmt.Sprintf("tool:%s:%s", t.Name, t.Args)
	title := formatToolLabel(t.Name, t.Args)
	if t.Status == "running" {
		title = formatRunningLabel(t.Name, t.Args)
	}
	bodyParts := []string{}
	if summary := summarizeToolArgs(t.Name, t.Args); summary != "" {
		bodyParts = append(bodyParts, summary)
	}
	if out := sanitizeToolOutput(t.Output, 24); out != "" {
		bodyParts = append(bodyParts, out)
	}
	m.upsertActivity(activityItem{
		Key:     key,
		Kind:    "tool",
		ID:      t.Name,
		AgentID: agentID,
		Title:   title,
		Detail:  summarizeToolArgs(t.Name, t.Args),
		Body:    strings.Join(bodyParts, "\n\n"),
		Status:  t.Status,
		At:      time.Now(),
	})
	m.ensureSession()
	_ = session.AppendEvent(m.store.GlobalDir, m.sessionID, session.AgentEvent{
		Kind:       "tool",
		AgentID:    agentID,
		Status:     t.Status,
		Summary:    truncateLabel(strings.TrimSpace(strings.Join(bodyParts, " ")), 240),
		ToolName:   t.Name,
		ToolArgs:   t.Args,
		ToolOutput: sanitizeToolOutput(t.Output, 24),
	})
}

func (m *Model) upsertActivity(item activityItem) {
	if item.At.IsZero() {
		item.At = time.Now()
	}
	for i := range m.activityFeed {
		if m.activityFeed[i].Key == item.Key {
			m.activityFeed[i] = item
			return
		}
	}
	m.activityFeed = append(m.activityFeed, item)
}

func extractCommentMessage(argsJSON, output string) string {
	var args struct {
		Message string `json:"message"`
	}
	if json.Unmarshal([]byte(argsJSON), &args) == nil && strings.TrimSpace(args.Message) != "" {
		return strings.TrimSpace(args.Message)
	}
	return strings.TrimSpace(output)
}

func (m *Model) recordCommandEvent(command string) {
	command = strings.TrimSpace(command)
	if command == "" {
		return
	}
	m.ensureSession()
	m.upsertActivity(activityItem{
		Key:     fmt.Sprintf("command:%d", time.Now().UnixNano()),
		Kind:    "command",
		ID:      command,
		AgentID: "tui",
		Title:   command,
		Detail:  "command",
		Body:    command,
		Status:  "done",
		At:      time.Now(),
	})
	_ = session.AppendEvent(m.store.GlobalDir, m.sessionID, session.AgentEvent{
		Kind:    "command",
		AgentID: "tui",
		Task:    command,
		Status:  "done",
		Summary: command,
	})
}

// autoSaveMinInterval is the minimum wall-clock gap between debounced session
// saves. refreshViewport used to call autoSave() on every render — including
// scroll, tick, and banner-only updates — so a long run with frequent tool
// traces re-serialized and rewrote the whole session file dozens of times a
// second. autoSaveDebounced() collapses those into at most one write per
// interval; flushSave()/autoSave() still force an immediate write at the
// critical persistence points (/clear, /compact, resume, quit).
