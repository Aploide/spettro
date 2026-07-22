package acp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	acpsdk "github.com/coder/acp-go-sdk"

	"spettro/internal/agent"
	"spettro/internal/config"
	"spettro/internal/provider"
	"spettro/internal/session"
	"spettro/internal/version"
)

// bridge implements acpsdk.Agent on top of the LLMAgent runtime. One bridge
// serves one editor connection; each ACP session maps to an independent
// conversation (own cwd, own agent mode, own bounded history).
type bridge struct {
	conn *acpsdk.AgentSideConnection
	opts Options

	mu       sync.Mutex
	sessions map[string]*acpSession
}

// acpSession is the per-conversation state. Mutable fields are guarded by the
// owning bridge's mu. A session runs at most one agent turn at a time; a
// prompt that arrives while one is running is delivered to it as mid-run
// steering (see beginRun / steerRunningTurn) rather than starting a new turn.
type acpSession struct {
	id       string
	cwd      string
	agentID  string
	manifest config.AgentManifest
	mediaDir string
	// history is the structured conversation carried across prompt turns,
	// exactly as returned by the last run's RunResult.Messages (assistant
	// turns, tool calls and tool results included). Passing it back verbatim
	// keeps the provider request prefix byte-stable so prompt caching hits;
	// growth is bounded by the runtime's in-loop compaction, not by evicting
	// lines here (head eviction would churn the prefix and defeat the cache).
	history []provider.Message
	// transcript is the flat user/assistant conversation persisted to the
	// session store after each turn (the same store the TUI's /resume reads).
	// It is what session/load replays and what seeds the flattened History
	// fallback on the first turn after a load, when no structured history
	// exists yet.
	transcript []session.Message
	startedAt  time.Time
	// commandsAnnounced records that a prompt turn has re-sent the available
	// commands list, the fallback for clients that dropped the initial
	// announcement (see NewSession).
	commandsAnnounced bool
	// lastGoal is the outcome summary of the most recent /goal run, surfaced
	// by /goal status.
	lastGoal string
	// running / runCancel track the in-flight prompt turn. The agent runs
	// under a session-owned context detached from the SDK's request context
	// (the SDK cancels that as soon as ANY new prompt arrives for the
	// session); runCancel is what an explicit session/cancel fires instead.
	running   bool
	runCancel context.CancelFunc
	// steering carries user text sent while a turn is running into that run:
	// the tool loop drains it at every step boundary. It outlives single
	// turns, so a message the run never reached is delivered at the start of
	// the next one instead of being lost.
	steering *agent.SteeringQueue
	// permission is the live permission level the in-flight run consults on
	// every approval decision. Updated by /permission and the permission
	// config option so a mid-run change (e.g. to yolo) applies immediately.
	permission config.PermissionLevel
	// autoCompactFailures counts consecutive failed auto-compactions; past
	// the configured maximum, auto compaction pauses (mirrors the TUI) and
	// the pre-turn guard falls back to asking the user instead.
	autoCompactFailures int
}

var _ acpsdk.Agent = (*bridge)(nil)

func newBridge(opts Options) *bridge {
	return &bridge{opts: opts, sessions: make(map[string]*acpSession)}
}

func (b *bridge) Initialize(_ context.Context, params acpsdk.InitializeRequest) (acpsdk.InitializeResponse, error) {
	return acpsdk.InitializeResponse{
		ProtocolVersion: acpsdk.ProtocolVersionNumber,
		AgentInfo: &acpsdk.Implementation{
			Name:    "spettro",
			Title:   new("Spettro"),
			Version: version.App,
		},
		AgentCapabilities: acpsdk.AgentCapabilities{
			LoadSession: true,
			SessionCapabilities: acpsdk.SessionCapabilities{
				List:   &acpsdk.SessionListCapabilities{},
				Resume: &acpsdk.SessionResumeCapabilities{},
			},
			PromptCapabilities: acpsdk.PromptCapabilities{
				Image:           true,
				EmbeddedContext: true,
			},
		},
		AuthMethods: []acpsdk.AuthMethod{
			{
				Terminal: &acpsdk.AuthMethodTerminalInline{
					Id:   "spettro-setup",
					Name: "Configure a provider",
					Description: new(
						"Launch Spettro's own interactive TUI (no --acp flag) and " +
							"run /models to add a provider API key; ACP sessions " +
							"reuse that stored configuration.",
					),
					// No extra args: the plain `spettro` invocation already opens
					// the interactive TUI where /models manages provider keys.
					Args: []string{},
				},
			},
		},
	}, nil
}

