package acp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	acpsdk "github.com/coder/acp-go-sdk"

	"spettro/internal/agent"
	"spettro/internal/config"
	"spettro/internal/provider"
	"spettro/internal/session"
	"spettro/internal/skills"
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

	// clientCaps / clientExtensions are what the client declared at
	// initialize: its core capabilities (elicitation in particular) and the
	// `_spettro/*` methods it serves. Both gate the agent-question transports
	// in question.go.
	clientCaps       acpsdk.ClientCapabilities
	clientExtensions map[string]bool
	// questionTransport overrides the client request surface used by agent
	// questions. Nil in production (the SDK connection is used); tests set it.
	questionTransport questionTransport

	// logins holds the in-flight Spettro Subscription device-flow login, if
	// any. It is connection-scoped rather than session-scoped: signing in is
	// an account-level act that every session on this bridge observes.
	logins loginRegistry

	// lateModels starts, at most once, the goroutine that re-sends config
	// options when model discovery finishes after a session response gave
	// up waiting for it (see awaitModels).
	lateModels sync.Once
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
	// storedGoal and storedStats are metadata a session loaded from the
	// store carried that ACP does not manage itself: the TUI's /goal record
	// (its /resume offers to continue an unfinished goal) and the /stats
	// usage counters. persistState writes them back unchanged, so
	// continuing a TUI session from an editor does not erase them.
	storedGoal  *session.GoalRecord
	storedStats *provider.SessionUsage
	// commandsAnnounced records that a prompt turn has re-sent the available
	// commands list, the fallback for clients that dropped the initial
	// announcement (see NewSession).
	commandsAnnounced bool
	// lastGoal is the outcome summary of the most recent /goal run, surfaced
	// by /goal status.
	lastGoal string
	// lastLoop is the outcome summary of the most recent /loop run, surfaced
	// by /loop status.
	lastLoop string
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
	// Record what the client can do before any capability gating: agent
	// questions pick their transport from the extension surface the client
	// mirrors back and from its elicitation capability (see question.go).
	b.mu.Lock()
	b.clientCaps = params.ClientCapabilities
	b.clientExtensions = parseClientExtensions(params.Meta)
	b.mu.Unlock()

	return acpsdk.InitializeResponse{
		ProtocolVersion: acpsdk.ProtocolVersionNumber,
		// Advertise the `_spettro/*` extension surface (see ext.go) so a
		// native client can detect it at handshake and fall back to
		// "configure this in the TUI" against an older CLI instead of
		// calling methods that would come back method-not-found.
		// `clientMethods` is the other direction: methods this agent will
		// call on a client that mirrors them back.
		Meta: map[string]any{
			metaExtensionsKey: map[string]any{
				"version":       extensionsVersion,
				"methods":       extensionMethods,
				"clientMethods": extensionClientMethods,
			},
		},
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
				Close:  &acpsdk.SessionCloseCapabilities{},
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

// projectManifest loads the agent manifest of the project a session runs
// in, which may not be the process's own. A manifest that cannot be loaded
// is the client's error to see: falling back to the process manifest
// would run the session under another project's agents, rules and sandbox,
// possibly wider ones. (A migration that merely cannot be written back is
// not an error; see config.LoadAgentManifestForProject.)
func (b *bridge) projectManifest(cwd string) (config.AgentManifest, error) {
	manifest, err := config.LoadAgentManifestForProject(cwd)
	if err != nil {
		return config.AgentManifest{}, acpsdk.NewInternalError(map[string]any{"error": fmt.Sprintf("agent manifest of %s: %v", cwd, err)})
	}
	return manifest, nil
}

func (b *bridge) NewSession(ctx context.Context, params acpsdk.NewSessionRequest) (acpsdk.NewSessionResponse, error) {
	cwd := params.Cwd
	if cwd == "" {
		cwd = b.opts.CWD
	}
	if !filepath.IsAbs(cwd) {
		return acpsdk.NewSessionResponse{}, acpsdk.NewInvalidParams(map[string]any{"error": "cwd must be an absolute path"})
	}

	manifest, err := b.projectManifest(cwd)
	if err != nil {
		return acpsdk.NewSessionResponse{}, err
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
	// selectors reflect the current model/permission (mirrors Prompt), and
	// give the background model discovery a bounded chance to finish.
	b.awaitModels(ctx)
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
		return acpsdk.SetSessionModeResponse{}, errSessionNotFound(params.SessionId)
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
//
// shown, when not empty, is what the transcript records instead of task: the
// "/<skill> args" or $mention the user sent, where task carries the skill's
// instructions (see shownTask in Prompt).
func (b *bridge) steerRunningTurn(ctx context.Context, s *acpSession, sessionID acpsdk.SessionId, task, shown string) (acpsdk.PromptResponse, error) {
	if shown == "" {
		shown = task
	}
	b.mu.Lock()
	q := s.steering
	// Record the steering text in the flat transcript now; the structured
	// history picks it up from the running turn's RunResult.Messages.
	s.transcript = append(s.transcript, session.Message{Role: "user", Content: shown, At: time.Now()})
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
		return acpsdk.PromptResponse{}, errSessionNotFound(params.SessionId)
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

	content, err := readPromptContent(params.Prompt, s.mediaDir)
	if err != nil {
		return acpsdk.PromptResponse{}, acpsdk.NewInvalidParams(map[string]any{"error": err.Error()})
	}
	task, images, mentioned := content.task(), content.images, content.mentioned
	trimmedTask := strings.TrimSpace(task)
	// typed is the text the user typed, without the files the editor
	// attached. Skill commands and $mentions are parsed from it alone: an
	// attached file is context for the turn, not the skill's arguments,
	// and a $word inside it is not a mention.
	typed := strings.TrimSpace(content.typed)
	if trimmedTask == "" {
		return acpsdk.PromptResponse{}, acpsdk.NewInvalidParams(map[string]any{"error": "prompt has no text content"})
	}

	turn := &turnState{
		bridge:    b,
		ctx:       ctx,
		sessionID: params.SessionId,
		cwd:       s.cwd,
	}
	// shownTask, when set, is what the transcript records as the user's
	// message instead of task: a skill invocation or $mention sends the
	// skill's instructions to the model, but the transcript (replayed on
	// session/load) keeps what the user sent. This holds for a prompt that
	// becomes steering of a running turn too (see steerRunningTurn).
	shownTask := ""

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
			typed = strings.TrimSpace(strings.TrimPrefix(typed, "/plan"))
			// $skill mentions in the task load those skills, as in any
			// other prompt.
			if section, names := skills.MentionInstructions(typed, agent.SkillCatalogFor(s.cwd, cfg)); len(names) > 0 {
				shownTask = task
				task += section
			}
		} else if rewritten, ok := acpWorkflowRunPrompt(s.cwd, trimmedTask); ok {
			// /workflows run <name> becomes an ordinary turn instructing the
			// agent to invoke the saved script, so the model reviews and acts
			// on the result exactly as it would for one it wrote itself.
			trimmedTask = rewritten
			task = rewritten
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
					strings.TrimSpace(strings.TrimPrefix(trimmedTask, "/goal")), "")
			}
			defer finish()
			turn.ctx = runCtx
			b.ensureContextHeadroom(runCtx, s, &cfg, turn)
			return b.runGoalCommand(runCtx, s, &cfg, turn, trimmedTask)
		} else if fields[0] == "/loop" {
			if strings.TrimSpace(strings.TrimPrefix(trimmedTask, "/loop")) == "stop" {
				// /loop stop while a loop turn is running: cancel that run.
				b.mu.Lock()
				cancel := s.runCancel
				b.mu.Unlock()
				if cancel != nil {
					cancel()
					_ = b.conn.SessionUpdate(ctx, acpsdk.SessionNotification{
						SessionId: params.SessionId,
						Update:    acpsdk.UpdateAgentMessageText("loop stopped"),
					})
					return acpsdk.PromptResponse{StopReason: acpsdk.StopReasonEndTurn}, nil
				}
			}
			runCtx, finish, ok := b.beginRun(ctx, s)
			if !ok {
				// A loop/run is already in flight: treat "/loop <text>" sent
				// mid-turn as steering for it (minus the command prefix).
				return b.steerRunningTurn(ctx, s, params.SessionId,
					strings.TrimSpace(strings.TrimPrefix(trimmedTask, "/loop")), "")
			}
			defer finish()
			turn.ctx = runCtx
			b.ensureContextHeadroom(runCtx, s, &cfg, turn)
			return b.runLoopCommand(runCtx, s, &cfg, turn, trimmedTask)
		}
		if fields := strings.Fields(trimmedTask); fields[0] == "/compact" {
			return b.handleCompactCommand(ctx, s, &cfg, turn, trimmedTask)
		}
		b.mu.Lock()
		before := sharedSettings(&cfg)
		reply, modeChanged, handled := handleSlashCommand(s, &cfg, b.opts.Providers, trimmedTask)
		if !handled {
			reply, modeChanged, handled = handleExtendedSlashCommand(b, s, &cfg, b.opts.Providers, trimmedTask)
		}
		_ = modeChanged
		var options []acpsdk.SessionConfigOption
		if handled {
			options = buildConfigOptions(s, &cfg, b.opts.Providers)
		}
		sharedChanged := handled && sharedSettings(&cfg) != before
		b.mu.Unlock()
		if sharedChanged {
			b.syncOtherSessions(params.SessionId, cfg)
		}
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
		// Not one of Spettro's commands: "/<skill-name> [args]" runs that
		// skill; anything else goes to the model as typed.
		if prompt, errText, ok := resolveSkillCommand(s.cwd, cfg, typed); ok {
			if errText != "" {
				_ = b.conn.SessionUpdate(ctx, acpsdk.SessionNotification{
					SessionId: params.SessionId,
					Update:    acpsdk.UpdateAgentMessageText(errText),
				})
				return acpsdk.PromptResponse{StopReason: acpsdk.StopReasonEndTurn}, nil
			}
			shownTask = trimmedTask
			task = content.withContexts(prompt)
		}
	} else if section, names := skills.MentionInstructions(typed, agent.SkillCatalogFor(s.cwd, cfg)); len(names) > 0 {
		// $skill-name mentions pull those skills' instructions into the
		// prompt (Codex style).
		shownTask = task
		task += section
	}

	// Claim the session's run slot. If a turn is already executing, this
	// prompt becomes mid-run steering for it instead of a new turn (the SDK
	// has already cancelled the request context of the running turn, but the
	// agent runs under runCtx, so it is unaffected).
	runCtx, finish, ok := b.beginRun(ctx, s)
	if !ok {
		return b.steerRunningTurn(ctx, s, params.SessionId, task, shownTask)
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
	turn.manifest = manifest
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
	turn.agentID = spec.ID
	livePermission := func() config.PermissionLevel {
		b.mu.Lock()
		defer b.mu.Unlock()
		return s.permission
	}

	thinking := b.opts.Providers.ConfiguredThinking(cfg.ActiveProvider, cfg.ActiveModel, cfg.ThinkingLevel)

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
		MaxOutputTokens: cfg.MaxOutputTokens,
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
			return turn.requestApproval(sctx, ar)
		},
		// The whole form when the client can take one, question by question when
		// it cannot. See question_form.go for the ladder.
		AskUser: turn.askForm,
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
	if shownTask == "" {
		shownTask = task
	}
	s.transcript = append(s.transcript, session.Message{Role: "user", Content: shownTask, At: now})
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
		return promptFailure(runCtx, runErr, turnUsageResponse(turnUsage, result.TokensUsed))
	}

	// The answer is sent once from the authoritative final content; see
	// turnState.onStream for why answer deltas are not streamed live.
	if result.Content != "" {
		turn.sessionUpdate(acpsdk.UpdateAgentMessageText(result.Content))
	}

	return acpsdk.PromptResponse{
		StopReason: acpsdk.StopReasonEndTurn,
		Usage:      turnUsageResponse(turnUsage, result.TokensUsed),
		Meta:       map[string]any{"spettro.app/tokensUsed": result.TokensUsed},
	}, nil
}

