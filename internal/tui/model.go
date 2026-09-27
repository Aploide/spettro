package tui

import (
	"context"
	"image/color"
	"strings"
	"time"

	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"spettro/internal/agent"
	"spettro/internal/checkpoint"
	"spettro/internal/commands"
	"spettro/internal/config"
	"spettro/internal/memory"
	"spettro/internal/notify"
	"spettro/internal/provider"
	"spettro/internal/remote"
	"spettro/internal/session"
	"spettro/internal/spettro"
	"spettro/internal/storage"
	"spettro/internal/telegram"
	"spettro/internal/theme"
	"spettro/internal/update"
	"spettro/internal/version"
)

const coAuthorInfo = "Co-Authored-By: Spettro <spettro@eyed.to>"

// compactSummaryPrefix marks the system message that replaces the transcript
// after a /compact. The cross-turn history builder treats such a message as a
// conversation summary worth carrying forward (see buildConversationHistory).
const compactSummaryPrefix = "── conversation compacted ──"

// maxConversationHistoryBytes bounds the cross-turn transcript fed back to the
// model on each turn so token cost stays controlled. It mirrors the agent
// package's maxHistoryBytes (32 KB) for the in-run tool log; most-recent turns
// win when the cap is hit.
const maxConversationHistoryBytes = 32 * 1024

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
	// Seq is a monotonically increasing identity assigned when a completed
	// tool entry is created. It lets an asynchronously-computed file diff
	// (toolDiffMsg) be attached to exactly the right entry even after the
	// tool-stream messages are merged. Zero means "no async diff pending".
	Seq int
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
	content       string
	meta          string
	tools         []agent.ToolTrace
	tokensUsed    int // cumulative session cost contributed by this run
	contextTokens int // approximate context occupancy after this run
	err           error
	// goalComplete is set when the agent called goal-complete during this run
	// (only meaningful for goal-mode runs). The outer orchestration loop reads
	// this to decide whether to stop or continue.
	goalComplete bool
	goalSummary  string
	// messages is the full structured post-run conversation (RunResult.Messages),
	// stored as the next turn's cache-stable prefix.
	messages []provider.Message
}

type planDoneMsg struct {
	plan          string
	tools         []agent.ToolTrace
	tokensUsed    int
	contextTokens int
	err           error
	// messages mirrors agentDoneMsg.messages: the structured post-run
	// conversation carried into the next turn.
	messages []provider.Message
}

type commitDoneMsg struct {
	commitMsg string
	err       error
}

type searchDoneMsg struct {
	result string
	err    error
}

type memoryEditDoneMsg struct{ err error }

type bannerClearMsg struct{}
type quitWarningMsg struct{}

type compactDoneMsg struct {
	summary string
	// messages is the compacted structured history, when the carried one
	// was compacted (nil after summarizing the flat transcript: the history
	// is then reseeded from the summary).
	messages []provider.Message
	// noop reports that the history was too short to shrink.
	noop bool
	err  error
}

type toolProgressMsg struct {
	trace agent.ToolTrace
}

// streamChunkMsg delivers one demultiplexed thinking/answer chunk from the
// running agent so the transcript can stream tokens live.
type streamChunkMsg struct {
	chunk agent.StreamChunk
}

// usageEventMsg delivers per-request token accounting from the running agent
// so the footer token counter and context gauge update after every LLM call
// instead of only when the run finishes.
type usageEventMsg struct {
	event agent.UsageEvent
}

// modifiedFilesMsg delivers the result of an asynchronous git query for the
// side-panel branch + modified-file list (see refreshModifiedFilesCmd).
type modifiedFilesMsg struct {
	branch string
	files  []modifiedFileEntry
}

// toolDiffMsg delivers an asynchronously-computed file diff to attach to the
// completed tool entry identified by seq (see computeFileDiffCmd).
type toolDiffMsg struct {
	seq  int
	diff string
}