// Authenticate is a no-op: the advertised auth method just points the client
// at running `spettro` directly, which handles provider setup itself.
func (b *bridge) Authenticate(_ context.Context, _ acpsdk.AuthenticateRequest) (acpsdk.AuthenticateResponse, error) {
	return acpsdk.AuthenticateResponse{}, nil
}

func (b *bridge) NewSession(ctx context.Context, params acpsdk.NewSessionRequest) (acpsdk.NewSessionResponse, error) {
	cwd := params.Cwd
	if cwd == "" {
		cwd = b.opts.CWD
	}
	if !filepath.IsAbs(cwd) {
		return acpsdk.NewSessionResponse{}, acpsdk.NewInvalidParams(map[string]any{"error": "cwd must be an absolute path"})
	}

	// Sessions may target a different project than the process cwd, so load
	// that project's manifest; fall back to the process manifest on error.
	manifest, err := config.LoadAgentManifestForProject(cwd)
	if err != nil {
		manifest = b.opts.Manifest
	}
	agentID := manifest.DefaultAgent
	if agentID == "" {
		agentID = "plan"
	}

	sid := session.NewID(cwd)
	s := &acpSession{
		id:        sid,
		cwd:       cwd,
		agentID:   agentID,
		manifest:  manifest,
		mediaDir:  filepath.Join(session.SessionDir(b.opts.GlobalDir, sid), "acp-media"),
		startedAt: time.Now(),
	}
	b.mu.Lock()
	b.sessions[sid] = s
	b.mu.Unlock()

	// Config options describe the mode, model, permission, and thinking
	// selectors the editor draws in its toolbar. Load fresh config so those
	// selectors reflect the current model/permission (mirrors Prompt).
	cfg := b.opts.Cfg
	if fresh, err := config.LoadFull(); err == nil {
		cfg = fresh
		b.opts.Providers.SetAPIKeys(cfg.APIKeys)
	}

	// The command list must be announced AFTER the session/new response is on
	// the wire: clients (Zed) only register the session when the response
	// arrives and silently drop session/update notifications for unknown
	// sessions — announcing synchronously here loses the commands and the
	// editor rejects every "/…" input with "not a recognized command".
	// Deferring past the handler return keeps the write ordered behind the
	// response. Prompt re-announces once more as a belt-and-braces fallback.
	go func() {
		time.Sleep(200 * time.Millisecond)
		b.announceCommands(context.Background(), acpsdk.SessionId(sid))
	}()

	return acpsdk.NewSessionResponse{
		SessionId:     acpsdk.SessionId(sid),
		ConfigOptions: buildConfigOptions(s, &cfg, b.opts.Providers),
	}, nil
}

// SetSessionMode maps ACP session modes onto Spettro agents (plan, coding,
// ask, ...), the same switch the TUI's /mode command performs.
func (b *bridge) SetSessionMode(_ context.Context, params acpsdk.SetSessionModeRequest) (acpsdk.SetSessionModeResponse, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	s, ok := b.sessions[string(params.SessionId)]
	if !ok {
		return acpsdk.SetSessionModeResponse{}, fmt.Errorf("session %s not found", params.SessionId)
	}
	if _, ok := s.manifest.AgentByID(string(params.ModeId)); !ok {
		return acpsdk.SetSessionModeResponse{}, acpsdk.NewInvalidParams(map[string]any{"error": "unknown mode: " + string(params.ModeId)})
	}
	s.agentID = string(params.ModeId)
	return acpsdk.SetSessionModeResponse{}, nil
}

