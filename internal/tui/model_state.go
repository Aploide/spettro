package tui

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

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
	m.publishRemote("system_message", map[string]interface{}{"content": content})
}

func (m *Model) showBanner(text, kind string) {
	m.banner = text
	m.bannerKind = kind
	m.publishRemote("banner", map[string]interface{}{"text": text, "level": kind})
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
	return nil
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
	m.publishRemote("comment", map[string]interface{}{"message": text})
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

func parseNumstat(text string, totals map[string][2]int) {
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 3)
		if len(parts) != 3 {
			continue
		}
		added, _ := strconv.Atoi(parts[0])
		deleted, _ := strconv.Atoi(parts[1])
		path := strings.TrimSpace(parts[2])
		if strings.Contains(path, " -> ") {
			segs := strings.Split(path, " -> ")
			path = strings.TrimSpace(segs[len(segs)-1])
		}
		if path == "" {
			continue
		}
		curr := totals[path]
		totals[path] = [2]int{curr[0] + added, curr[1] + deleted}
	}
}

func normalizeWorkspacePath(cwd, p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	if filepath.IsAbs(p) {
		rel, err := filepath.Rel(cwd, p)
		if err == nil && !strings.HasPrefix(rel, "..") {
			p = rel
		}
	}
	p = filepath.ToSlash(filepath.Clean(p))
	if p == "." || strings.HasPrefix(p, "../") {
		return ""
	}
	return p
}

func (m *Model) markSessionEdit(path string) {
	path = normalizeWorkspacePath(m.cwd, path)
	if path == "" {
		return
	}
	if m.sessionEdits == nil {
		m.sessionEdits = map[string]struct{}{}
	}
	m.sessionEdits[path] = struct{}{}
}

func (m *Model) trackSessionEditFromTrace(t agent.ToolTrace) {
	if t.Name != "file-write" || t.Status == "running" {
		return
	}
	var args struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal([]byte(t.Args), &args); err != nil {
		return
	}
	m.markSessionEdit(args.Path)
}

// minModifiedRefreshInterval throttles how often the modified-files git query
// runs from the Update hot path. A tool-heavy run emits many traces; without
// this guard each one would spawn up to four git subprocesses synchronously on
// the Bubble Tea Update goroutine, serializing the whole UI.
const minModifiedRefreshInterval = time.Second

// queryModifiedFiles runs the git commands needed to compute the side-panel
// branch + modified-file list. It is a pure function (no Model state) so it can
// run inside a tea.Cmd off the Update goroutine. An empty branch with nil files
// means "not a git work tree".
func queryModifiedFiles(cwd string) (branch string, files []modifiedFileEntry) {
	cmd := exec.Command("git", "rev-parse", "--is-inside-work-tree")
	cmd.Dir = cwd
	out, err := cmd.Output()
	if err != nil || strings.TrimSpace(string(out)) != "true" {
		return "", nil
	}

	branch = readGitBranch(cwd)

	cmd = exec.Command("git", "status", "--porcelain")
	cmd.Dir = cwd
	out, err = cmd.Output()
	if err != nil {
		return branch, nil
	}

	stat := make(map[string]modifiedFileEntry)
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimRight(line, "\r")
		if len(line) < 4 {
			continue
		}
		x, y := line[0], line[1]
		path := strings.TrimSpace(line[3:])
		if strings.Contains(path, " -> ") {
			segs := strings.Split(path, " -> ")
			path = strings.TrimSpace(segs[len(segs)-1])
		}
		if path == "" {
			continue
		}
		normPath := normalizeWorkspacePath(cwd, path)
		if normPath == "" {
			continue
		}
		entry := stat[normPath]
		entry.Path = normPath
		entry.Untracked = x == '?' && y == '?'
		entry.Staged = !entry.Untracked && x != ' '
		entry.Unstaged = !entry.Untracked && y != ' '
		stat[normPath] = entry
	}

	numTotals := make(map[string][2]int)
	for _, args := range [][]string{{"diff", "--numstat"}, {"diff", "--cached", "--numstat"}} {
		d := exec.Command("git", args...)
		d.Dir = cwd
		data, derr := d.Output()
		if derr == nil {
			parseNumstat(string(data), numTotals)
		}
	}

	for path, entry := range stat {
		if v, ok := numTotals[path]; ok {
			entry.Added, entry.Deleted = v[0], v[1]
		}
		files = append(files, entry)
	}
	sort.Slice(files, func(i, j int) bool {
		return files[i].Path < files[j].Path
	})
	return branch, files
}