type parallelAgentEntry struct {
	ID       string
	Label    string
	Kind     string // "worker", "microagent", or "swarm" (Ultra fan-out member)
	Instance int
	Task     string
	Status   string
	At       time.Time // when the agent started running
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
	// previewCache is filled by approvalPreviewLines the first time the
	// dialog is drawn; see approvalPreviewCache.
	previewCache *approvalPreviewCache
}

type shellApprovalResponse struct {
	decision agent.ShellApprovalDecision
	err      error
}

// askUserRequestMsg carries a whole form: every question the agent asked in one
// tool call is put to the user together, and every answer comes back together.
type askUserRequestMsg struct {
	form     agent.AskUserForm
	response chan askUserResponse
}

type askUserResponse struct {
	answers []agent.AskUserAnswer
	err     error
}

type queuedPrompt struct {
	Input          string
	Prompt         string
	MentionedFiles []string
	Images         []string
}

// goalState tracks an in-flight /goal run across the outer orchestration loop.
// nil means no goal is active.
type goalState struct {
	Objective       string // the user's goal text, verbatim
	Iteration       int    // outer-loop iterations dispatched so far
	NoProgress      int    // consecutive iterations with no detected progress
	StartedAt       time.Time
	LastSignature   string // fingerprint of workspace/tool state, for progress detection (step 04)
	MaxIterations   int    // resolved from cfg at start (0 = unlimited)
	NoProgressLimit int    // resolved from cfg at start
	Completed       bool   // set when goal-complete fired (step 03/04)
	Summary         string // completion summary, if any
	Retries         int    // bounded retry counter for hard run errors
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

	// renderCache memoizes per-message rendered blocks so the chat transcript
	// is not re-rendered (markdown regex and all) on every frame. See
	// renderMessages / renderCacheState. Pointer so the cache survives the
	// value-copy semantics of the Bubble Tea Model.
	renderCache *renderCacheState

	mode string
	cfg  config.UserConfig

	messages []ChatMessage

	// convHistory is the structured model-facing conversation carried across
	// turns (user/assistant turns, tool calls and tool results), exactly as
	// returned by the last run's RunResult.Messages. Passing it back verbatim
	// keeps the provider request prefix byte-stable so prompt caching hits and
	// previously generated tokens are never re-summarized or discarded. It is
	// distinct from m.messages, which is the human-facing transcript.
	convHistory []provider.Message

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
	// connectLocalURL holds the endpoint URL between the local connect steps
	// (URL entry → optional API key entry → probe).
	connectLocalURL string

	cmdItems  []commandDef
	cmdCursor int

	// customCommands are user-defined slash commands discovered from
	// ~/.spettro/commands and <cwd>/.spettro/commands at startup.
	customCommands []commands.Command

	repoFiles []string
	// mentionItems are the completions offered for the token being typed:
	// file paths for an @mention, skill names for a $mention (mentionKind
	// says which). Each item is the text inserted after the sigil.
	mentionItems  []string
	mentionKind   mentionKind
	mentionCursor int

	showSetup bool
	setup     setupState

	showOnboarding bool
	onboarding     onboardingState

	showLogin bool
	login     loginState

	favorites map[string]bool

	pendingPlan string

	banner        string
	bannerKind    string
	bannerClearAt time.Time // when set, banner auto-clears at this time

	ctrlCAt time.Time

	showTrust   bool
	trustCursor int

	showTools bool
	// showFullOutput (ctrl+g) lifts the per-tool line caps while showTools
	// (ctrl+o) is on; hiding details clears it.
	showFullOutput bool

	mouseCaptureOff bool
	// textSel tracks an in-progress left-drag text selection over the frame
	// (screen cell coordinates); released selections are copied via OSC 52.
	textSel textSelection

	liveTools   []ToolItem
	currentTool *ToolItem
	toolCh      chan agent.ToolTrace
	streamCh    chan agent.StreamChunk
	usageCh     chan agent.UsageEvent
	approvalCh  chan shellApprovalRequestMsg
	askUserCh   chan askUserRequestMsg
	cancelAgent context.CancelFunc
	pendingAuth *shellApprovalRequestMsg
	// pendingQuestion is the form the question modal is showing, nil when no
	// form is open. It owns the whole interaction: see dialog_question.go.
	pendingQuestion *questionForm
	// questionQueue holds forms that arrived while another one was on
	// screen — parallel tool calls, or a second agent question raised while
	// the user was still typing an answer. They are asked in arrival order as
	// each is answered; none is ever dropped, because every one of them has a
	// tool call blocked on its reply.
	questionQueue  []askUserRequestMsg
	approvalCursor int
	// approvalPreviewExpanded toggles (ctrl+o) the approval dialog's preview
	// (the diff of a file-write/file-edit, or a long command) between its
	// collapsed cap and every row the terminal can spare.
	approvalPreviewExpanded bool
	// approvalScroll is the first preview row on screen; pgup/pgdn move it.
	// approvalLayout clamps it to the preview on every render.
	approvalScroll  int
	progressNote    string
	pendingPrompts  []queuedPrompt
	awaitingInstead bool
	// steering carries mid-run user guidance into the active run; the agent
	// loop drains it at every step boundary. One queue per Model so goal-mode
	// iterations share it (a message typed between iterations reaches the
	// next one). Pointer: survives Bubble Tea's value-copy semantics.
	steering *agent.SteeringQueue
	// showSteerChoice is the "steer now or queue?" picker opened when the
	// user submits input while a run is active; steerPending holds that input.
	showSteerChoice bool
	steerCursor     int
	steerPending    string
	activePrompt    *queuedPrompt
	activeAgentID   string

	showPlanApproval   bool
	planApprovalCursor int

	parallelAgents []parallelAgentEntry
	// workflow is the most recent workflow run, kept after it finishes so the
	// panel still shows the whole tree until the next turn clears it.
	workflow         *workflowRun
	tickCount        int
	sideCursor       int
	sideDetailScroll int
	modifiedFiles    []modifiedFileEntry
	gitBranch        string
	// lastModifiedRefreshAt throttles the async git modified-files query
	// (see scheduleModifiedRefresh).
	lastModifiedRefreshAt time.Time
	// lastRepoScanAt throttles the async repo-file scan that feeds @-mention
	// suggestions (see scheduleRepoScan).
	lastRepoScanAt time.Time
	// toolSeq is the monotonic counter handed to completed ToolItems so an
	// async file diff can be matched back to its entry.
	toolSeq         int
	showSidePanel   bool
	sessionEdits    map[string]struct{}
	activityFeed    []activityItem
	currentRunKey   string
	recentApprovals []session.AgentEvent

	// totalTokensUsed is the cumulative session token COST (sum of every
	// run's prompt+completion). It drives the goodbye stats and remote status
	// — NOT the context gauge.
	totalTokensUsed int
	// liveRunTokens is the portion of the current run's cost already applied
	// to totalTokensUsed by live usageEventMsg updates, so the done handler
	// only adds the remainder instead of double-counting the whole run.
	liveRunTokens int
	// contextTokens is the approximate current context-window OCCUPANCY: the
	// largest single LLM request of the most recent run. The compaction gauge
	// and auto-compaction read this so a multi-step run no longer inflates the
	// reading by summing each step's re-embedded history (EFF-3).
	contextTokens       int
	autoCompactFailures int
	compactWarningLevel int
	autoCompactInFlight bool
	// autoCompactNoopLen is the length of convHistory when an automatic
	// compaction last found nothing worth compacting; auto-compaction waits
	// for the history to change before trying again.
	autoCompactNoopLen int
	sessionID          string

	// lastAutoSaveAt throttles debounced session writes (see
	// autoSaveDebounced). Zero value means "never saved", so the first save
	// always fires.
	lastAutoSaveAt time.Time

	showResume   bool
	resumeItems  []session.Summary
	resumeCursor int
	resumeScroll int

	// Memory review inbox: candidates drafted by
	// /memory mine awaiting approval or discard.
	showMemoryReview   bool
	memoryReviewItems  []memory.Candidate
	memoryReviewCursor int

	// /memory curate proposed ops awaiting apply or skip.
	showMemoryCurate   bool
	memoryCurateItems  []curateItem
	memoryCurateCursor int

	// Checkpointing / rewind checkpointer is opened lazily on the
	// first agent run; checkpointerFailed latches an open failure so we don't
	// retry (and re-warn) every run.
	checkpointer       *checkpoint.Checkpointer
	checkpointerFailed bool
	showRewind         bool
	rewindItems        []checkpoint.Checkpoint
	rewindCounts       []int // per item: files edited during that turn (see openRewind)
	rewindCursor       int
	rewindScroll       int
	rewindModePick     bool
	rewindModeCursor   int
	// lastEscAt drives the esc-esc shortcut that opens /rewind when idle.
	lastEscAt time.Time

	// /storage clean multi-select: items from the storage registry with
	// per-item checkboxes (safe defaults preselected).
	showStorageClean bool
	storageItems     []storage.Item
	storageChecked   []bool
	storageCursor    int

	todos []session.Todo

	remoteServer        *remote.Server
	remoteRequestedPort int

	telegramRelay *telegram.Relay

	// updateAvailable is populated by a background version check on startup;
	// nil means none was found (or the check hasn't completed / failed —
	// failures are silent since this is a passive background check).
	updateAvailable *update.Release
	updateBusy      bool
	// relaunchBinary is set once /update has finished installing a new
	// binary. main() reads it via RelaunchPath after the TUI exits and execs
	// into it so the restart is seamless.
	relaunchBinary string

	// startupCmds are tea.Cmds that need to fire on Init. Populated by
	// New() when initial state requires background work (e.g. autostarting
	// the Telegram relay) before the first tea event is processed.
	startupCmds []tea.Cmd

	// themeAuto is set when the palette was seeded by detection rather than
	// chosen, and a late answer to the terminal's background query is still
	// allowed to revise it. An explicit dark/light selection leaves it false
	// so nothing can flip the UI out from under the user mid-session.
	themeAuto bool

	// themeDetected records that the palette in force came from the
	// terminal's own answer to the background query, rather than from
	// COLORFGBG or the dark fallback. Only /theme reads it, to report how the
	// current palette was arrived at instead of guessing after the fact.
	themeDetected bool

	// showThemePicker owns the /theme overlay: a list of the themes with a
	// panel previewing the one under the cursor. Nothing global changes until
	// the choice is confirmed, so cancelling is just closing.
	showThemePicker bool

	// themeCursor indexes themePickerOrder.
	themeCursor int

	// Attachments (ctrl+f to attach, ctrl+r to remove; ctrl+v to paste image)
	attachments      []attachmentItem
	showAttachPrompt bool
	attachDraft      string // textarea value saved while attach prompt is open
	clipboardTempDir string // temp dir for pasted images, created on first paste
	clipboardCounter int    // increments each paste for [Image #N] labelling

	// Notifications (OSC 9 / BEL / desktop)
	agentStartAt time.Time
	// workingVerb is the status word the working indicator shows for the
	// current turn, drawn once when the run starts (see beginRunIndicator).
	workingVerb     string
	terminalFocused bool
	notifier        *notify.Notifier

	cwd       string
	store     *storage.Store
	providers *provider.Manager
	manifest  config.AgentManifest
	committer agent.CommitAgent
	searcher  agent.SearchAgent
	// sandboxState is the session-scoped OS sandbox policy shared with every
	// agent run; nil when the binary was started without sandbox plumbing
	// (tests).
	sandboxState *agent.SandboxState

	// livePerm shares the user's current permission level with agent runs
	// already in flight, so a mid-run /permission change (e.g. to yolo)
	// applies to the rest of the run instead of only the next one.
	livePerm *livePermission

	// activeGoal is non-nil while a /goal run is in progress. The goal persists
	// across agent runs (resetRunState does NOT clear it); it is cleared by the
	// orchestration loop (step 04) on completion / stall / interrupt.
	activeGoal *goalState

	// pendingGoalResume is set when a session is loaded and contains an
	// unfinished goal record. The user is offered to resume via /goal resume.
	pendingGoalResume *session.GoalRecord

	// goalResumeAfterCompact is set when an inter-iteration compaction is in
	// flight during a goal run. The compactDoneMsg handler checks it to resume
	// the goal loop after a successful compaction.
	goalResumeAfterCompact bool

	// activeLoop is non-nil while a /loop schedule is active. The loop persists
	// across agent runs (resetRunState does NOT clear it); it is cleared by
	// /loop stop or a user interrupt of an in-flight run.
	activeLoop *loopState

	// loopSeq numbers loops so timer ticks scheduled by a stopped or replaced
	// loop can be recognized and dropped.
	loopSeq int
}