// Cancel stops the session's in-flight run. The SDK also cancels the prompt
// request context, but the agent deliberately does not run under that context
// (a new prompt for the session cancels it too — see Prompt's steering path),
// so the explicit cancel must land here.
func (b *bridge) Cancel(_ context.Context, params acpsdk.CancelNotification) error {
	b.mu.Lock()
	var cancel context.CancelFunc
	if s, ok := b.sessions[string(params.SessionId)]; ok {
		cancel = s.runCancel
	}
	b.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return nil
}

// beginRun atomically claims the session's single run slot. On success it
// returns the context the agent must run under — derived from the bridge
// lifetime but NOT from the SDK's per-request context, which is cancelled
// whenever another prompt arrives for the session — plus a finish func that
// releases the slot. ok=false means a turn is already running: the caller
// should deliver the prompt as steering instead. Explicit session/cancel
// goes through Cancel → s.runCancel.
func (b *bridge) beginRun(ctx context.Context, s *acpSession) (runCtx context.Context, finish func(), ok bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if s.steering == nil {
		s.steering = agent.NewSteeringQueue()
	}
	if s.running {
		return nil, nil, false
	}
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	s.running = true
	s.runCancel = cancel
	return runCtx, func() {
		b.mu.Lock()
		s.running = false
		s.runCancel = nil
		b.mu.Unlock()
		cancel()
	}, true
}

// steerRunningTurn delivers a prompt that arrived while a turn was already in
// flight as mid-run steering: the text is queued for injection at the agent's
// next step boundary and this (second) prompt turn ends immediately. Clients
// that want replace-semantics instead send session/cancel first, which stops
// the run before the new prompt arrives.
func (b *bridge) steerRunningTurn(ctx context.Context, s *acpSession, sessionID acpsdk.SessionId, task string) (acpsdk.PromptResponse, error) {
	b.mu.Lock()
	q := s.steering
	// Record the steering text in the flat transcript now; the structured
	// history picks it up from the running turn's RunResult.Messages.
	s.transcript = append(s.transcript, session.Message{Role: "user", Content: task, At: time.Now()})
	b.mu.Unlock()
	q.Push(task)
	_ = b.conn.SessionUpdate(ctx, acpsdk.SessionNotification{
		SessionId: sessionID,
		Update:    acpsdk.UpdateAgentMessageText("→ steering queued: the running agent will see this message at its next step"),
	})
	return acpsdk.PromptResponse{StopReason: acpsdk.StopReasonEndTurn}, nil
}

