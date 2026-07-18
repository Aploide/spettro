package tui

import (
	"os"
	"path/filepath"
	"time"

	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"

	"spettro/internal/agent"
	"spettro/internal/config"
	"spettro/internal/provider"
	"spettro/internal/session"
	"spettro/internal/storage"
)

func RenderMarkdownForTesting(md string, width int) string {
	return renderMarkdown(md, width)
}

func PrefixBlockWithBulletForTesting(bullet, body string) string {
	return prefixBlockWithBullet(bullet, body)
}

func FormatToolLabelForTesting(name, argsJSON string) string {
	return formatToolLabel(name, argsJSON)
}

func FormatRunningLabelForTesting(name, argsJSON string) string {
	return formatRunningLabel(name, argsJSON)
}

func FormatApprovalCommandLabelForTesting(command string) string {
	return formatApprovalCommandLabel(command)
}

func SanitizeToolOutputForTesting(output string, maxLines int) string {
	return sanitizeToolOutput(output, maxLines)
}

func ShellApprovalOptionsForTesting() []string {
	return append([]string(nil), shellApprovalOptions...)
}

func IsPlanningEyeModeForTesting(mode string) bool {
	return isPlanningEyeMode(mode)
}

func NewModelForTesting() Model {
	ta := textarea.New()
	ta.Focus()
	tmp := filepath.Join(os.TempDir(), "spettro-tui-tests")
	cfg := config.Default()
	// Point at a reasoning-capable catalog model so thinking-level commands
	// (which are hidden/refused for non-reasoning models) stay testable.
	cfg.ActiveProvider = "anthropic"
	cfg.ActiveModel = "claude-sonnet-4-5"
	pm := provider.NewManager()
	return Model{
		ta:        ta,
		cwd:       tmp,
		cfg:       cfg,
		providers: pm,
		store:     &storage.Store{ProjectDir: filepath.Join(tmp, ".spettro"), GlobalDir: tmp},
	}
}

func (m *Model) SetTextareaValueForTesting(v string) {
	m.ta.SetValue(v)
}

func (m *Model) SetCommandItemsForTesting(items []string) {
	m.cmdItems = make([]commandDef, 0, len(items))
	for _, item := range items {
		m.cmdItems = append(m.cmdItems, commandDef{name: item})
	}
}

func (m *Model) SetInputHistoryForTesting(history []string) {
	m.inputHistory = append([]string(nil), history...)
	m.historyIndex = -1
}

func (m Model) InputHistoryForTesting() []string {
	return append([]string(nil), m.inputHistory...)
}

func IsInstantCommandForTesting(input string) bool {
	return isInstantCommand(input)
}

func (m *Model) SetPendingShellApprovalForTesting(cursor int) {
	m.pendingAuth = &shellApprovalRequestMsg{response: make(chan shellApprovalResponse, 1)}
	m.approvalCursor = cursor
}

func (m *Model) SetPendingAskUserForTesting(req agent.AskUserRequest, freeform bool) {
	m.pendingQuestion = &askUserRequestMsg{
		request:  req,
		response: make(chan askUserResponse, 1),
	}
	m.questionCursor = askUserDefaultCursor(req)
	m.questionFreeform = freeform
}

func (m Model) TextareaValueForTesting() string {
	return m.ta.Value()
}

func (m Model) MessagesForTesting() []ChatMessage {
	return append([]ChatMessage(nil), m.messages...)
}

func (m Model) HistoryBrowsingForTesting() bool {
	return m.historyBrowsing
}

func (m Model) ApprovalCursorForTesting() int {
	return m.approvalCursor
}

func (m Model) HasPendingShellApprovalForTesting() bool {
	return m.pendingAuth != nil
}

func (m Model) HasPendingAskUserForTesting() bool {
	return m.pendingQuestion != nil
}

func (m Model) QuestionCursorForTesting() int {
	return m.questionCursor
}

func (m Model) QuestionFreeformForTesting() bool {
	return m.questionFreeform
}

func (m *Model) SetThinkingForTesting(v bool) {
	m.thinking = v
}

func (m Model) ThinkingForTesting() bool {
	return m.thinking
}

func (m *Model) SetActiveAgentForTesting(id string) {
	m.activeAgentID = id
}

func (m *Model) SetLiveToolsForTesting(tools []ToolItem, current *ToolItem) {
	m.liveTools = append([]ToolItem(nil), tools...)
	if current == nil {
		m.currentTool = nil
		return
	}
	cp := *current
	m.currentTool = &cp
}