func New(cwd string, cfg config.UserConfig, store *storage.Store, pm *provider.Manager, sb *agent.SandboxState) Model {
	// Resolve the palette before anything else: the textarea chrome and the
	// spinner style below are captured at construction, so a theme installed
	// after them would leave the input box in the wrong palette for the rest
	// of the session. For "auto" theme.Seed reads COLORFGBG when there is a
	// terminal that could be asked and falls back to dark when there is not —
	// a pure env read either way, no I/O — so the first paint is usually
	// already right; Init then asks the terminal for the real answer.
	wanted := theme.Preferred(cfg.Theme)
	pal := theme.Set(theme.Seed(wanted))

	ta := textarea.New()
	ta.Placeholder = "enter message…"
	ta.ShowLineNumbers = false
	ta.CharLimit = 8000
	ta.SetHeight(3)
	ta.SetStyles(textareaStyles(pal))
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

	// Never start with a model the user can't run (fresh install, key removed
	// since last run): keep the configured model only if its provider has
	// credentials, otherwise switch to the best connected model. Persist the
	// switch so headless/ACP runs agree with what the TUI shows.
	if resolvedProv, resolvedModel := pm.ResolveActive(cfg.ActiveProvider, cfg.ActiveModel, cfg.APIKeys); resolvedProv != cfg.ActiveProvider || resolvedModel != cfg.ActiveModel {
		cfg.ActiveProvider = resolvedProv
		cfg.ActiveModel = resolvedModel
		if updated, err := config.Update(func(c *config.UserConfig) error {
			c.ActiveProvider = resolvedProv
			c.ActiveModel = resolvedModel
			return nil
		}); err == nil {
			cfg = updated
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
		searcher:     agent.NewRepoSearcher(cwd),
		sandboxState: sb,
		historyIndex: -1,
		themeAuto:    wanted == theme.AutoKind,
		livePerm:     &livePermission{},
		notifier:     notify.New(!cfg.NotificationsDisabled, time.Duration(cfg.NotifyQuietSec)*time.Second),
	}
	m.livePerm.set(cfg.Permission)
	m.customCommands, _ = commands.Discover(cwd)
	m.refreshModifiedFiles()
	// Scan the working directory in the background: walking a large tree
	// synchronously here would block the first paint (seen: ~56s from $HOME).
	m.lastRepoScanAt = time.Now()
	m.startupCmds = append(m.startupCmds, scanRepoFilesCmd(cwd))
	// Warm the repo symbol index off the UI thread so the first symbol-aware
	// grep's symbol search answers from cache instead of paying the initial scan.
	if rs, ok := m.searcher.(agent.RepoSearcher); ok && rs.Index != nil {
		idx := rs.Index
		m.startupCmds = append(m.startupCmds, func() tea.Msg {
			idx.Warm(context.Background())
			return nil
		})
	}
	// Skip the update check entirely for from-source ("dev") builds — there
	// is no meaningful version to compare, and self-replacing a dev binary
	// with an official release would surprise a developer.
	if version.App != "dev" {
		m.startupCmds = append(m.startupCmds, checkUpdateCmd())
	}
	if cmd := m.autostartTelegram(); cmd != nil {
		m.startupCmds = append(m.startupCmds, cmd)
	}
	// If a Spettro Subscription is connected, register its endpoint immediately
	// (so inference resolves) and refresh the model list + plan in the background.
	hasSpettro := strings.TrimSpace(cfg.APIKeys[spettro.ProviderID]) != ""
	if hasSpettro {
		pm.SetSpettro(spettro.InferenceBaseURL(), nil)
		spettroKey := cfg.APIKeys[spettro.ProviderID]
		// Spettro models arrive async; if no usable model is active yet (e.g.
		// signed in but no other provider connected), activate the first
		// subscription model once the list lands.
		activateOnLoad := !provider.HasCredentials(cfg.APIKeys, cfg.ActiveProvider)
		m.startupCmds = append(m.startupCmds, loadSpettroCmd(spettroKey, activateOnLoad, false))
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

func (m Model) currentColor() color.Color {
	if spec, ok := m.manifest.AgentByID(m.mode); ok {
		return modeColor(spec.Color)
	}
	return modeColor(m.mode)
}

func (m Model) Init() tea.Cmd {
	cmds := []tea.Cmd{textarea.Blink, tick(), m.spin.Tick}
	// Only "auto" asks the terminal what colour it is. An explicit selection
	// must never put an OSC 11 query on the wire, and neither must a
	// redirected stdout — Bubble Tea writes the sequence whether or not the
	// far end can answer it, so an unguarded query would land as literal
	// escape bytes in a captured transcript.
	if m.themeAuto && theme.CanQueryTerminal() {
		cmds = append(cmds, tea.RequestBackgroundColor)
	}
	cmds = append(cmds, m.startupCmds...)
	return tea.Batch(cmds...)
}

// textareaStyles adapts bubbles' input chrome — placeholder, cursor line,
// line numbers, end-of-buffer markers — to the palette. Only the polarity is
// taken from the theme; the prompt and cursor-line overrides are the same
// blanking the input box has always done.
func textareaStyles(p theme.Palette) textarea.Styles {
	var s textarea.Styles
	if p.IsDark() {
		s = textarea.DefaultDarkStyles()
	} else {
		s = textarea.DefaultLightStyles()
		// bubbles' DefaultStyles hardcodes Cursor.Color to ANSI 7 for both
		// polarities, and the virtual cursor renders as that colour reversed —
		// so the caret becomes a silver block whose glyph is painted in the
		// terminal's own background. On a light terminal that is white on
		// white: the caret and the character under it both vanish. Painting it
		// with body text ink instead gives a dark block with the page showing
		// through the glyph. The dark branch is left alone so its bytes stay
		// exactly what shipped before themes existed.
		s.Cursor.Color = p.Text
	}
	s.Focused.CursorLine = lipgloss.NewStyle()
	s.Focused.Prompt = lipgloss.NewStyle()
	s.Blurred.Prompt = lipgloss.NewStyle()
	return s
}

// applyTheme installs a resolved palette and repaints everything that captured
// colours earlier. Both of the TUI's memoised render caches key on layout, not
// on the palette, so they have to be dropped by hand or already-rendered
// strings keep the old theme while new ones arrive in the new one: the message
// blocks in renderCache, and the question form's preview, whose "… N more
// lines" footer is styled at cache-fill time.
func (m Model) applyTheme(k theme.Kind) Model {
	pal := theme.Set(k)
	m.ta.SetStyles(textareaStyles(pal))
	m.renderCache = nil
	if m.pendingQuestion != nil {
		m.pendingQuestion.previewKey, m.pendingQuestion.previewLines = "", nil
	}
	m.refreshViewport()
	return m
}

func tick() tea.Cmd {
	return tea.Tick(50*time.Millisecond, func(t time.Time) tea.Msg { return tickMsg(t) })
}

var spinnerFrames = []string{"⣾", "⣽", "⣻", "⢿", "⡿", "⣟", "⣯", "⣷"}

func agentTickCmd() tea.Cmd {
	return tea.Tick(500*time.Millisecond, func(time.Time) tea.Msg { return agentTickMsg{} })
}
