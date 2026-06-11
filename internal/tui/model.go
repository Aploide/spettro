package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/atotto/clipboard"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"spettro/internal/agent"
	"spettro/internal/compact"
	"spettro/internal/config"
	"spettro/internal/provider"
	"spettro/internal/remote"
	"spettro/internal/session"
	"spettro/internal/spettro"
	"spettro/internal/storage"
	"spettro/internal/telegram"
)

const coAuthorInfo = "Co-Authored-By: Spettro <spettro@eyed.to>"

type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleSystem    Role = "system"
)

type ToolItem struct {
	Name   string
	Status string
	Args   string
	Output string
	Diff   string
	Open   bool
}

// maxLiveTools bounds how many completed ToolItem entries we retain in
// m.liveTools for a single run. The slice feeds compactRunSummary at
// interrupt time and is otherwise informational; older entries get dropped
// FIFO once the cap is hit so a runaway batch of tool calls cannot bloat
// memory or the run summary.
const maxLiveTools = 200

type ChatMessage struct {
	Role     Role
	Content  string
	Thinking string
	Meta     string
	Kind     string
	Tools    []ToolItem
	Images   []string
	At       time.Time
}

const localConnectProviderID = "__local_endpoint__"

type tickMsg time.Time

type agentDoneMsg struct {
	content    string
	meta       string
	tools      []agent.ToolTrace
	tokensUsed int
	err        error
}

type planDoneMsg struct {
	plan       string
	tools      []agent.ToolTrace
	tokensUsed int
	err        error
}

type commitDoneMsg struct {
	commitMsg string
	err       error
}

type searchDoneMsg struct {
	result string
	err    error
}

type bannerClearMsg struct{}
type quitWarningMsg struct{}

type compactDoneMsg struct {
	summary string
	err     error
}

type toolProgressMsg struct {
	trace agent.ToolTrace
}

type parallelAgentEntry struct {
	ID       string
	Label    string
	Kind     string
	Instance int
	Task     string
	Status   string
}

type modifiedFileEntry struct {
	Path      string
	Added     int
	Deleted   int
	Untracked bool
	Staged    bool
	Unstaged  bool
}

type sidePanelItem struct {
	Kind   string
	ID     string
	Title  string
	Detail string
	Body   string
	Agent  string
	Status string
}

type activityItem struct {
	Key     string
	Kind    string
	ID      string
	AgentID string
	Title   string
	Detail  string
	Body    string
	Status  string
	At      time.Time
}

type agentTickMsg struct{}

type shellApprovalRequestMsg struct {
	request  agent.ShellApprovalRequest
	response chan shellApprovalResponse
}

type shellApprovalResponse struct {
	decision agent.ShellApprovalDecision
	err      error
}

type askUserRequestMsg struct {
	request  agent.AskUserRequest
	response chan askUserResponse
}

type askUserResponse struct {
	answer string
	err    error
}

type queuedPrompt struct {
	Input          string
	Prompt         string
	MentionedFiles []string
	Images         []string
}

type attachmentItem struct {
	Kind    string // "file"
	Path    string // absolute path
	RelPath string // relative to cwd (shown in chip)
}

type setupState struct {
	step     int
	provider string
	model    string
}

type Model struct {
	width     int
	height    int
	ready     bool
	startedAt time.Time

	vp   viewport.Model
	ta   textarea.Model
	spin spinner.Model

	mode string
	cfg  config.UserConfig

	messages []ChatMessage

	inputHistory    []string
	historyIndex    int
	historyDraft    string
	historyBrowsing bool

	eyeFrame int
	thinking bool

	showSelector bool
	selItems     []provider.Model
	selFilter    string
	selCursor    int

	showConnect         bool
	connectItems        []provider.ProviderInfo
	connectFilter       string
	connectCursor       int
	connectStep         int
	connectProvider     string
	connectActionCursor int
	connectEditMode     bool

	cmdItems  []commandDef
	cmdCursor int

	repoFiles     []string
	mentionItems  []string
	mentionCursor int

	showSetup bool
	setup     setupState

	showOnboarding bool
	onboarding     onboardingState

	showLogin bool
	login     loginState

	favorites map[string]bool

	pendingPlan string

	banner     string
	bannerKind string

	ctrlCAt time.Time

	showTrust   bool
	trustCursor int

	showTools bool

	mouseCaptureOff bool

	liveTools        []ToolItem
	currentTool      *ToolItem
	toolCh           chan agent.ToolTrace
	approvalCh       chan shellApprovalRequestMsg
	askUserCh        chan askUserRequestMsg
	cancelAgent      context.CancelFunc
	pendingAuth      *shellApprovalRequestMsg
	pendingQuestion  *askUserRequestMsg
	approvalCursor   int
	questionCursor   int
	questionFreeform bool
	progressNote     string
	pendingPrompts   []queuedPrompt
	awaitingInstead  bool
	activePrompt     *queuedPrompt
	activeAgentID    string

	showPlanApproval   bool
	planApprovalCursor int

	parallelAgents   []parallelAgentEntry
	tickCount        int
	sideCursor       int
	sideScroll       int
	sideDetailScroll int
	modifiedFiles    []modifiedFileEntry
	gitBranch        string
	showSidePanel    bool
	sessionEdits     map[string]struct{}
	activityFeed     []activityItem
	currentRunKey    string
	recentApprovals  []session.AgentEvent

	totalTokensUsed     int
	autoCompactFailures int
	compactWarningLevel int
	autoCompactInFlight bool
	sessionID           string

	showResume   bool
	resumeItems  []session.Summary
	resumeCursor int
	resumeScroll int

	todos []session.Todo

	remoteServer        *remote.Server
	remoteRequestedPort int

	telegramRelay *telegram.Relay

	// startupCmds are tea.Cmds that need to fire on Init. Populated by
	// New() when initial state requires background work (e.g. autostarting
	// the Telegram relay) before the first tea event is processed.
	startupCmds []tea.Cmd

	// Attachments (ctrl+f to attach, ctrl+r to remove; ctrl+v to paste image)
	attachments      []attachmentItem
	showAttachPrompt bool
	attachDraft      string // textarea value saved while attach prompt is open
	clipboardTempDir string // temp dir for pasted images, created on first paste
	clipboardCounter int    // increments each paste for [Image #N] labelling

	// Desktop notifications
	agentStartAt    time.Time
	terminalFocused bool

	cwd       string
	store     *storage.Store
	providers *provider.Manager
	manifest  config.AgentManifest
	committer agent.CommitAgent
	searcher  agent.SearchAgent
}

