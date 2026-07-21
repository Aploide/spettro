package tui

import (
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"spettro/internal/agent"
	"spettro/internal/config"
	"spettro/internal/session"
)

func (m *Model) stopAgent() {
	if m.cancelAgent != nil {
		m.cancelAgent()
		m.cancelAgent = nil
	}
	if m.pendingAuth != nil {
		select {
		case m.pendingAuth.response <- shellApprovalResponse{decision: agent.ShellApprovalDeny}:
		default:
		}
	}
	if m.pendingQuestion != nil {
		select {
		case m.pendingQuestion.response <- askUserResponse{err: fmt.Errorf("cancelled")}:
		default:
		}
	}
	m.thinking = false
	m.toolCh = nil
	m.streamCh = nil
	m.usageCh = nil
	m.approvalCh = nil
	m.askUserCh = nil
	m.liveTools = nil
	m.currentTool = nil
	m.pendingAuth = nil
	m.pendingQuestion = nil
	m.approvalCursor = 0
	m.questionCursor = 0
	m.questionFreeform = false
	m.progressNote = ""
	m.activePrompt = nil
	m.activeAgentID = ""
	if m.activeGoal != nil {
		m.activeGoal = nil // Clear goal on user interrupt (stop/Esc)
		m.goalResumeAfterCompact = false
		m.pushSystemMsg("goal abandoned by user")
	}
}

func (m *Model) pushSystemMsg(content string) {
	m.messages = append(m.messages, ChatMessage{
		Role:    RoleSystem,
		Content: content,
		At:      time.Now(),
	})
	m.autoSaveDebounced()
	m.publishRemote("system_message", map[string]any{"content": content})
}

func (m *Model) showBanner(text, kind string) {
	m.banner = text
	m.bannerKind = kind
	m.bannerClearAt = time.Now().Add(3 * time.Second)
	m.publishRemote("banner", map[string]any{"text": text, "level": kind})
}

func (m *Model) persistUIState() {
	_ = m.updateConfig(func(cfg *config.UserConfig) error {
		cfg.LastAgentID = m.mode
		cfg.ShowSidePanel = m.showSidePanel
		return nil
	})
}

func (m *Model) updateConfig(mut func(*config.UserConfig) error) error {
	cfg, err := config.Update(mut)
	if err != nil {
		return err
	}
	m.cfg = cfg
	if m.providers != nil {
		m.providers.SetAPIKeys(cfg.APIKeys)
	}
	if m.livePerm != nil {
		m.livePerm.set(cfg.Permission)
	}
	return nil
}

// livePermission is a concurrency-safe holder for the user's current
// permission level, shared between the TUI event loop (which writes it on
// every config change) and in-flight agent goroutines (which read it before
// each approval decision). It is what makes /permission apply mid-run.
type livePermission struct{ v atomic.Value }

func (l *livePermission) set(p config.PermissionLevel) { l.v.Store(p) }

func (l *livePermission) get() config.PermissionLevel {
	if p, ok := l.v.Load().(config.PermissionLevel); ok {
		return p
	}
	return ""
}

func (m *Model) setProgressNote(text string) {
	text = strings.TrimSpace(text)
	if text == "" || text == m.progressNote {
		m.progressNote = text
		return
	}
	m.progressNote = text
	m.messages = append(m.messages, ChatMessage{
		Role:    RoleAssistant,
		Kind:    "comment",
		Content: text,
		At:      time.Now(),
	})
	m.autoSaveDebounced()
	m.publishRemote("comment", map[string]any{"message": text})
}

// streamKinds are the transient, in-place message kinds used to render live
// thinking and answer tokens during a run. They are never persisted (see
// autoSave) and are collapsed into the authoritative final message at run end.
const (
	kindThinkingStream = "thinking-stream"
	kindAnswerStream   = "answer-stream"
)

// applyStreamChunk routes a demultiplexed stream chunk to the matching live
// message, creating or extending it in place.
func (m *Model) applyStreamChunk(c agent.StreamChunk) {
	switch c.Kind {
	case agent.StreamKindThinking:
		m.appendOrUpdateStream(kindThinkingStream, c.Delta)
	case agent.StreamKindAnswer:
		if c.Reset {
			m.clearStreamKind(kindAnswerStream)
		}
		m.appendOrUpdateStream(kindAnswerStream, c.Delta)
	}
}