func (b *bridge) Prompt(ctx context.Context, params acpsdk.PromptRequest) (acpsdk.PromptResponse, error) {
	b.mu.Lock()
	s, ok := b.sessions[string(params.SessionId)]
	var announced bool
	if ok {
		announced = s.commandsAnnounced
		s.commandsAnnounced = true
	}
	b.mu.Unlock()
	if !ok {
		return acpsdk.PromptResponse{}, fmt.Errorf("session %s not found", params.SessionId)
	}
	// Re-announce the commands once per session from inside a prompt turn:
	// by now the client provably knows the session, so this delivery cannot
	// be dropped even if the deferred NewSession announcement raced.
	if !announced {
		b.announceCommands(ctx, params.SessionId)
	}

	// Reload config each turn so key/model/permission changes made in a
	// concurrent TUI or via `spettro` config commands take effect (mirrors
	// headless mode).
	cfg := b.opts.Cfg
	if fresh, err := config.LoadFull(); err == nil {
		cfg = fresh
		b.opts.Providers.SetAPIKeys(cfg.APIKeys)
	}

	task, images, mentioned, err := promptFromBlocks(params.Prompt, s.mediaDir)
	if err != nil {
		return acpsdk.PromptResponse{}, acpsdk.NewInvalidParams(map[string]any{"error": err.Error()})
	}
	trimmedTask := strings.TrimSpace(task)
	if trimmedTask == "" {
		return acpsdk.PromptResponse{}, acpsdk.NewInvalidParams(map[string]any{"error": "prompt has no text content"})
	}

	turn := &turnState{
		bridge:    b,
		ctx:       ctx,
		sessionID: params.SessionId,
		open:      make(map[string][]acpsdk.ToolCallId),
	}

	if strings.HasPrefix(trimmedTask, "/") {
		// /plan <task> runs the plan agent on the task as a one-shot turn
		// (mirrors the TUI); bare /plan is a mode switch handled by the
		// extended slash-command set below.
		if fields := strings.Fields(trimmedTask); fields[0] == "/plan" && len(fields) > 1 {
			b.mu.Lock()
			if _, ok := s.manifest.AgentByID("plan"); ok {
				s.agentID = "plan"
			}
			b.mu.Unlock()
			trimmedTask = strings.TrimSpace(strings.TrimPrefix(trimmedTask, "/plan"))
			task = trimmedTask
		} else if fields[0] == "/goal" {
			if strings.TrimSpace(strings.TrimPrefix(trimmedTask, "/goal")) == "stop" {
				// /goal stop while a goal turn is running: cancel that run.
				b.mu.Lock()
				cancel := s.runCancel
				b.mu.Unlock()
				if cancel != nil {
					cancel()
					_ = b.conn.SessionUpdate(ctx, acpsdk.SessionNotification{
						SessionId: params.SessionId,
						Update:    acpsdk.UpdateAgentMessageText("goal stopped"),
					})
					return acpsdk.PromptResponse{StopReason: acpsdk.StopReasonEndTurn}, nil
				}
			}
			runCtx, finish, ok := b.beginRun(ctx, s)
			if !ok {
				// A goal/run is already in flight: treat "/goal <text>" sent
				// mid-turn as steering for it (minus the command prefix).
				return b.steerRunningTurn(ctx, s, params.SessionId,
					strings.TrimSpace(strings.TrimPrefix(trimmedTask, "/goal")))
			}
			defer finish()
			turn.ctx = runCtx
			b.ensureContextHeadroom(runCtx, s, &cfg, turn)
			return b.runGoalCommand(runCtx, s, &cfg, turn, trimmedTask)
		}
		if fields := strings.Fields(trimmedTask); fields[0] == "/compact" {
			return b.handleCompactCommand(ctx, s, &cfg, turn, trimmedTask)
		}
		b.mu.Lock()
		reply, modeChanged, handled := handleSlashCommand(s, &cfg, b.opts.Providers, trimmedTask)
		if !handled {
			reply, modeChanged, handled = handleExtendedSlashCommand(b, s, &cfg, b.opts.Providers, trimmedTask)
		}
		_ = modeChanged
		var options []acpsdk.SessionConfigOption
		if handled {
			options = buildConfigOptions(s, &cfg, b.opts.Providers)
		}
		b.mu.Unlock()
		if handled {
			if reply != "" {
				_ = b.conn.SessionUpdate(ctx, acpsdk.SessionNotification{
					SessionId: params.SessionId,
					Update:    acpsdk.UpdateAgentMessageText(reply),
				})
			}
			// A slash command may have changed the mode, model, permission, or
			// thinking level; push the refreshed option set so the editor's
			// toolbar selectors stay in sync (supersedes current_mode_update).
			_ = b.conn.SessionUpdate(ctx, acpsdk.SessionNotification{
				SessionId: params.SessionId,
				Update: acpsdk.SessionUpdate{ConfigOptionUpdate: &acpsdk.SessionConfigOptionUpdate{
					ConfigOptions: options,
				}},
			})
			return acpsdk.PromptResponse{StopReason: acpsdk.StopReasonEndTurn}, nil
		}
	}

	// Claim the session's run slot. If a turn is already executing, this
	// prompt becomes mid-run steering for it instead of a new turn (the SDK
	// has already cancelled the request context of the running turn, but the
	// agent runs under runCtx, so it is unaffected).
	runCtx, finish, ok := b.beginRun(ctx, s)
	if !ok {
		return b.steerRunningTurn(ctx, s, params.SessionId, task)
	}
	defer finish()
	turn.ctx = runCtx

	// Pre-turn context guard: the carried history must never enter a run so
	// large that compaction itself can no longer fit in the window. Compact
	// automatically when enabled, or ask the user before the window fills.
	b.ensureContextHeadroom(runCtx, s, &cfg, turn)

	b.mu.Lock()
	agentID := s.agentID
	manifest := s.manifest
	steering := s.steering
	// Seed the session's live permission from the freshly loaded config; a
	// mid-run /permission or config-option change overwrites it and the
	// running agent picks it up on its next approval decision.
	s.permission = cfg.Permission
	history := s.history
	// First turn after session/load: no structured history exists yet, so
	// fall back to the flattened stored transcript (mirrors the TUI's resume).
	flatHistory := ""
	if len(history) == 0 && len(s.transcript) > 0 {
		flatHistory = flattenTranscript(s.transcript)
	}
	b.mu.Unlock()

	spec, ok := manifest.AgentByID(agentID)
	if !ok {
		return acpsdk.PromptResponse{}, fmt.Errorf("agent not found: %s", agentID)
	}
	spec.Permission = cfg.Permission
	livePermission := func() config.PermissionLevel {
		b.mu.Lock()
		defer b.mu.Unlock()
		return s.permission
	}

	thinking := provider.ThinkingLevel("")
	if b.opts.Providers.SupportsReasoning(cfg.ActiveProvider, cfg.ActiveModel) {
		thinking = provider.ThinkingLevel(cfg.ThinkingLevel)
	}

	contextWindow := b.opts.Providers.ModelContext(cfg.ActiveProvider, cfg.ActiveModel)
	// Turn-level usage accumulation for the final PromptResponse. The
	// callback runs on the agent goroutine and the final read happens after
	// Run returns, so no locking is needed.
	var turnUsage provider.Usage

	ag := agent.LLMAgent{
		Spec:            spec,
		ProviderManager: b.opts.Providers,
		ProviderName:    func() string { return cfg.ActiveProvider },
		ModelName:       func() string { return cfg.ActiveModel },
		CWD:             s.cwd,
		MaxTokens:       cfg.TokenBudget,
		Thinking:        thinking,
		Ultra:           cfg.UltraActive(),
		RequiredReads:   mentioned,
		Images:          images,
		History:         flatHistory,
		Messages:        history,
		Manifest:        &manifest,
		SandboxState:    b.opts.SandboxState,
		SessionDir:      session.SessionDir(b.opts.GlobalDir, s.id),
		ContextWindow:   contextWindow,
		Compact:         cfg.CompactConfig(),
		Steering:        steering,
		StreamCallback:  turn.onStream,
		ToolCallback:    turn.onTool,
		UsageCallback: func(ev agent.UsageEvent) {
			turnUsage.InputTokens += ev.Usage.InputTokens
			turnUsage.OutputTokens += ev.Usage.OutputTokens
			turnUsage.CacheReadTokens += ev.Usage.CacheReadTokens
			turnUsage.CacheWriteTokens += ev.Usage.CacheWriteTokens
			turn.onUsage(ev, contextWindow)
		},
		PermissionFn: livePermission,
		ShellApproval: func(sctx context.Context, ar agent.ShellApprovalRequest) (agent.ShellApprovalDecision, error) {
			if livePermission() == config.PermissionYOLO {
				return agent.ShellApprovalAllowOnce, nil
			}
			return turn.requestShellApproval(sctx, ar)
		},
		AskUser: turn.askUser,
	}

	result, runErr := ag.Run(runCtx, task)

	// Preserve whatever context the run produced — even on failure or
	// cancellation. Losing s.history/s.transcript here is what made a failed
	// or interrupted turn restart the conversation from scratch.
	b.mu.Lock()
	if len(result.Messages) > 0 {
		s.history = result.Messages
	}
	now := time.Now()
	s.transcript = append(s.transcript, session.Message{Role: "user", Content: task, At: now})
	if content := strings.TrimSpace(result.Content); content != "" {
		s.transcript = append(s.transcript, session.Message{Role: "assistant", Content: result.Content, At: now})
	} else if runErr != nil {
		note := "turn interrupted"
		if runCtx.Err() == nil {
			note = "turn failed: " + runErr.Error()
		}
		s.transcript = append(s.transcript, session.Message{Role: "assistant", Content: "[" + note + "]", At: now})
	}
	state := s.persistState()
	b.mu.Unlock()
	// Persist so the TUI's /resume and future session/load calls see this
	// conversation; a save failure must not fail the prompt turn.
	_ = session.Save(b.opts.GlobalDir, state)

	if runErr != nil {
		if runCtx.Err() != nil {
			return acpsdk.PromptResponse{StopReason: acpsdk.StopReasonCancelled}, nil
		}
		return acpsdk.PromptResponse{}, runErr
	}

	// The answer is sent once from the authoritative final content; see
	// turnState.onStream for why answer deltas are not streamed live.
	if result.Content != "" {
		turn.sessionUpdate(acpsdk.UpdateAgentMessageText(result.Content))
	}

	return acpsdk.PromptResponse{
		StopReason: acpsdk.StopReasonEndTurn,
		Usage:      turnUsageResponse(turnUsage, result.TokensUsed),
		Meta:       map[string]any{"spettro.dev/tokensUsed": result.TokensUsed},
	}, nil
}