func (m Model) PendingPromptCountForTesting() int {
	return len(m.pendingPrompts)
}

func (m Model) AwaitingInsteadForTesting() bool {
	return m.awaitingInstead
}

func (m Model) BannerForTesting() string {
	return m.banner
}

// ThinkingLevelForTesting returns the persisted extended-thinking level so
// tests can assert that /thinking <level> took effect. The value mirrors
// UserConfig.ThinkingLevel and is empty when thinking is off (default).
func (m Model) ThinkingLevelForTesting() string {
	return m.cfg.ThinkingLevel
}

func (m *Model) SetCtrlCAtForTesting(t time.Time) {
	m.ctrlCAt = t
}

func (m Model) ProgressNoteForTesting() string {
	return m.progressNote
}

func (m Model) ModeForTesting() string {
	return m.mode
}

func (m Model) SidePanelVisibleForTesting() bool {
	return m.showSidePanel
}

func (m *Model) AddMessageForTesting(msg ChatMessage) {
	m.messages = append(m.messages, msg)
}

// MutateMessageContentForTesting edits a message's content in place so tests
// can verify the render cache re-renders a mutated message rather than serving
// a stale block.
func (m *Model) MutateMessageContentForTesting(idx int, content string) {
	if idx >= 0 && idx < len(m.messages) {
		m.messages[idx].Content = content
	}
}

func (m *Model) RenderMessagesForTesting() string {
	return m.renderMessages()
}

// RenderCacheSizeForTesting returns the number of cached message blocks, or -1
// when no cache exists. Used to assert PERF-1 caching behavior.
func (m Model) RenderCacheSizeForTesting() int {
	if m.renderCache == nil {
		return -1
	}
	return len(m.renderCache.blocks)
}

// RenderCacheWidthForTesting returns the layout width the cache was built for.
func (m Model) RenderCacheWidthForTesting() int {
	if m.renderCache == nil {
		return -1
	}
	return m.renderCache.width
}

func (m Model) ActivityCountForTesting() int {
	return len(m.activityFeed)
}

func (m Model) UpdateMainForTesting(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	return m.updateMain(msg)
}

func (m Model) UpdateForTesting(msg tea.Msg) (tea.Model, tea.Cmd) {
	return m.update(msg)
}

func (m Model) HandleCommandForTesting(input string) (tea.Model, tea.Cmd) {
	return m.handleCommand(input)
}

func (m Model) TriggerQuitWarningTimeoutForTesting() (tea.Model, tea.Cmd) {
	return m.update(quitWarningMsg{})
}

func ToolProgressMsgForTesting(name, status, args, output string) tea.Msg {
	return toolProgressMsg{trace: agent.ToolTrace{
		Name:   name,
		Status: status,
		Args:   args,
		Output: output,
	}}
}

func StreamChunkMsgForTesting(kind, delta string, reset bool) tea.Msg {
	return streamChunkMsg{chunk: agent.StreamChunk{Kind: kind, Delta: delta, Reset: reset}}
}

// SetStreamChForTesting installs a non-nil stream channel so the streamChunkMsg
// handler re-arms its wait command (matching a live run).
func (m *Model) SetStreamChForTesting() {
	m.streamCh = make(chan agent.StreamChunk, 8)
}

func AgentDoneMsgForTesting(content string) tea.Msg {
	return agentDoneMsg{content: content}
}

func (m Model) UpdateShellApprovalForTesting(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	return m.updateShellApproval(msg)
}

func (m Model) ShowSteerChoiceForTesting() bool {
	return m.showSteerChoice
}

func (m Model) SteerPendingForTesting() string {
	return m.steerPending
}

// SteeringQueueForTesting returns the Model's mid-run steering queue (nil
// until a run has started or a test installs one).
func (m Model) SteeringQueueForTesting() *agent.SteeringQueue {
	return m.steering
}

func (m *Model) SetSteeringQueueForTesting(q *agent.SteeringQueue) {
	m.steering = q
}

func SteerChoiceOptionsForTesting() []string {
	return append([]string(nil), steerChoiceOptions...)
}

func AskUserOptionsForTesting(req agent.AskUserRequest) []string {
	return askUserOptions(req)
}

func (m Model) UpdateAskUserQuestionForTesting(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	return m.updateAskUserQuestion(msg)
}

