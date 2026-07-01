package acp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

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
// owning bridge's mu; a session runs at most one prompt turn at a time (the
// SDK cancels the previous turn's context when a new prompt arrives).
type acpSession struct {
	id       string
	cwd      string
	agentID  string
	manifest config.AgentManifest
	mediaDir string
	// history is the bounded oldest-first transcript of prior turns, in the
	// same "user:/assistant:" line format the TUI feeds LLMAgent.History.
	history []string
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
			Title:   acpsdk.Ptr("Spettro"),
			Version: version.App,
		},
		AgentCapabilities: acpsdk.AgentCapabilities{
			LoadSession: false,
			PromptCapabilities: acpsdk.PromptCapabilities{
				Image:           true,
				EmbeddedContext: true,
			},
		},
		AuthMethods: []acpsdk.AuthMethod{},
	}, nil
}

// Authenticate is a no-op: Spettro reads provider API keys from its own
// config, so no ACP-level auth methods are advertised.
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
		id:       sid,
		cwd:      cwd,
		agentID:  agentID,
		manifest: manifest,
		mediaDir: filepath.Join(session.SessionDir(b.opts.GlobalDir, sid), "acp-media"),
	}
	b.mu.Lock()
	b.sessions[sid] = s
	b.mu.Unlock()

	b.announceCommands(ctx, acpsdk.SessionId(sid))

	return acpsdk.NewSessionResponse{
		SessionId: acpsdk.SessionId(sid),
		Modes:     sessionModes(manifest, agentID),
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

// Cancel is a no-op: the SDK already cancels the in-flight Prompt context for
// the session when the client sends session/cancel.
func (b *bridge) Cancel(_ context.Context, _ acpsdk.CancelNotification) error {
	return nil
}

func (b *bridge) Prompt(ctx context.Context, params acpsdk.PromptRequest) (acpsdk.PromptResponse, error) {
	b.mu.Lock()
	s, ok := b.sessions[string(params.SessionId)]
	b.mu.Unlock()
	if !ok {
		return acpsdk.PromptResponse{}, fmt.Errorf("session %s not found", params.SessionId)
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

	if strings.HasPrefix(trimmedTask, "/") {
		b.mu.Lock()
		reply, modeChanged, handled := handleSlashCommand(s, &cfg, trimmedTask)
		b.mu.Unlock()
		if handled {
			if reply != "" {
				_ = b.conn.SessionUpdate(ctx, acpsdk.SessionNotification{
					SessionId: params.SessionId,
					Update:    acpsdk.UpdateAgentMessageText(reply),
				})
			}
			if modeChanged {
				_ = b.conn.SessionUpdate(ctx, acpsdk.SessionNotification{
					SessionId: params.SessionId,
					Update: acpsdk.SessionUpdate{CurrentModeUpdate: &acpsdk.SessionCurrentModeUpdate{
						CurrentModeId: acpsdk.SessionModeId(s.agentID),
					}},
				})
			}
			return acpsdk.PromptResponse{StopReason: acpsdk.StopReasonEndTurn}, nil
		}
	}

	b.mu.Lock()
	agentID := s.agentID
	manifest := s.manifest
	history := strings.Join(s.history, "\n")
	b.mu.Unlock()

	spec, ok := manifest.AgentByID(agentID)
	if !ok {
		return acpsdk.PromptResponse{}, fmt.Errorf("agent not found: %s", agentID)
	}
	spec.Permission = cfg.Permission

	turn := &turnState{
		bridge:    b,
		ctx:       ctx,
		sessionID: params.SessionId,
		open:      make(map[string][]acpsdk.ToolCallId),
	}

	thinking := provider.ThinkingLevel("")
	if b.opts.Providers.SupportsReasoning(cfg.ActiveProvider, cfg.ActiveModel) {
		thinking = provider.ThinkingLevel(cfg.ThinkingLevel)
	}

	ag := agent.LLMAgent{
		Spec:            spec,
		ProviderManager: b.opts.Providers,
		ProviderName:    func() string { return cfg.ActiveProvider },
		ModelName:       func() string { return cfg.ActiveModel },
		CWD:             s.cwd,
		MaxTokens:       cfg.TokenBudget,
		Thinking:        thinking,
		RequiredReads:   mentioned,
		Images:          images,
		History:         history,
		Manifest:        &manifest,
		SandboxState:    b.opts.SandboxState,
		SessionDir:      session.SessionDir(b.opts.GlobalDir, s.id),
		StreamCallback:  turn.onStream,
		ToolCallback:    turn.onTool,
		ShellApproval: func(sctx context.Context, ar agent.ShellApprovalRequest) (agent.ShellApprovalDecision, error) {
			if cfg.Permission == config.PermissionYOLO {
				return agent.ShellApprovalAllowOnce, nil
			}
			return turn.requestShellApproval(sctx, ar)
		},
		AskUser: turn.askUser,
	}

	result, runErr := ag.Run(ctx, task)
	if runErr != nil {
		if ctx.Err() != nil {
			return acpsdk.PromptResponse{StopReason: acpsdk.StopReasonCancelled}, nil
		}
		return acpsdk.PromptResponse{}, runErr
	}

	// The answer is sent once from the authoritative final content; see
	// turnState.onStream for why answer deltas are not streamed live.
	if result.Content != "" {
		turn.sessionUpdate(acpsdk.UpdateAgentMessageText(result.Content))
	}

	b.mu.Lock()
	s.appendHistory("user: " + singleLineHistory(task))
	if result.Content != "" {
		s.appendHistory("assistant: " + singleLineHistory(result.Content))
	}
	b.mu.Unlock()

	return acpsdk.PromptResponse{
		StopReason: acpsdk.StopReasonEndTurn,
		Meta:       map[string]any{"spettro.dev/tokensUsed": result.TokensUsed},
	}, nil
}

// sessionModes converts the manifest's enabled agents into the ACP session
// mode list, so editors surface Spettro's agent roster in their mode picker.
func sessionModes(manifest config.AgentManifest, current string) *acpsdk.SessionModeState {
	agents := manifest.EnabledAgents()
	if len(agents) == 0 {
		return nil
	}
	modes := make([]acpsdk.SessionMode, 0, len(agents))
	for _, a := range agents {
		mode := acpsdk.SessionMode{
			Id:   acpsdk.SessionModeId(a.ID),
			Name: a.Name,
		}
		if a.Description != "" {
			mode.Description = acpsdk.Ptr(a.Description)
		}
		modes = append(modes, mode)
	}
	return &acpsdk.SessionModeState{
		AvailableModes: modes,
		CurrentModeId:  acpsdk.SessionModeId(current),
	}
}

// requestShellApproval bridges Spettro's shell approval flow to ACP's
// session/request_permission, letting the editor render its native prompt.
func (t *turnState) requestShellApproval(ctx context.Context, ar agent.ShellApprovalRequest) (agent.ShellApprovalDecision, error) {
	title := "Run shell command: " + ar.Command
	update := acpsdk.ToolCallUpdate{
		ToolCallId: t.openToolCallID(ar.ToolID),
		Title:      acpsdk.Ptr(title),
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
			Title:      acpsdk.Ptr(title),
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

func (b *bridge) ListSessions(_ context.Context, _ acpsdk.ListSessionsRequest) (acpsdk.ListSessionsResponse, error) {
	return acpsdk.ListSessionsResponse{}, acpsdk.NewMethodNotFound(acpsdk.AgentMethodSessionList)
}

func (b *bridge) ResumeSession(_ context.Context, _ acpsdk.ResumeSessionRequest) (acpsdk.ResumeSessionResponse, error) {
	return acpsdk.ResumeSessionResponse{}, acpsdk.NewMethodNotFound(acpsdk.AgentMethodSessionResume)
}

func (b *bridge) SetSessionConfigOption(_ context.Context, _ acpsdk.SetSessionConfigOptionRequest) (acpsdk.SetSessionConfigOptionResponse, error) {
	return acpsdk.SetSessionConfigOptionResponse{}, acpsdk.NewMethodNotFound(acpsdk.AgentMethodSessionSetConfigOption)
}

// maxHistoryBytes mirrors the TUI's maxConversationHistoryBytes: the bounded
// cross-turn transcript fed back to the model, most-recent turns winning.
const maxHistoryBytes = 32 * 1024

// appendHistory adds one formatted turn line and evicts oldest lines past the
// byte cap. Caller holds the bridge mutex.
func (s *acpSession) appendHistory(line string) {
	s.history = append(s.history, line)
	total := 0
	for _, l := range s.history {
		total += len(l) + 1
	}
	for total > maxHistoryBytes && len(s.history) > 1 {
		total -= len(s.history[0]) + 1
		s.history = s.history[1:]
	}
}

// singleLineHistory collapses a turn to a single bounded line, matching the
// TUI's transcript format so prompts look identical across front-ends.
func singleLineHistory(v string) string {
	v = strings.Join(strings.Fields(v), " ")
	const maxPerTurn = 4000
	if len(v) > maxPerTurn {
		v = v[:maxPerTurn] + " …(truncated)"
	}
	return v
}

// ensureMediaDir creates the session's media directory for decoded image
// attachments.
func ensureMediaDir(dir string) error {
	return os.MkdirAll(dir, 0o755)
}