// turnUsageResponse converts the turn's accumulated provider usage into the
// ACP PromptResponse usage block. When the provider reported no accounting at
// all, only the local estimate (total) is meaningful.
func turnUsageResponse(u provider.Usage, estimatedTotal int) *acpsdk.Usage {
	total := u.TotalInput() + u.OutputTokens
	if total == 0 {
		total = estimatedTotal
	}
	if total == 0 {
		return nil
	}
	out := &acpsdk.Usage{
		InputTokens:  u.InputTokens,
		OutputTokens: u.OutputTokens,
		TotalTokens:  total,
	}
	if u.CacheReadTokens > 0 {
		out.CachedReadTokens = new(u.CacheReadTokens)
	}
	if u.CacheWriteTokens > 0 {
		out.CachedWriteTokens = new(u.CacheWriteTokens)
	}
	return out
}

// requestShellApproval bridges Spettro's shell approval flow to ACP's
// session/request_permission, letting the editor render its native prompt.
func (t *turnState) requestShellApproval(ctx context.Context, ar agent.ShellApprovalRequest) (agent.ShellApprovalDecision, error) {
	title := "Run shell command: " + ar.Command
	update := acpsdk.ToolCallUpdate{
		ToolCallId: t.openToolCallID(ar.ToolID),
		Title:      new(title),
		Kind:       acpsdk.Ptr(acpsdk.ToolKindExecute),
		Status:     acpsdk.Ptr(acpsdk.ToolCallStatusPending),
		RawInput:   map[string]any{"command": ar.Command, "reason": ar.Reason},
	}
	resp, err := t.bridge.conn.RequestPermission(ctx, acpsdk.RequestPermissionRequest{
		SessionId: t.sessionID,
		ToolCall:  update,
		Options: []acpsdk.PermissionOption{
			{OptionId: "allow-once", Name: "Allow once", Kind: acpsdk.PermissionOptionKindAllowOnce},
			{OptionId: "allow-always", Name: "Always allow", Kind: acpsdk.PermissionOptionKindAllowAlways},
			{OptionId: "deny", Name: "Deny", Kind: acpsdk.PermissionOptionKindRejectOnce},
		},
	})
	if err != nil {
		return agent.ShellApprovalDeny, err
	}
	if resp.Outcome.Cancelled != nil || resp.Outcome.Selected == nil {
		return agent.ShellApprovalDeny, nil
	}
	switch string(resp.Outcome.Selected.OptionId) {
	case "allow-once":
		return agent.ShellApprovalAllowOnce, nil
	case "allow-always":
		return agent.ShellApprovalAllowAlways, nil
	default:
		return agent.ShellApprovalDeny, nil
	}
}