func New(cwd string, cfg config.UserConfig, store *storage.Store, pm *provider.Manager) Model {
	ta := textarea.New()
	ta.Placeholder = "enter message…"
	ta.ShowLineNumbers = false
	ta.CharLimit = 8000
	ta.SetHeight(3)
	ta.FocusedStyle.CursorLine = lipgloss.NewStyle()
	ta.FocusedStyle.Prompt = lipgloss.NewStyle()
	ta.BlurredStyle.Prompt = lipgloss.NewStyle()
	ta.Focus()

	sp := spinner.New()
	sp.Spinner = spinner.Dot
	sp.Style = lipgloss.NewStyle().Foreground(colorMuted)

	favs := map[string]bool{}
	for _, f := range cfg.Favorites {
		favs[f] = true
	}

	manifest, _ := config.LoadAgentManifestForProject(cwd)
	defaultMode := manifest.DefaultAgent
	if defaultMode == "" {
		defaultMode = "plan"
	}
	if cfg.LastAgentID != "" {
		if spec, ok := manifest.AgentByID(cfg.LastAgentID); ok && spec.Enabled {
			defaultMode = cfg.LastAgentID
		}
	}

	m := Model{
		mode:          defaultMode,
		cfg:           cfg,
		cwd:           cwd,
		store:         store,
		providers:     pm,
		manifest:      manifest,
		ta:            ta,
		spin:          sp,
		favorites:     favs,
		showSidePanel: cfg.ShowSidePanel,
		startedAt:     time.Now(),
		committer: agent.LLMCommitter{
			ProviderManager: pm,
			ProviderName:    func() string { return cfg.ActiveProvider },
			ModelName:       func() string { return cfg.ActiveModel },
		},
		searcher:     agent.RepoSearcher{},
		historyIndex: -1,
	}
	m.refreshModifiedFiles()
	// Scan the working directory in the background: walking a large tree
	// synchronously here would block the first paint (seen: ~56s from $HOME).
	m.startupCmds = append(m.startupCmds, scanRepoFilesCmd(cwd))
	if cmd := m.autostartTelegram(); cmd != nil {
		m.startupCmds = append(m.startupCmds, cmd)
	}
	// If a Spettro Subscription is connected, register its endpoint immediately
	// (so inference resolves) and refresh the model list + plan in the background.
	hasSpettro := strings.TrimSpace(cfg.APIKeys[spettro.ProviderID]) != ""
	if hasSpettro {
		pm.SetSpettro(spettro.InferenceBaseURL(), nil)
		spettroKey := cfg.APIKeys[spettro.ProviderID]
		m.startupCmds = append(m.startupCmds, loadSpettroCmd(spettroKey, false, false))
	}
	if len(pm.ConnectedModels(cfg.APIKeys)) == 0 && len(cfg.LocalEndpoints) == 0 && !hasSpettro {
		m.showOnboarding = true
		m.onboarding = onboardingState{
			step:  0,
			items: m.allOnboardingModels(""),
		}
	}
	return m
}

func (m Model) currentAgent() (config.AgentSpec, bool) {
	return m.manifest.AgentByID(m.mode)
}

func (m Model) currentColor() lipgloss.Color {
	if spec, ok := m.manifest.AgentByID(m.mode); ok {
		return modeColor(spec.Color)
	}
	return modeColor(m.mode)
}

func (m Model) Init() tea.Cmd {
	cmds := []tea.Cmd{textarea.Blink, tick(), m.spin.Tick}
	cmds = append(cmds, m.startupCmds...)
	return tea.Batch(cmds...)
}

func tick() tea.Cmd {
	return tea.Tick(50*time.Millisecond, func(t time.Time) tea.Msg { return tickMsg(t) })
}

var spinnerFrames = []string{"⣾", "⣽", "⣻", "⢿", "⡿", "⣟", "⣯", "⣷"}

func agentTickCmd() tea.Cmd {
	return tea.Tick(500*time.Millisecond, func(time.Time) tea.Msg { return agentTickMsg{} })
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	newModel, cmd := m.update(msg)
	if nm, ok := newModel.(Model); ok {
		nm = nm.recalcLayout()
		return nm, cmd
	}
	return newModel, cmd
}

// resetRunState clears every per-run field when an agent or plan run ends, so
// no channel, cursor, live-tool, or progress state leaks into the next run.
// Both the agentDoneMsg and planDoneMsg handlers begin with this identical
// teardown; keeping it in one place means a new per-run field only has to be
// reset once.
func (m *Model) resetRunState() {
	m.thinking = false
	m.cancelAgent = nil
	m.toolCh = nil
	m.approvalCh = nil
	m.askUserCh = nil
	m.liveTools = nil
	m.currentTool = nil
	m.pendingAuth = nil
	m.pendingQuestion = nil
	m.questionCursor = 0
	m.questionFreeform = false
	m.parallelAgents = nil
	m.progressNote = ""
	m.activePrompt = nil
	m.activeAgentID = ""
	m.refreshModifiedFiles()
}