// refreshModifiedFiles synchronously updates the side-panel git state. It is
// used for the one-time startup query in New() and as the message applier for
// modifiedFilesMsg. Hot-path callers should prefer scheduleModifiedRefresh.
func (m *Model) refreshModifiedFiles() {
	m.gitBranch, m.modifiedFiles = queryModifiedFiles(m.cwd)
	m.lastModifiedRefreshAt = time.Now()
}

// scheduleModifiedRefresh returns a tea.Cmd that recomputes the modified-files
// list off the Update goroutine, throttled to minModifiedRefreshInterval.
// Returns nil when a refresh ran too recently (the side panel is eventually
// consistent, so dropping a redundant refresh is safe).
func (m *Model) scheduleModifiedRefresh() tea.Cmd {
	if !m.lastModifiedRefreshAt.IsZero() && time.Since(m.lastModifiedRefreshAt) < minModifiedRefreshInterval {
		return nil
	}
	m.lastModifiedRefreshAt = time.Now()
	return refreshModifiedFilesCmd(m.cwd)
}

// refreshModifiedFilesCmd runs queryModifiedFiles in the background.
func refreshModifiedFilesCmd(cwd string) tea.Cmd {
	return func() tea.Msg {
		branch, files := queryModifiedFiles(cwd)
		return modifiedFilesMsg{branch: branch, files: files}
	}
}

func readGitBranch(cwd string) string {
	cmd := exec.Command("git", "branch", "--show-current")
	cmd.Dir = cwd
	out, err := cmd.Output()
	if err == nil {
		branch := strings.TrimSpace(string(out))
		if branch != "" {
			return branch
		}
	}

	cmd = exec.Command("git", "rev-parse", "--short", "HEAD")
	cmd.Dir = cwd
	out, err = cmd.Output()
	if err != nil {
		return "(unknown)"
	}
	hash := strings.TrimSpace(string(out))
	if hash == "" {
		return "(unknown)"
	}
	return "detached@" + hash
}

func computeFileDiff(cwd, name, argsJSON, status string) string {
	if status != "success" {
		return ""
	}
	if name != "file-write" && name != "file-edit" {
		return ""
	}
	var args struct {
		Path string `json:"path"`
	}
	if json.Unmarshal([]byte(argsJSON), &args) != nil || strings.TrimSpace(args.Path) == "" {
		return ""
	}
	path := strings.TrimSpace(args.Path)

	// Try working-tree diff vs HEAD (covers modified tracked files).
	cmd := exec.Command("git", "diff", "HEAD", "--", path)
	cmd.Dir = cwd
	if out, err := cmd.Output(); err == nil && len(strings.TrimSpace(string(out))) > 0 {
		return string(out)
	}

	// Try staged diff vs HEAD (file was git-added before we see the trace).
	cmd2 := exec.Command("git", "diff", "--cached", "--", path)
	cmd2.Dir = cwd
	if out2, err2 := cmd2.Output(); err2 == nil && len(strings.TrimSpace(string(out2))) > 0 {
		return string(out2)
	}

	// New / untracked file: format entire content as additions.
	absPath := path
	if !filepath.IsAbs(path) {
		absPath = filepath.Join(cwd, path)
	}
	content, err := exec.Command("git", "ls-files", "--others", "--exclude-standard", "--", absPath).Output()
	if err != nil || len(strings.TrimSpace(string(content))) == 0 {
		return ""
	}
	data, readErr := os.ReadFile(absPath)
	if readErr != nil {
		return ""
	}
	return buildNewFileDiff(path, string(data))
}

// computeFileDiffCmd runs computeFileDiff off the Update goroutine and returns
// a toolDiffMsg carrying the diff plus the seq of the tool entry it belongs to.
// computeFileDiff itself early-returns "" for non file-write/file-edit tools,
// so the cmd is cheap to spawn for every completed tool. When the diff is empty
// the cmd still resolves (the handler simply finds nothing to attach).
func computeFileDiffCmd(seq int, cwd, name, argsJSON, status string) tea.Cmd {
	return func() tea.Msg {
		return toolDiffMsg{seq: seq, diff: computeFileDiff(cwd, name, argsJSON, status)}
	}
}