// askUser maps the ask-user tool onto session/request_permission, the only
// stable interactive request ACP offers. Options become permission choices;
// free-form input is not representable, so option-less questions fail with a
// hint the model can act on.
func (t *turnState) askUser(ctx context.Context, ar agent.AskUserRequest) (string, error) {
	if len(ar.Options) == 0 {
		if ar.DefaultOption != "" {
			return ar.DefaultOption, nil
		}
		return "", errors.New("this client cannot answer free-form questions; proceed with your best judgment or offer explicit options")
	}
	opts := make([]acpsdk.PermissionOption, 0, len(ar.Options))
	for i, o := range ar.Options {
		opts = append(opts, acpsdk.PermissionOption{
			OptionId: acpsdk.PermissionOptionId(fmt.Sprintf("opt-%d", i)),
			Name:     o,
			Kind:     acpsdk.PermissionOptionKindAllowOnce,
		})
	}
	title := ar.Question
	if ar.Context != "" {
		title += " — " + ar.Context
	}
	resp, err := t.bridge.conn.RequestPermission(ctx, acpsdk.RequestPermissionRequest{
		SessionId: t.sessionID,
		ToolCall: acpsdk.ToolCallUpdate{
			ToolCallId: t.nextToolCallID("ask"),
			Title:      new(title),
			Kind:       acpsdk.Ptr(acpsdk.ToolKindThink),
			Status:     acpsdk.Ptr(acpsdk.ToolCallStatusPending),
		},
		Options: opts,
	})
	if err != nil {
		return "", err
	}
	if resp.Outcome.Cancelled != nil || resp.Outcome.Selected == nil {
		return "", errors.New("user did not answer")
	}
	for i, o := range ar.Options {
		if string(resp.Outcome.Selected.OptionId) == fmt.Sprintf("opt-%d", i) {
			return o, nil
		}
	}
	return "", errors.New("user did not answer")
}