func (m Model) update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m = m.recalcLayout()
		if !m.ready {
			m.ready = true
			if !config.IsTrusted(m.cwd) {
				m.showTrust = true
			} else {
				msg := "spettro ready — /help for commands, shift+tab to switch mode"
				m.pushSystemMsg(msg)
			}
			m.refreshViewport()
		}
	case tickMsg:
		m.eyeFrame++
		cmds = append(cmds, tick())
	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spin, cmd = m.spin.Update(msg)
		cmds = append(cmds, cmd)
	case agentDoneMsg:
		if !m.thinking {
			break
		}
		m.resetRunState()
		if msg.tokensUsed > 0 {
			m.totalTokensUsed += msg.tokensUsed
			m.updateCompactWarningState()
		}
		if msg.err != nil {
			m.finishAgentActivity(m.mode, "failed", msg.err.Error(), "")
			m.showBanner("error: "+msg.err.Error(), "error")
			m.publishRemote("assistant_error", map[string]interface{}{"error": msg.err.Error()})
		} else {
			m.syncTodosFromSession()
			main, thinking := stripThinking(msg.content)
			m.messages = append(m.messages, ChatMessage{
				Role:     RoleAssistant,
				Content:  main,
				Thinking: thinking,
				Meta:     msg.meta,
				Tools:    toToolItems(msg.tools),
				At:       time.Now(),
			})
			m.finishAgentActivity(m.mode, "done", main, thinking)
			m.publishRemote("assistant_message", map[string]interface{}{
				"content":     main,
				"thinking":    thinking,
				"meta":        msg.meta,
				"tools_count": len(msg.tools),
				"tokens_used": msg.tokensUsed,
			})
		}
		m.publishRemoteState("agent_done")
		m.maybeNotify(msg.err)
		m.refreshViewport()
		if cmd := m.autoCompactIfNeeded(); cmd != nil {
			cmds = append(cmds, cmd)
		} else if _, nextCmd := m.maybeRunNextQueuedPrompt(); nextCmd != nil {
			cmds = append(cmds, nextCmd)
		}
	case planDoneMsg:
		if !m.thinking {
			break
		}
		m.resetRunState()
		if msg.tokensUsed > 0 {
			m.totalTokensUsed += msg.tokensUsed
			m.updateCompactWarningState()
		}
		if msg.err != nil {
			m.finishAgentActivity(m.mode, "failed", msg.err.Error(), "")
			m.showBanner("plan error: "+msg.err.Error(), "error")
			m.publishRemote("plan_error", map[string]interface{}{"error": msg.err.Error()})
		} else {
			m.syncTodosFromSession()
			m.pendingPlan = msg.plan
			m.messages = append(m.messages, ChatMessage{
				Role:    RoleAssistant,
				Kind:    "plan",
				Content: msg.plan,
				Tools:   toToolItems(msg.tools),
				At:      time.Now(),
			})
			m.finishAgentActivity(m.mode, "done", msg.plan, "")
			m.showPlanApproval = true
			m.planApprovalCursor = 0
			m.publishRemote("plan", map[string]interface{}{
				"plan":        msg.plan,
				"tools_count": len(msg.tools),
				"tokens_used": msg.tokensUsed,
			})
		}
		m.publishRemoteState("plan_done")
		m.maybeNotify(msg.err)
		m.refreshViewport()
		if cmd := m.autoCompactIfNeeded(); cmd != nil {
			cmds = append(cmds, cmd)
		}
	case commitDoneMsg:
		if !m.thinking {
			break
		}
		m.thinking = false
		m.cancelAgent = nil
		m.refreshModifiedFiles()
		if msg.err != nil {
			m.showBanner("commit error: "+msg.err.Error(), "error")
			m.publishRemote("commit_error", map[string]interface{}{"error": msg.err.Error()})
		} else {
			m.messages = append(m.messages, ChatMessage{
				Role:    RoleSystem,
				Content: fmt.Sprintf("committed: %s\n\n%s", msg.commitMsg, coAuthorInfo),
				At:      time.Now(),
			})
			m.publishRemote("commit", map[string]interface{}{"message": msg.commitMsg})
		}
		m.publishRemoteState("commit_done")
		m.refreshViewport()
	case searchDoneMsg:
		if !m.thinking {
			break
		}
		m.thinking = false
		m.cancelAgent = nil
		if msg.err != nil {
			m.showBanner("search error: "+msg.err.Error(), "error")
			m.publishRemote("search_error", map[string]interface{}{"error": msg.err.Error()})
		} else {
			m.messages = append(m.messages, ChatMessage{
				Role:    RoleSystem,
				Content: msg.result,
				At:      time.Now(),
			})
			m.publishRemote("search", map[string]interface{}{"result": msg.result})
		}
		m.publishRemoteState("search_done")
		m.refreshViewport()
	case compactDoneMsg:
		if !m.thinking {
			break
		}
		m.thinking = false
		m.cancelAgent = nil
		wasAutoCompact := m.autoCompactInFlight
		m.autoCompactInFlight = false
		if msg.err != nil {
			if wasAutoCompact {
				m.autoCompactFailures++
			}
			m.showBanner("compact error: "+msg.err.Error(), "error")
		} else {
			m.autoCompactFailures = 0
			m.autoSave()
			m.sessionID = ""
			m.todos = nil
			m.totalTokensUsed = 0
			m.compactWarningLevel = 0
			m.messages = []ChatMessage{{
				Role:    RoleSystem,
				Content: "── conversation compacted ──\n\n" + msg.summary,
				At:      time.Now(),
			}}
		}
		m.publishRemoteState("compact_done")
		m.refreshViewport()
	case agentTickMsg:
		m.tickCount++
		for _, a := range m.parallelAgents {
			if a.Status == "running" {
				cmds = append(cmds, agentTickCmd())
				break
			}
		}
		m.vp.SetContent(m.renderMessages())
	case toolProgressMsg:
		if m.thinking {
			t := msg.trace
			m.applyToolTraceToObservability(t)
			m.publishRemoteToolTrace(t)
			// When an agent finishes generating an image/video, push the
			// produced files into every bound Telegram chat. The
			// dispatcher is a no-op when the relay is offline or nobody
			// is subscribed, so it stays cheap on the hot path.
			m.dispatchTelegramMedia(t)
			if t.Name == "comment" {
				if t.Status == "success" {
					if message := extractCommentMessage(t.Args, t.Output); message != "" {
						m.setProgressNote(message)
					}
				}
				if m.toolCh != nil {
					cmds = append(cmds, waitForTool(m.toolCh))
				}
				m.vp.SetContent(m.renderMessages())
				m.vp.GotoBottom()
				break
			}
			if t.Name == "todo-write" && t.Status != "running" {
				m.syncTodosFromSession()
			}
			m.trackSessionEditFromTrace(t)
			if t.Status != "running" {
				switch t.Name {
				case "file-write", "shell-exec", "bash", "agent":
					m.refreshModifiedFiles()
				}
			}
			if t.Status == "running" {
				item := ToolItem{Name: t.Name, Args: t.Args, Status: "running"}
				m.currentTool = &item
				m.appendToolStreamMessage(item)
			} else {
				completed := ToolItem{
					Name:   t.Name,
					Status: t.Status,
					Args:   t.Args,
					Output: t.Output,
					Diff:   computeFileDiff(m.cwd, t.Name, t.Args, t.Status),
				}
				// Cap m.liveTools to bound memory and the run summary built
				// at interrupt time. When the LLM emits very large tool
				// batches we keep the most recent maxLiveTools entries so
				// the most useful context (what just happened) survives.
				m.liveTools = append(m.liveTools, completed)
				if len(m.liveTools) > maxLiveTools {
					m.liveTools = append([]ToolItem(nil), m.liveTools[len(m.liveTools)-maxLiveTools:]...)
				}
				m.currentTool = nil
				m.updateToolStreamMessage(completed)
			}
			if m.toolCh != nil {
				cmds = append(cmds, waitForTool(m.toolCh))
			}
			m.vp.SetContent(m.renderMessages())
			m.vp.GotoBottom()
		}
	case shellApprovalRequestMsg:
		if m.thinking {
			m.pendingAuth = &msg
			m.approvalCursor = 0
			m.ta.Reset()
			m.showBanner("command approval required", "warn")
			m.publishRemote("approval_request", map[string]interface{}{
				"command":  msg.request.Command,
				"tool_id":  msg.request.ToolID,
				"segments": msg.request.Segments,
				"reason":   msg.request.Reason,
			})
			if m.approvalCh != nil {
				cmds = append(cmds, waitForShellApproval(m.approvalCh))
			}
			m.refreshViewport()
		}
	case askUserRequestMsg:
		if m.thinking {
			m.pendingQuestion = &msg
			m.questionCursor = askUserDefaultCursor(msg.request)
			m.questionFreeform = len(msg.request.Options) == 0
			m.ta.Reset()
			m.showBanner("agent is waiting for your answer", "info")
			m.publishRemote("ask_user", map[string]interface{}{
				"question":            msg.request.Question,
				"options":             msg.request.Options,
				"context":             msg.request.Context,
				"default":             msg.request.DefaultOption,
				"allow_free_response": msg.request.AllowFreeResponse,
			})
			if m.askUserCh != nil {
				cmds = append(cmds, waitForAskUser(m.askUserCh))
			}
			m.refreshViewport()
		}
	case pasteImageMsg:
		if msg.err != nil {
			m.clipboardCounter-- // keep numbering gap-free on failure
			m.showBanner("paste image: "+msg.err.Error(), "error")
			return m, tea.Batch(cmds...)
		}
		m.attachments = append(m.attachments, attachmentItem{
			Kind:    "image",
			Path:    msg.path,
			RelPath: fmt.Sprintf("Image #%d", msg.counter),
		})
		m.showBanner(fmt.Sprintf("pasted Image #%d", msg.counter), "success")
		m.refreshViewport()
		return m, tea.Batch(cmds...)
	case verifyKeyDoneMsg:
		newModel, cmd := m.handleVerifyKeyDone(msg)
		return newModel, cmd
	case loginInitiatedMsg:
		return m.handleLoginInitiated(msg)
	case loginPolledMsg:
		return m.handleLoginPolled(msg)
	case spettroLoadedMsg:
		return m.handleSpettroLoaded(msg)
	case repoFilesScannedMsg:
		m.repoFiles = msg.files
		m.syncInputSuggestions()
	case tea.FocusMsg:
		m.terminalFocused = true
	case tea.BlurMsg:
		m.terminalFocused = false
	case bannerClearMsg:
		m.banner = ""
		m.bannerKind = ""
	case remoteSubmitMsg:
		newModel, cmd := m.handleRemoteSubmission(msg.req)
		nm, _ := newModel.(Model)
		if cmd != nil {
			cmds = append(cmds, cmd)
		}
		if nm.remoteServer != nil {
			cmds = append(cmds, waitForRemoteSubmit(nm.remoteServer))
		}
		return nm, tea.Batch(cmds...)
	case remoteInterruptMsg:
		if m.thinking {
			m.interruptRun("Interrupted by remote client.", false)
			m.publishRemote("remote_interrupt", map[string]interface{}{"thinking": true})
			m.refreshViewport()
		} else {
			m.publishRemote("remote_interrupt", map[string]interface{}{"thinking": false})
		}
		if m.remoteServer != nil {
			cmds = append(cmds, waitForRemoteInterrupt(m.remoteServer))
		}
	case telegramSubmitMsg:
		newModel, cmd := m.handleTelegramSubmission(msg.req)
		nm, _ := newModel.(Model)
		if cmd != nil {
			cmds = append(cmds, cmd)
		}
		if nm.telegramRelay != nil {
			cmds = append(cmds, waitForTelegramSubmit(nm.telegramRelay))
		}
		return nm, tea.Batch(cmds...)
	case telegramInterruptMsg:
		if m.thinking {
			m.interruptRun("Interrupted via Telegram.", false)
			m.publishRemote("telegram_interrupt", map[string]interface{}{"thinking": true})
			m.refreshViewport()
		} else {
			m.publishRemote("telegram_interrupt", map[string]interface{}{"thinking": false})
		}
		if m.telegramRelay != nil {
			cmds = append(cmds, waitForTelegramInterrupt(m.telegramRelay))
		}
	case quitWarningMsg:
		if m.banner == "press again ctrl C to exit" {
			m.banner = ""
			m.bannerKind = ""
			m.ctrlCAt = time.Time{}
		}
	case tea.MouseMsg:
		if m.mouseCaptureOff {
			return m, tea.Batch(cmds...)
		}
		if m.showResume {
			switch msg.Button {
			case tea.MouseButtonWheelUp:
				if m.resumeCursor > 0 {
					m.resumeCursor--
				}
				m.ensureResumeWindow()
				return m, tea.Batch(cmds...)
			case tea.MouseButtonWheelDown:
				if m.resumeCursor < len(m.resumeItems)-1 {
					m.resumeCursor++
				}
				m.ensureResumeWindow()
				return m, tea.Batch(cmds...)
			}
		}
		sideW := m.sidePanelWidth()
		onSidePanel := sideW > 0 && msg.X >= m.paneWidth()+1
		if onSidePanel {
			items := m.sidePanelItems()
			innerHeight := m.sidePanelInnerHeight()
			_, gitRows := m.sidePanelGitSummary(sideW)
			_, _, rows := m.sidePanelWindow(items, innerHeight, gitRows)
			maxStart := max(0, len(items)-rows)
			switch msg.Button {
			case tea.MouseButtonWheelUp:
				if m.sideDetailScroll > 0 {
					m.sideDetailScroll--
					return m, tea.Batch(cmds...)
				}
				if m.sideScroll > 0 {
					m.sideScroll--
				}
				if m.sideCursor > 0 {
					m.sideCursor--
					m.sideDetailScroll = 0
				}
				return m, tea.Batch(cmds...)
			case tea.MouseButtonWheelDown:
				detailMax := m.sidePanelDetailMaxScroll(sideW)
				if m.sideDetailScroll < detailMax {
					m.sideDetailScroll++
					return m, tea.Batch(cmds...)
				}
				if m.sideScroll < maxStart {
					m.sideScroll++
				}
				if m.sideCursor < len(items)-1 {
					m.sideCursor++
					m.sideDetailScroll = 0
				}
				return m, tea.Batch(cmds...)
			case tea.MouseButtonLeft:
				startY, _ := m.sideListGeometry()
				row := msg.Y - startY
				if row >= 0 {
					cursor, start, rows := m.sidePanelWindow(items, innerHeight, gitRows)
					_, rowToItem := m.sidePanelLines(items, sideW, cursor, start, rows)
					if row >= 0 && row < len(rowToItem) {
						idx := rowToItem[row]
						if idx >= 0 && idx < len(items) {
							if m.sideCursor != idx {
								m.sideDetailScroll = 0
							}
							m.sideCursor = idx
						}
					}
					if len(rowToItem) == 0 {
						idx := m.sideScroll + row
						if idx >= 0 && idx < len(items) {
							if m.sideCursor != idx {
								m.sideDetailScroll = 0
							}
							m.sideCursor = idx
						}
					}
				}
				return m, tea.Batch(cmds...)
			}
		}
		switch msg.Button {
		case tea.MouseButtonWheelUp:
			switch {
			case m.showOnboarding && m.onboarding.step == 0:
				if m.onboarding.cursor > 0 {
					m.onboarding.cursor--
				}
			case m.showSelector:
				if m.selCursor > 0 {
					m.selCursor--
				}
			case m.showConnect:
				if m.connectCursor > 0 {
					m.connectCursor--
				}
			default:
				m.vp.LineUp(3)
			}
		case tea.MouseButtonWheelDown:
			switch {
			case m.showOnboarding && m.onboarding.step == 0:
				if m.onboarding.cursor < len(m.onboarding.items)-1 {
					m.onboarding.cursor++
				}
			case m.showSelector:
				if m.selCursor < len(m.selItems)-1 {
					m.selCursor++
				}
			case m.showConnect:
				if m.connectCursor < len(m.connectItems)-1 {
					m.connectCursor++
				}
			default:
				m.vp.LineDown(3)
			}
		}
		return m, tea.Batch(cmds...)
	case tea.KeyMsg:
		if msg.String() == "ctrl+t" {
			m.mouseCaptureOff = !m.mouseCaptureOff
			var mouseCmd tea.Cmd
			if m.mouseCaptureOff {
				mouseCmd = tea.DisableMouse
				m.showBanner("text-select mode — mouse off, ctrl+t to re-enable", "info")
			} else {
				mouseCmd = tea.EnableMouseCellMotion
				m.showBanner("mouse on — scroll wheel and side panel clicks active", "info")
			}
			return m, tea.Batch(append(cmds, mouseCmd)...)
		}
		if m.showLogin {
			return m.updateLogin(msg)
		}
		if m.showTrust {
			return m.updateTrust(msg)
		}
		if m.showResume {
			return m.updateResume(msg)
		}
		if m.showConnect {
			return m.updateConnect(msg)
		}
		if m.showSelector {
			return m.updateSelector(msg)
		}
		if m.showSetup {
			return m.updateSetup(msg)
		}
		if m.showOnboarding {
			return m.updateOnboarding(msg)
		}
		return m.updateMain(msg)
	}

	if !m.showLogin && !m.showTrust && !m.showResume && !m.showSelector && !m.showSetup && !m.showConnect {
		var taCmd tea.Cmd
		m.ta, taCmd = m.ta.Update(msg)
		cmds = append(cmds, taCmd)
		m.syncInputSuggestions()

		var vpCmd tea.Cmd
		m.vp, vpCmd = m.vp.Update(msg)
		cmds = append(cmds, vpCmd)
	}

	return m, tea.Batch(cmds...)
}