// promptFailure turns a failed run into the prompt turn's outcome. Per the
// ACP spec a cancelled turn is not an error: it ends with the "cancelled"
// stop reason, whatever error the aborted provider call or tool produced. A
// reply the provider's content filter stopped ends with "refusal". Any other
// failure is returned as a JSON-RPC error carrying the runtime's message.
func promptFailure(runCtx context.Context, runErr error, usage *acpsdk.Usage) (acpsdk.PromptResponse, error) {
	switch {
	case runCtx.Err() != nil:
		return acpsdk.PromptResponse{StopReason: acpsdk.StopReasonCancelled, Usage: usage}, nil
	case errors.Is(runErr, agent.ErrContentFiltered):
		return acpsdk.PromptResponse{StopReason: acpsdk.StopReasonRefusal, Usage: usage}, nil
	}
	return acpsdk.PromptResponse{}, runErr
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

// Agent questions (the ask-user tool) live in question.go: turnState.askUser
// negotiates the extension / `_meta` / elicitation transports there.

// Unsupported optional capabilities.

func (b *bridge) Logout(_ context.Context, _ acpsdk.LogoutRequest) (acpsdk.LogoutResponse, error) {
	return acpsdk.LogoutResponse{}, acpsdk.NewMethodNotFound(acpsdk.AgentMethodLogout)
}

// CloseSession ends a session: its running turn, if any, is cancelled as by
// session/cancel (that turn still answers its session/prompt, with the
// "cancelled" stop reason, and still saves its transcript), and the session
// is dropped from this connection, freeing its in-memory history. The stored
// conversation stays on disk, so session/load or session/resume can bring it
// back later. Closing an unknown session is an error.
func (b *bridge) CloseSession(_ context.Context, params acpsdk.CloseSessionRequest) (acpsdk.CloseSessionResponse, error) {
	b.mu.Lock()
	s, ok := b.sessions[string(params.SessionId)]
	var cancel context.CancelFunc
	if ok {
		cancel = s.runCancel
		delete(b.sessions, string(params.SessionId))
	}
	b.mu.Unlock()
	if !ok {
		return acpsdk.CloseSessionResponse{}, errSessionNotFound(params.SessionId)
	}
	if cancel != nil {
		cancel()
	}
	return acpsdk.CloseSessionResponse{}, nil
}

// errSessionNotFound is the error for a request naming a session this
// connection does not hold (never created, or closed).
func errSessionNotFound(sid acpsdk.SessionId) error {
	return acpsdk.NewInvalidParams(map[string]any{"error": "session not found: " + string(sid)})
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
	s, ok := b.sessions[string(sid)]
	if !ok {
		b.mu.Unlock()
		return acpsdk.SetSessionConfigOptionResponse{}, errSessionNotFound(sid)
	}
	before := sharedSettings(&cfg)
	if err := b.applyConfigOption(s, &cfg, configID, value); err != nil {
		b.mu.Unlock()
		return acpsdk.SetSessionConfigOptionResponse{}, err
	}
	options := buildConfigOptions(s, &cfg, b.opts.Providers)
	sharedChanged := sharedSettings(&cfg) != before
	b.mu.Unlock()
	if sharedChanged {
		b.syncOtherSessions(sid, cfg)
	}
	return acpsdk.SetSessionConfigOptionResponse{ConfigOptions: options}, nil
}

// sharedSettings fingerprints the settings every session on the connection
// shares. Only the mode is per session; the model, permission level,
// thinking level and Ultra live in the user config, so a change made from
// one session applies to all of them (and to a TUI running alongside).
func sharedSettings(cfg *config.UserConfig) string {
	return strings.Join([]string{
		cfg.ActiveProvider, cfg.ActiveModel, string(cfg.Permission),
		cfg.ThinkingLevel, strconv.FormatBool(cfg.Ultra),
	}, "\x00")
}

// syncOtherSessions tells every session on this connection except the one
// that made the change about a change to the shared settings: each gets a
// config_option_update so the editor's selectors in every open session show
// the new model, permission and thinking level, and each session's live
// permission level moves to the new one, so a run in progress there applies
// it at its next approval decision exactly as its next turn would.
func (b *bridge) syncOtherSessions(except acpsdk.SessionId, cfg config.UserConfig) {
	type update struct {
		sid     acpsdk.SessionId
		options []acpsdk.SessionConfigOption
	}
	b.mu.Lock()
	var updates []update
	for id, s := range b.sessions {
		if id == string(except) {
			continue
		}
		if cfg.Permission != "" {
			s.permission = cfg.Permission
		}
		updates = append(updates, update{acpsdk.SessionId(id), buildConfigOptions(s, &cfg, b.opts.Providers)})
	}
	b.mu.Unlock()
	for _, u := range updates {
		_ = b.conn.SessionUpdate(context.Background(), acpsdk.SessionNotification{
			SessionId: u.sid,
			Update: acpsdk.SessionUpdate{ConfigOptionUpdate: &acpsdk.SessionConfigOptionUpdate{
				ConfigOptions: u.options,
			}},
		})
	}
}

// ensureMediaDir creates the session's media directory for decoded image
// attachments.
func ensureMediaDir(dir string) error {
	return os.MkdirAll(dir, 0o755)
}