// Unsupported optional capabilities.

func (b *bridge) Logout(_ context.Context, _ acpsdk.LogoutRequest) (acpsdk.LogoutResponse, error) {
	return acpsdk.LogoutResponse{}, acpsdk.NewMethodNotFound(acpsdk.AgentMethodLogout)
}

func (b *bridge) CloseSession(_ context.Context, params acpsdk.CloseSessionRequest) (acpsdk.CloseSessionResponse, error) {
	return acpsdk.CloseSessionResponse{}, acpsdk.NewMethodNotFound(acpsdk.AgentMethodSessionClose)
}

// SetSessionConfigOption applies a change made in the editor's toolbar
// selectors (mode, model, permission, thinking) and returns the full, updated
// option set so the client reflects any dependent changes.
func (b *bridge) SetSessionConfigOption(_ context.Context, params acpsdk.SetSessionConfigOptionRequest) (acpsdk.SetSessionConfigOptionResponse, error) {
	var sid acpsdk.SessionId
	var configID, value string
	switch {
	case params.ValueId != nil:
		sid = params.ValueId.SessionId
		configID = string(params.ValueId.ConfigId)
		value = string(params.ValueId.Value)
	case params.Boolean != nil:
		sid = params.Boolean.SessionId
		configID = string(params.Boolean.ConfigId)
		if params.Boolean.Value {
			value = "true"
		} else {
			value = "false"
		}
	default:
		return acpsdk.SetSessionConfigOptionResponse{}, acpsdk.NewInvalidParams(map[string]any{"error": "missing config option value"})
	}

	// Reload config so a concurrent TUI's changes are the baseline we mutate.
	cfg := b.opts.Cfg
	if fresh, err := config.LoadFull(); err == nil {
		cfg = fresh
		b.opts.Providers.SetAPIKeys(cfg.APIKeys)
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	s, ok := b.sessions[string(sid)]
	if !ok {
		return acpsdk.SetSessionConfigOptionResponse{}, fmt.Errorf("session %s not found", sid)
	}
	if err := b.applyConfigOption(s, &cfg, configID, value); err != nil {
		return acpsdk.SetSessionConfigOptionResponse{}, err
	}
	return acpsdk.SetSessionConfigOptionResponse{
		ConfigOptions: buildConfigOptions(s, &cfg, b.opts.Providers),
	}, nil
}

// ensureMediaDir creates the session's media directory for decoded image
// attachments.
func ensureMediaDir(dir string) error {
	return os.MkdirAll(dir, 0o755)
}