func (m Model) updateMain(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.showPlanApproval {
		return m.updatePlanApproval(msg)
	}
	if m.pendingAuth != nil {
		return m.updateShellApproval(msg)
	}
	if m.pendingQuestion != nil {
		return m.updateAskUserQuestion(msg)
	}

	switch msg.String() {
	case "ctrl+c":
		if !m.ctrlCAt.IsZero() && time.Since(m.ctrlCAt) < 5*time.Second {
			return m, tea.Quit
		}
		m.ctrlCAt = time.Now()
		m.showBanner("press again ctrl C to exit", "warn")
		return m, tea.Tick(5*time.Second, func(time.Time) tea.Msg { return quitWarningMsg{} })
	case "ctrl+q":
		return m, tea.Quit
	case "up":
		if len(m.cmdItems) > 0 || len(m.mentionItems) > 0 {
			if m.cmdCursor > 0 {
				m.cmdCursor--
			}
			if m.mentionCursor > 0 {
				m.mentionCursor--
			}
			return m, nil
		}
		if m.recallPreviousInput() {
			m.syncInputSuggestions()
			return m, nil
		}
	case "ctrl+y":
		for i := len(m.messages) - 1; i >= 0; i-- {
			if m.messages[i].Role == RoleAssistant {
				if err := clipboard.WriteAll(m.messages[i].Content); err != nil {
					m.showBanner("clipboard error: "+err.Error(), "error")
				} else {
					m.showBanner("last response copied to clipboard", "success")
				}
				return m, nil
			}
		}
		m.showBanner("no response to copy yet", "info")
		return m, nil
	case "ctrl+v":
		if m.showAttachPrompt {
			break // let textarea handle text paste in attach mode
		}
		if !m.providers.SupportsVision(m.cfg.ActiveProvider, m.cfg.ActiveModel) {
			m.showBanner("current model does not support vision", "error")
			return m, nil
		}
		if err := m.ensureClipboardTempDir(); err != nil {
			m.showBanner("clipboard temp dir: "+err.Error(), "error")
			return m, nil
		}
		m.clipboardCounter++
		return m, readClipboardImageCmd(m.clipboardTempDir, m.clipboardCounter)
	case "ctrl+f":
		if m.showAttachPrompt {
			m.ta.Reset()
			m.ta.SetValue(m.attachDraft)
			m.ta.Placeholder = "enter message…"
			m.showAttachPrompt = false
			return m, nil
		}
		m.attachDraft = m.ta.Value()
		m.ta.Reset()
		m.ta.Placeholder = "file path to attach…"
		m.showAttachPrompt = true
		m.showBanner("enter file path and press enter (esc cancels)", "info")
		return m, nil
	case "ctrl+r":
		if m.showAttachPrompt {
			m.ta.Reset()
			m.ta.SetValue(m.attachDraft)
			m.ta.Placeholder = "enter message…"
			m.showAttachPrompt = false
			return m, nil
		}
		if len(m.attachments) > 0 {
			removed := m.attachments[len(m.attachments)-1]
			m.attachments = m.attachments[:len(m.attachments)-1]
			m.showBanner("removed attachment: "+removed.RelPath, "info")
		} else {
			m.showBanner("no attachments to remove", "info")
		}
		return m, nil
	case "down", "ctrl+n":
		if len(m.cmdItems) > 0 || len(m.mentionItems) > 0 {
			if m.cmdCursor < len(m.cmdItems)-1 {
				m.cmdCursor++
			}
			if m.mentionCursor < len(m.mentionItems)-1 {
				m.mentionCursor++
			}
			return m, nil
		}
		if m.recallNextInput() {
			m.syncInputSuggestions()
			return m, nil
		}
	case "tab":
		if len(m.cmdItems) > 0 {
			m.cmdCursor = (m.cmdCursor + 1) % len(m.cmdItems)
			return m, nil
		}
		if len(m.mentionItems) > 0 {
			m.mentionCursor = (m.mentionCursor + 1) % len(m.mentionItems)
			return m, nil
		}
	case "shift+tab":
		m.mode = nextAgent(m.manifest, m.mode)
		m.persistUIState()
		m.showBanner(fmt.Sprintf("switched to %s mode", m.mode), "info")
		m.publishRemoteState("mode_change")
		return m, nil
	case "ctrl+o":
		m.showTools = !m.showTools
		m.sideDetailScroll = 0
		m.refreshViewport()
		return m, nil
	case "ctrl+b":
		m.showSidePanel = !m.showSidePanel
		m.persistUIState()
		m.refreshModifiedFiles()
		m.refreshViewport()
		if m.showSidePanel {
			m.showBanner("activity panel enabled", "info")
		} else {
			m.showBanner("activity panel hidden", "info")
		}
		return m, nil
	case "f2":
		models := m.favoriteModels()
		if len(models) > 0 {
			current := -1
			for i, mod := range models {
				if mod.Provider == m.cfg.ActiveProvider && mod.Name == m.cfg.ActiveModel {
					current = i
					break
				}
			}
			next := models[(current+1)%len(models)]
			_ = m.updateConfig(func(cfg *config.UserConfig) error {
				cfg.ActiveProvider = next.Provider
				cfg.ActiveModel = next.Name
				return nil
			})
			m.showBanner(fmt.Sprintf("model → %s:%s", next.Provider, next.Name), "success")
		} else {
			m.showBanner("no favorite models — mark one with f in /models", "info")
		}
		return m, nil
	case "shift+f2":
		models := m.favoriteModels()
		if len(models) > 0 {
			current := -1
			for i, mod := range models {
				if mod.Provider == m.cfg.ActiveProvider && mod.Name == m.cfg.ActiveModel {
					current = i
					break
				}
			}
			prev := (current - 1 + len(models)) % len(models)
			_ = m.updateConfig(func(cfg *config.UserConfig) error {
				cfg.ActiveProvider = models[prev].Provider
				cfg.ActiveModel = models[prev].Name
				return nil
			})
			m.showBanner(fmt.Sprintf("model → %s:%s", models[prev].Provider, models[prev].Name), "success")
		} else {
			m.showBanner("no favorite models — mark one with f in /models", "info")
		}
		return m, nil
	case "enter":
		if m.showAttachPrompt {
			path := strings.TrimSpace(m.ta.Value())
			m.ta.Reset()
			m.ta.SetValue(m.attachDraft)
			m.ta.Placeholder = "enter message…"
			m.showAttachPrompt = false
			if path != "" {
				m.addAttachment(path)
			}
			return m, nil
		}
		if len(m.cmdItems) > 0 {
			chosen := m.cmdItems[m.cmdCursor].name
			// Sub-commands (e.g. "/think high") execute immediately when selected.
			if strings.Contains(chosen[1:], " ") {
				m.ta.Reset()
				m.cmdItems = nil
				m.cmdCursor = 0
				m.mentionItems = nil
				if m.thinking && !isInstantCommand(chosen) {
					m.showBanner("commands cannot be queued while an agent is running", "warn")
					return m, nil
				}
				return m.handleCommand(chosen)
			}
			// Commands that require a parameter always open the second-level selector.
			if requiresParam(chosen) {
				m.ta.SetValue(chosen + " ")
				m.cmdItems = nil
				m.cmdCursor = 0
				m.syncInputSuggestions()
				return m, nil
			}
			// Other commands: execute if textarea already matches, else complete.
			current := strings.TrimSpace(m.ta.Value())
			if current == chosen {
				m.ta.Reset()
				m.cmdItems = nil
				m.cmdCursor = 0
				m.mentionItems = nil
				if m.thinking && !isInstantCommand(chosen) {
					m.showBanner("commands cannot be queued while an agent is running", "warn")
					return m, nil
				}
				return m.handleCommand(chosen)
			}
			m.ta.SetValue(chosen + " ")
			m.cmdItems = nil
			m.cmdCursor = 0
			m.syncInputSuggestions()
			return m, nil
		}
		if len(m.mentionItems) > 0 {
			m = m.acceptMention()
			m.syncInputSuggestions()
			return m, nil
		}
		input := strings.TrimSpace(m.ta.Value())
		if input == "" {
			return m, nil
		}
		isCmd := strings.HasPrefix(input, "/")
		instant := isCmd && isInstantCommand(input)
		if !instant {
			m.pushInputHistory(input)
		}
		m.ta.Reset()
		m.cmdItems = nil
		m.mentionItems = nil
		if m.pendingPlan != "" && !isCmd {
			return m.handlePlanEdit(input)
		}
		if isCmd {
			if m.thinking && !instant {
				m.showBanner("commands cannot be queued while an agent is running", "warn")
				return m, nil
			}
			return m.handleCommand(input)
		}
		return m.handlePrompt(input)
	case "esc":
		if m.showAttachPrompt {
			m.ta.Reset()
			m.ta.SetValue(m.attachDraft)
			m.ta.Placeholder = "enter message…"
			m.showAttachPrompt = false
			return m, nil
		}
		if m.thinking {
			m.interruptRun("Stopped by user.", true)
			m.refreshViewport()
			return m, nil
		}
		if len(m.cmdItems) > 0 {
			m.ta.Reset()
			m.cmdItems = nil
			m.cmdCursor = 0
			return m, nil
		}
		if len(m.mentionItems) > 0 {
			m.mentionItems = nil
			m.mentionCursor = 0
			m.syncInputSuggestions()
			return m, nil
		}
		m.ta.Reset()
		m.banner = ""
		return m, nil
	}

	var taCmd tea.Cmd
	m.ta, taCmd = m.ta.Update(msg)
	m.syncInputSuggestions()
	return m, taCmd
}