// appendOrUpdateStream extends the trailing live message of the given kind, or
// starts a new block when the last message is something else (e.g. a tool
// trace). Appending a fresh block after an interrupting message keeps thinking
// and answer text chronologically ordered with tool activity.
func (m *Model) appendOrUpdateStream(kind, delta string) {
	if delta == "" {
		return
	}
	if n := len(m.messages); n > 0 {
		last := &m.messages[n-1]
		if last.Role == RoleAssistant && last.Kind == kind {
			last.Content += delta
			last.At = time.Now()
			return
		}
	}
	m.messages = append(m.messages, ChatMessage{
		Role:    RoleAssistant,
		Kind:    kind,
		Content: delta,
		At:      time.Now(),
	})
}

// clearStreamKind drops all live messages of the given kind (used to discard an
// answer draft on reset).
func (m *Model) clearStreamKind(kind string) {
	out := m.messages[:0]
	for _, msg := range m.messages {
		if msg.Role == RoleAssistant && msg.Kind == kind {
			continue
		}
		out = append(out, msg)
	}
	m.messages = out
}

// collectStreamThinking concatenates the streamed thinking blocks so the run's
// reasoning can be preserved on the final assistant message.
func (m *Model) collectStreamThinking() string {
	var sb strings.Builder
	for _, msg := range m.messages {
		if msg.Role == RoleAssistant && msg.Kind == kindThinkingStream {
			if sb.Len() > 0 {
				sb.WriteString("\n")
			}
			sb.WriteString(msg.Content)
		}
	}
	return strings.TrimSpace(sb.String())
}

// clearStreamMessages removes every transient live-stream message. Called at run
// completion before the authoritative final message is appended.
func (m *Model) clearStreamMessages() {
	out := m.messages[:0]
	for _, msg := range m.messages {
		if msg.Role == RoleAssistant && (msg.Kind == kindThinkingStream || msg.Kind == kindAnswerStream) {
			continue
		}
		out = append(out, msg)
	}
	m.messages = out
}

func (m *Model) appendToolStreamMessage(item ToolItem) {
	m.messages = append(m.messages, ChatMessage{
		Role:  RoleAssistant,
		Kind:  "tool-stream",
		Tools: []ToolItem{item},
		At:    time.Now(),
	})
	m.autoSaveDebounced()
}

func (m *Model) updateToolStreamMessage(item ToolItem) {
	for i := len(m.messages) - 1; i >= 0; i-- {
		msg := &m.messages[i]
		if msg.Role != RoleAssistant || msg.Kind != "tool-stream" || len(msg.Tools) != 1 {
			continue
		}
		tool := msg.Tools[0]
		if tool.Name == item.Name && tool.Args == item.Args && tool.Status == "running" {
			msg.Tools[0] = item
			msg.At = time.Now()
			m.mergeAdjacentToolStreamMessage(i)
			m.autoSaveDebounced()
			return
		}
	}
	m.appendToolStreamMessage(item)
	m.mergeAdjacentToolStreamMessage(len(m.messages) - 1)
}

func (m *Model) mergeAdjacentToolStreamMessage(idx int) {
	if idx <= 0 || idx >= len(m.messages) {
		return
	}
	curr := m.messages[idx]
	if curr.Kind != "tool-stream" || len(curr.Tools) != 1 || curr.Tools[0].Status == "running" {
		return
	}
	prev := &m.messages[idx-1]
	if prev.Role != RoleAssistant || prev.Kind != "tool-stream" || len(prev.Tools) == 0 {
		return
	}
	if prev.Tools[0].Status == "running" || prev.Tools[0].Name != curr.Tools[0].Name {
		return
	}
	prev.Tools = append(prev.Tools, curr.Tools[0])
	m.messages = append(m.messages[:idx], m.messages[idx+1:]...)
}