func buildNewFileDiff(path, content string) string {
	lines := strings.Split(content, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "--- /dev/null\n+++ b/%s\n@@ -0,0 +1,%d @@\n", path, len(lines))
	for _, l := range lines {
		sb.WriteString("+")
		sb.WriteString(l)
		sb.WriteString("\n")
	}
	return sb.String()
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
		if parent, ok := m.manifest.AgentByID(args.ParentAgentID); ok && parent.Mode == "worker" {
			kind = "microagent"
		}
		entry := parallelAgentEntry{
			ID:       agentID,
			Label:    agentID,
			Kind:     kind,
			Instance: instance + 1,
			Task:     task,
			Status:   "running",
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
	for i, a := range m.parallelAgents {
		if a.ID == agentID && a.Status == "running" {
			m.parallelAgents = append(m.parallelAgents[:i], m.parallelAgents[i+1:]...)
			break
		}
	}
	m.ensureSession()
	status := "done"
	if t.Status == "error" {
		status = "failed"
	}
	_ = session.AppendEvent(m.store.GlobalDir, m.sessionID, session.AgentEvent{
		Kind:          "agent",
		AgentID:       agentID,
		AgentType:     "worker",
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
const autoSaveMinInterval = 2 * time.Second

// autoSaveDebounced persists the session at most once per autoSaveMinInterval.
// Use it on high-frequency mutation paths (tool-stream updates, progress
// comments). Critical, low-frequency persistence points should call autoSave()
// directly so nothing is lost.
func (m *Model) autoSaveDebounced() {
	if !m.lastAutoSaveAt.IsZero() && time.Since(m.lastAutoSaveAt) < autoSaveMinInterval {
		return
	}
	m.autoSave()
}

// flushSave forces an unconditional save, ignoring the debounce window. It is
// the persistence safety-net invoked on exit so the final turn — which may
// have landed inside the debounce window — is never lost.
func (m *Model) flushSave() {
	m.autoSave()
}

func (m *Model) autoSave() {
	hasContent := false
	for _, msg := range m.messages {
		if msg.Role == RoleUser || msg.Role == RoleAssistant {
			hasContent = true
			break
		}
	}
	if !hasContent {
		return
	}
	m.lastAutoSaveAt = time.Now()
	m.ensureSession()
	msgs := make([]session.Message, 0, len(m.messages))
	for _, msg := range m.messages {
		// Transient live-stream drafts are never persisted: their Kind is not
		// serialized, so they would otherwise leak into a mid-run save as a
		// plain assistant message.
		if msg.Role == RoleAssistant && (msg.Kind == kindThinkingStream || msg.Kind == kindAnswerStream) {
			continue
		}
		msgs = append(msgs, session.Message{
			Role:     string(msg.Role),
			Content:  msg.Content,
			Thinking: msg.Thinking,
			Meta:     msg.Meta,
			At:       msg.At,
		})
	}
	if len(msgs) == 0 {
		return
	}
	_ = session.Save(m.store.GlobalDir, session.State{
		Metadata: session.Metadata{
			ID:          m.sessionID,
			ProjectPath: m.cwd,
			ProjectHash: session.ProjectHash(m.cwd),
			StartedAt:   msgs[0].At,
		},
		Messages: msgs,
		Todos:    m.todos,
	})
}

// refreshViewport re-renders the chat transcript into the viewport. It no
// longer persists the session: saving is decoupled (see autoSaveDebounced /
// autoSave) so that scroll, tick, and banner-only refreshes do not trigger a
// full session rewrite.
func (m *Model) refreshViewport() {
	m.vp.SetContent(m.renderMessages())
	m.vp.GotoBottom()
}

func (m Model) renderPlanMessage(msg ChatMessage, mc lipgloss.Color) string {
	innerW := m.paneWidth() - 8
	if innerW < 10 {
		innerW = 10
	}

	header := lipgloss.NewStyle().
		Foreground(mc).Bold(true).
		Render("◈ plan")

	var bodyParts []string
	if len(msg.Tools) > 0 {
		bodyParts = append(bodyParts, renderToolGroups(msg.Tools, m.showTools, mc))
	}
	bodyParts = append(bodyParts, renderMarkdown(strings.TrimSpace(msg.Content), innerW))

	box := lipgloss.NewStyle().
		BorderStyle(lipgloss.RoundedBorder()).
		BorderForeground(mc).
		Width(innerW+2).
		Padding(0, 1).
		Render(strings.Join(bodyParts, "\n"))

	return indent(header+"\n"+box, "  ")
}

func renderAssistantTextBlock(body string, width int) string {
	if strings.TrimSpace(body) == "" {
		return ""
	}
	if width < 10 {
		width = 10
	}
	wrapped := lipgloss.NewStyle().Width(width).Render(body)
	return indent(wrapped, "  ")
}

// renderThinkingBlock renders the model's reasoning as a dim, italic block. When
// live is true a small "streaming" cue marks the in-progress thinking.
func renderThinkingBlock(text string, width int, live bool) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	if width < 10 {
		width = 10
	}
	thinkStyle := lipgloss.NewStyle().Foreground(colorDim).Italic(true)
	header := "  thinking"
	if live {
		header += " …"
	}
	wrapped := lipgloss.NewStyle().Width(width).Render(text)
	return thinkStyle.Render(header + "\n" + indent(wrapped, "  │ "))
}

func renderUserTextBlock(body string, width int, prefix string) string {
	if strings.TrimSpace(body) == "" {
		return ""
	}
	if width < 10 {
		width = 10
	}
	lines := strings.Split(lipgloss.NewStyle().Width(width).Render(body), "\n")
	for i, line := range lines {
		if i == 0 {
			lines[i] = prefix + line
		} else {
			lines[i] = strings.Repeat(" ", lipgloss.Width(prefix)) + line
		}
	}
	return strings.Join(lines, "\n")
}

// renderCacheState memoizes per-message rendered blocks so renderMessages does
// not re-run the markdown regex over the whole transcript on every frame. The
// cache is keyed by a content hash of each message's render-relevant fields and
// scoped to the layout params (width / showTools / color); any change to those
// params invalidates the whole cache. Entries for messages no longer present
// are evicted by rebuilding the map on each call, bounding its size to the
// current transcript.
//
// Access is single-threaded: renderMessages is only ever called from the Bubble
// Tea Update goroutine, never from a background tea.Cmd.
type renderCacheState struct {
	width     int
	showTools bool
	color     string
	blocks    map[uint64]string
}

// renderMessageBlock renders a single chat message to its display string. It is
// the pure, cacheable unit of renderMessages.
func (m Model) renderMessageBlock(msg ChatMessage, mc lipgloss.Color) string {
	switch msg.Role {
	case RoleUser:
		prefix := lipgloss.NewStyle().Foreground(mc).Bold(true).Render("  › ")
		text := lipgloss.NewStyle().Foreground(colorText).Render(msg.Content)
		entry := renderUserTextBlock(text, m.paneWidth()-8, prefix)
		for i := range msg.Images {
			imgLabel := styleMuted.Render(fmt.Sprintf("     [Image #%d]", i+1))
			entry += "\n" + imgLabel
		}
		return entry
	case RoleAssistant:
		if msg.Kind == "plan" {
			return m.renderPlanMessage(msg, mc)
		}
		if msg.Kind == kindThinkingStream {
			return renderThinkingBlock(msg.Content, m.paneWidth()-8, true)
		}
		body := renderMarkdown(msg.Content, m.paneWidth()-8)
		var entryLines []string
		if len(msg.Tools) > 0 {
			entryLines = append(entryLines, renderToolGroups(msg.Tools, m.showTools, mc))
		}
		if strings.TrimSpace(msg.Content) != "" {
			entryLines = append(entryLines, renderAssistantTextBlock(body, m.paneWidth()-8))
		}
		if msg.Meta != "" {
			entryLines = append(entryLines, styleMuted.Render("  "+msg.Meta))
		}
		return strings.Join(entryLines, "\n")
	case RoleSystem:
		return lipgloss.NewStyle().
			Foreground(colorMuted).
			PaddingLeft(4).
			Width(m.paneWidth() - 4).
			Render(msg.Content)
	}
	return ""
}

// messageRenderKey hashes every field that influences how a message renders.
// Length prefixes guard against boundary collisions (e.g. "ab"+"c" vs
// "a"+"bc"). The layout params (width/showTools/color) are NOT folded in here —
// they scope the whole cache and invalidate it wholesale on change.
func messageRenderKey(msg ChatMessage) uint64 {
	h := fnv.New64a()
	writeHashField := func(s string) {
		var buf [8]byte
		binary.LittleEndian.PutUint64(buf[:], uint64(len(s)))
		_, _ = h.Write(buf[:])
		_, _ = io.WriteString(h, s)
	}
	writeHashField(string(msg.Role))
	writeHashField(msg.Kind)
	writeHashField(msg.Content)
	writeHashField(msg.Thinking)
	writeHashField(msg.Meta)
	for _, t := range msg.Tools {
		writeHashField(t.Name)
		writeHashField(t.Status)
		writeHashField(t.Args)
		writeHashField(t.Output)
		writeHashField(t.Diff)
		if t.Open {
			writeHashField("open")
		}
	}
	for _, img := range msg.Images {
		writeHashField(img)
	}
	return h.Sum64()
}

func (m *Model) renderMessages() string {
	if len(m.messages) == 0 {
		return styleMuted.Render("  no messages yet — type a prompt or /help")
	}

	mc := m.currentColor()
	width := m.paneWidth()
	color := string(mc)

	// Reuse the prior cache only when the layout params match; otherwise start
	// fresh so width/showTools/color changes fully re-render.
	var prev map[uint64]string
	if m.renderCache != nil && m.renderCache.width == width &&
		m.renderCache.showTools == m.showTools && m.renderCache.color == color {
		prev = m.renderCache.blocks
	}
	next := make(map[uint64]string, len(m.messages))

	parts := make([]string, 0, len(m.messages))
	for _, msg := range m.messages {
		key := messageRenderKey(msg)
		block, ok := next[key]
		if !ok {
			if block, ok = prev[key]; !ok {
				block = m.renderMessageBlock(msg, mc)
			}
			next[key] = block
		}
		parts = append(parts, block)
	}

	m.renderCache = &renderCacheState{
		width:     width,
		showTools: m.showTools,
		color:     color,
		blocks:    next,
	}

	return strings.Join(parts, "\n\n")
}

func (m Model) recalcLayout() Model {
	eyesH := len(eyesActing)
	headerH := 1
	sepH := 2
	statusH := 1

	inputH := 6
	if len(m.attachments) > 0 {
		inputH++
	}
	if m.showPlanApproval {
		inputH += 2 + len(planApprovalOptions)
	} else if m.pendingAuth != nil {
		inputH += 2 + len(shellApprovalOptions)
	}
	if len(m.mentionItems) > 0 {
		inputH += 5 + len(m.mentionItems)
	}

	parallelH := 0
	if m.sidePanelWidth() <= 0 {
		if pa := m.renderParallelAgents(); pa != "" {
			parallelH = lipgloss.Height(pa)
		}
	}

	fixed := headerH + eyesH + sepH + inputH + statusH + parallelH
	contentH := m.height - fixed
	if contentH < 3 {
		contentH = 3
	}
	vpW := m.paneWidth() - 2
	if vpW < 10 {
		vpW = 10
	}

	m.vp.Width = vpW
	m.vp.Height = contentH
	m.ta.SetWidth(m.paneWidth() - 6)

	return m
}

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

func (m Model) updateResume(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
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
			m.showResume = false
			m.refreshViewport()
			m.showBanner(fmt.Sprintf("resumed conversation from %s", state.Metadata.StartedAt.Format("2006-01-02 15:04")), "success")
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
	maxRows := m.height - 12
	if maxRows < 4 {
		maxRows = 4
	}
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
		start := m.resumeScroll
		if start < 0 {
			start = 0
		}
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
		Width(dialogWidth).
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
		lipgloss.WithWhitespaceForeground(colorDim),
	)
}

func (m Model) updateTrust(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
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
		Width(dialogWidth).
		Padding(1, 2).
		Render(inner)

	return lipgloss.Place(m.width, m.height,
		lipgloss.Center, lipgloss.Center,
		dialog,
		lipgloss.WithWhitespaceChars(" "),
		lipgloss.WithWhitespaceForeground(colorDim),
	)
}