func (m Model) handleCommand(input string) (tea.Model, tea.Cmd) {
	fields := strings.Fields(input)
	cmd := fields[0]
	m.recordCommandEvent(input)

	switch cmd {
	case "/help":
		m.pushSystemMsg(helpText)
	case "/exit", "/quit":
		return m, tea.Quit
	case "/mode", "/next":
		m.mode = nextAgent(m.manifest, m.mode)
		m.persistUIState()
		m.showBanner(fmt.Sprintf("switched to %s mode", m.mode), "info")
		m.publishRemoteState("mode_change")
	case "/login":
		return m.startLogin(false)
	case "/logout":
		return m.handleLogout()
	case "/connect":
		m = m.openConnect()
	case "/models":
		if len(fields) >= 2 {
			if strings.Contains(fields[1], ":") {
				parts := strings.SplitN(fields[1], ":", 2)
				if !m.providers.HasModel(parts[0], parts[1]) {
					m.showBanner("unknown model: "+fields[1], "error")
				} else {
					if len(fields) >= 3 {
						if err := config.SaveAPIKey(parts[0], fields[2]); err != nil {
							m.showBanner("failed to save API key: "+err.Error(), "error")
							return m, nil
						}
					}
					_ = m.updateConfig(func(cfg *config.UserConfig) error {
						cfg.ActiveProvider = parts[0]
						cfg.ActiveModel = parts[1]
						return nil
					})
					m.showBanner(fmt.Sprintf("model set to %s:%s", parts[0], parts[1]), "success")
				}
			} else {
				m = m.openSelector(fields[1])
			}
		} else {
			m = m.openSelector("")
		}
	case "/permission":
		if len(fields) < 2 {
			m.showBanner("usage: /permission <yolo|restricted|ask-first>", "info")
		} else {
			level := config.PermissionLevel(fields[1])
			switch level {
			case config.PermissionYOLO, config.PermissionRestricted, config.PermissionAskFirst:
				_ = m.updateConfig(func(cfg *config.UserConfig) error {
					cfg.Permission = level
					return nil
				})
				m.showBanner(fmt.Sprintf("permission set to %s", level), "success")
			default:
				m.showBanner("invalid permission: use yolo, restricted, or ask-first", "error")
			}
		}
	case "/budget":
		if len(fields) < 2 {
			if m.cfg.TokenBudget <= 0 {
				m.showBanner("token budget: unlimited  usage: /budget <n|0>", "info")
			} else {
				m.showBanner(fmt.Sprintf("token budget: %d  usage: /budget <n|0>", m.cfg.TokenBudget), "info")
			}
		} else {
			var n int
			if _, err := fmt.Sscanf(fields[1], "%d", &n); err != nil || n < 0 {
				m.showBanner("usage: /budget <n|0>", "error")
			} else {
				_ = m.updateConfig(func(cfg *config.UserConfig) error {
					cfg.TokenBudget = n
					return nil
				})
				if n == 0 {
					m.showBanner("token budget set to unlimited", "success")
				} else {
					m.showBanner(fmt.Sprintf("token budget set to %d", n), "success")
				}
			}
		}
	case "/thinking", "/think":
		// /think [off|low|medium|high|x-high|max] toggles the extended-thinking
		// budget passed to providers that support it (Anthropic Claude Opus
		// and Sonnet). Without an argument we report the current setting.
		current := strings.TrimSpace(m.cfg.ThinkingLevel)
		if current == "" {
			current = "off"
		}
		if len(fields) < 2 {
			m.showBanner("thinking: "+current+"  usage: /think <off|low|medium|high|x-high|max>", "info")
		} else {
			level := strings.ToLower(strings.TrimSpace(fields[1]))
			if !provider.IsValidThinkingLevel(level) {
				m.showBanner("usage: /think <off|low|medium|high|x-high|max>", "error")
			} else {
				if level == "off" {
					level = ""
				}
				_ = m.updateConfig(func(cfg *config.UserConfig) error {
					cfg.ThinkingLevel = level
					return nil
				})
				display := level
				if display == "" {
					display = "off"
				}
				m.showBanner("thinking level set to "+display, "success")
			}
		}
	case "/approve":
		if m.pendingPlan == "" {
			m.showBanner("no pending plan — run a plan prompt first", "info")
		} else {
			spec, ok := m.manifest.AgentByID("coding")
			if !ok {
				m.showBanner("coding agent not found in manifest", "error")
			} else {
				plan := m.pendingPlan
				m.pendingPlan = ""
				return m.runAgentApproved(spec, plan, nil, nil, true)
			}
		}
	case "/init":
		return m.runInit()
	case "/compact":
		return m.handleCompactCommand(input)
	case "/clear":
		m.autoSave()
		m.messages = nil
		m.sessionID = ""
		m.todos = nil
		m.pushSystemMsg("conversation cleared")
		m.refreshViewport()
	case "/tasks":
		return m.handleTasksCommand(input)
	case "/mcp":
		return m.handleMCPCommand(input)
	case "/skills", "/skill":
		return m.handleSkillsCommand(input)
	case "/hooks":
		return m.handleHooksCommand()
	case "/plan":
		return m.handlePlanCommand(input)
	case "/permissions":
		return m.handlePermissionsCommand(input)
	case "/resume":
		items, err := session.List(m.store.GlobalDir, m.cwd)
		if err != nil || len(items) == 0 {
			m.showBanner("no saved conversations found", "info")
		} else {
			m.showResume = true
			m.resumeItems = items
			m.resumeCursor = 0
			m.resumeScroll = 0
		}
	case "/remote":
		return m.handleRemoteCommand(input)
	case "/telegram", "/tg":
		return m.handleTelegramCommand(input)
	default:
		m.showBanner("unknown command: "+cmd, "error")
	}

	m.refreshViewport()
	return m, nil
}