// attachToolDiff sets the asynchronously-computed diff on the tool entry whose
// Seq matches, both in the rendered tool-stream messages and in m.liveTools so
// an interrupt summary and the side-panel detail stay consistent.
func (m *Model) attachToolDiff(seq int, diff string) {
	for i := range m.messages {
		if m.messages[i].Kind != "tool-stream" {
			continue
		}
		for j := range m.messages[i].Tools {
			if m.messages[i].Tools[j].Seq == seq {
				m.messages[i].Tools[j].Diff = diff
			}
		}
	}
	for i := range m.liveTools {
		if m.liveTools[i].Seq == seq {
			m.liveTools[i].Diff = diff
		}
	}
}

func (m *Model) queuePrompt(input, prompt string, mentionedFiles, images []string) {
	m.pendingPrompts = append(m.pendingPrompts, queuedPrompt{
		Input:          input,
		Prompt:         prompt,
		MentionedFiles: append([]string(nil), mentionedFiles...),
		Images:         append([]string(nil), images...),
	})
}

func (m *Model) nextQueuedPrompt() (queuedPrompt, bool) {
	if len(m.pendingPrompts) == 0 {
		return queuedPrompt{}, false
	}
	next := m.pendingPrompts[0]
	m.pendingPrompts = append([]queuedPrompt(nil), m.pendingPrompts[1:]...)
	return next, true
}

func compactRunSummary(tools []ToolItem, current *ToolItem) string {
	var parts []string
	for _, t := range tools {
		label := formatToolLabel(t.Name, t.Args)
		if strings.TrimSpace(label) == "" {
			label = t.Name
		}
		switch t.Status {
		case "error":
			parts = append(parts, label+" (failed)")
		default:
			parts = append(parts, label)
		}
	}
	if current != nil {
		label := formatRunningLabel(current.Name, current.Args)
		if strings.TrimSpace(label) == "" {
			label = current.Name
		}
		parts = append(parts, label+" (in progress)")
	}
	if len(parts) == 0 {
		return ""
	}
	if len(parts) > 5 {
		extra := len(parts) - 5
		parts = append(parts[:5], fmt.Sprintf("%d more step(s)", extra))
	}
	return strings.Join(parts, "; ")
}

func (m *Model) interruptRun(summaryPrefix string, askInstead bool) {
	if !m.thinking {
		return
	}
	agentID := m.activeAgentID
	if agentID == "" {
		agentID = m.mode
	}
	// Drop transient live-stream drafts; the kept-progress note below is the
	// canonical record of an interrupted run.
	m.clearStreamMessages()
	runSummary := compactRunSummary(m.liveTools, m.currentTool)
	content := strings.TrimSpace(summaryPrefix)
	if runSummary != "" {
		if content != "" {
			content += "\n\n"
		}
		content += "Progress kept:\n" + runSummary
	}
	if strings.TrimSpace(content) != "" {
		m.messages = append(m.messages, ChatMessage{
			Role:    RoleSystem,
			Content: content,
			At:      time.Now(),
		})
	}
	// An interrupt ends the run; persist the kept-progress note immediately.
	m.autoSave()
	m.finishAgentActivity(agentID, "cancelled", content, "")
	m.stopAgent()
	m.awaitingInstead = askInstead
	if askInstead {
		m.ta.Reset()
		m.showBanner("what should I do instead?", "warn")
	} else {
		m.showBanner("stopped", "warn")
	}
	// Refresh the remote snapshot and emit a state event so external clients
	// observe the run ending. Without this, GET /status keeps reporting
	// thinking:true until the next user message updates the snapshot.
	m.publishRemoteState("agent_interrupted")
	m.refreshViewport()
}

func (m *Model) ensureSession() {
	if m.sessionID == "" {
		m.sessionID = session.NewID(m.cwd)
	}
}

func (m *Model) syncTodosFromSession() {
	if m.sessionID == "" {
		return
	}
	todos, err := session.LoadTodos(m.store.GlobalDir, m.sessionID)
	if err == nil {
		m.todos = todos
	}
}

func (m *Model) updateCompactWarningState() {
	eval := m.evaluateCompact()
	level := 0
	if eval.IsError {
		level = 2
	} else if eval.IsWarning {
		level = 1
	}
	if level == m.compactWarningLevel {
		return
	}
	m.compactWarningLevel = level
	switch level {
	case 2:
		m.showBanner("context is near limit; compacting soon is recommended", "warn")
	case 1:
		m.showBanner("context usage is getting high", "info")
	default:
		m.showBanner("context pressure normalized", "success")
	}
}