func (m *Model) SetDimensionsForTesting(width, height int) {
	m.width = width
	m.height = height
}

func (m *Model) SetSidePanelVisibleForTesting(v bool) {
	m.showSidePanel = v
}

func (m *Model) SetShowToolsForTesting(v bool) {
	m.showTools = v
}

func (m *Model) SetSideDetailScrollForTesting(v int) {
	m.sideDetailScroll = v
}

func (m Model) SideDetailScrollForTesting() int {
	return m.sideDetailScroll
}

func (m *Model) SetResumeItemsForTesting(items []session.Summary) {
	m.resumeItems = append([]session.Summary(nil), items...)
}

func (m *Model) SetShowResumeForTesting(v bool) {
	m.showResume = v
}

func (m Model) ResumeCursorForTesting() int {
	return m.resumeCursor
}

func (m Model) ViewForTesting() string {
	return m.View().Content
}

func (m *Model) AddActivityForTesting(kind, id, agentID, title, detail, body, status string) {
	m.activityFeed = append(m.activityFeed, activityItem{
		Key:     title + time.Now().Format(time.RFC3339Nano),
		Kind:    kind,
		ID:      id,
		AgentID: agentID,
		Title:   title,
		Detail:  detail,
		Body:    body,
		Status:  status,
		At:      time.Now(),
	})
}

func (m *Model) SetGitBranchForTesting(branch string) {
	m.gitBranch = branch
}

func (m *Model) AddModifiedFileForTesting(path string, added, deleted int, untracked, staged, unstaged bool) {
	m.modifiedFiles = append(m.modifiedFiles, modifiedFileEntry{
		Path:      path,
		Added:     added,
		Deleted:   deleted,
		Untracked: untracked,
		Staged:    staged,
		Unstaged:  unstaged,
	})
}

func (m Model) SidePanelWidthForTesting() int {
	return m.sidePanelWidth()
}

func (m Model) ViewSidePanelForTesting(width int) string {
	return m.viewSidePanel(width)
}

func (m Model) StatusBarMessageForTesting() string {
	return m.statusBarMessage()
}

func (m Model) MouseCaptureOffForTesting() bool {
	return m.mouseCaptureOff
}

func PrimaryAgentIDsForTesting(manifest config.AgentManifest) []string {
	return primaryAgentIDs(manifest)
}

func (m *Model) SetManifestForTesting(manifest config.AgentManifest) {
	m.manifest = manifest
}

func (m *Model) SetModeForTesting(mode string) {
	m.mode = mode
}

func (m *Model) RebuildActivitiesFromEventsForTesting(events []session.AgentEvent) {
	m.rebuildActivitiesFromEvents(events)
}

func ParseRemotePortForTesting(arg string) (int, error) {
	_, port, err := parseRemoteArg(arg)
	return port, err
}

func (m Model) HasRemoteServerForTesting() bool {
	return m.remoteServer != nil
}

func (m Model) RemoteAddressForTesting() string {
	return m.remoteAddress()
}

func (m Model) RemoteHostForTesting() string {
	if m.remoteServer == nil {
		return ""
	}
	return m.remoteServer.Host()
}

func (m Model) RemoteTokenForTesting() string {
	if m.remoteServer == nil {
		return ""
	}
	return m.remoteServer.Token()
}

// MarkReadyAndTrustedForTesting bypasses the first-run trust dialog so tests
// can drive the chat viewport directly.
func (m *Model) MarkReadyAndTrustedForTesting() {
	m.ready = true
	m.showTrust = false
}

// ParseMediaTraceOutputForTesting exposes the JSON envelope parser the
// Telegram media dispatcher uses to extract file paths from grok-image /
// grok-video tool traces.
func ParseMediaTraceOutputForTesting(output string) ([]string, string) {
	return parseMediaTraceOutput(output)
}

// MediaCaptionForTesting renders the tool-specific Telegram caption the
// dispatcher would attach to an upload.
func MediaCaptionForTesting(toolName, prompt string) string {
	return mediaCaption(toolName, prompt)
}

// MediaAbsolutePathForTesting resolves a workspace-relative path against a
// cwd, mirroring the logic used by the Telegram dispatcher.
func MediaAbsolutePathForTesting(cwd, p string) string {
	return mediaAbsolutePath(cwd, p)
}

// RecalcLayoutForTesting forces the layout pass that the public Update would
// normally run after every message.
func (m Model) RecalcLayoutForTesting() Model {
	return m.recalcLayout()
}