func (m Model) handlePrompt(input string) (tea.Model, tea.Cmd) {
	window := m.contextWindow()
	if window == 0 {
		window = contextWindowDefault(m.cfg.ActiveProvider)
	}
	eval := compact.Evaluate(window, compact.Config{
		AutoEnabled:      m.cfg.AutoCompactEnabled,
		AutoThresholdPct: m.cfg.AutoCompactThresholdPct,
		MaxFailures:      m.cfg.AutoCompactMaxFailures,
	}, compact.State{
		TokensUsed:          m.totalTokensUsed,
		ConsecutiveFailures: m.autoCompactFailures,
	})
	if eval.IsBlocking {
		m.showBanner("context limit reached; run /compact before sending new prompts", "error")
		return m, nil
	}
	mentionedFiles := m.extractMentionedFiles(input)
	prompt := injectMentionGuidance(input, mentionedFiles)
	prompt = m.injectAttachments(prompt)
	// Collect image paths (Kind="image") to send via the vision channel.
	var imagePaths []string
	for _, att := range m.attachments {
		if att.Kind == "image" {
			imagePaths = append(imagePaths, att.Path)
		}
	}
	sentAttachments := append([]attachmentItem(nil), m.attachments...)
	m.attachments = nil
	if m.thinking {
		m.queuePrompt(input, prompt, mentionedFiles, imagePaths)
		m.pushSystemMsg(fmt.Sprintf("queued request: %s", truncateLabel(input, 140)))
		if len(sentAttachments) > 0 {
			m.pushSystemMsg(fmt.Sprintf("(with %d attachment(s))", len(sentAttachments)))
		}
		m.showBanner("request queued for when the current run finishes", "info")
		m.refreshViewport()
		return m, nil
	}
	return m.startPromptRun(queuedPrompt{
		Input:          input,
		Prompt:         prompt,
		MentionedFiles: mentionedFiles,
		Images:         imagePaths,
	})
}

func (m Model) startPromptRun(req queuedPrompt) (tea.Model, tea.Cmd) {
	m.parallelAgents = nil
	m.ensureSession()
	m.messages = append(m.messages, ChatMessage{
		Role:    RoleUser,
		Content: req.Input,
		Images:  req.Images,
		At:      time.Now(),
	})
	m.awaitingInstead = false
	m.publishRemote("user_message", map[string]interface{}{
		"content":         req.Input,
		"mentioned_files": req.MentionedFiles,
	})
	m.publishRemoteState("user_message")
	m.refreshViewport()

	spec, ok := m.manifest.AgentByID(m.mode)
	if !ok {
		m.showBanner("unknown agent: "+m.mode, "error")
		return m, nil
	}
	return m.runAgent(spec, req.Prompt, req.MentionedFiles, req.Images)
}

func (m Model) maybeRunNextQueuedPrompt() (tea.Model, tea.Cmd) {
	if m.thinking || m.awaitingInstead {
		return m, nil
	}
	next, ok := m.nextQueuedPrompt()
	if !ok {
		return m, nil
	}
	m.pushSystemMsg(fmt.Sprintf("continuing with queued request: %s", truncateLabel(next.Input, 140)))
	return m.startPromptRun(next)
}
