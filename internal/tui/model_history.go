package tui

import (
	"context"
	"fmt"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"

	"spettro/internal/agent"
	"spettro/internal/compact"
	"spettro/internal/config"
	"spettro/internal/provider"
	"spettro/internal/session"
)

// buildConversationHistory renders a bounded, oldest-first transcript of prior
// turns for the model (EFF-2). It includes user and assistant turns plus any
// /compact summary, and excludes the trailing user message when present (that
// is the current request, sent separately as the task — including it here would
// duplicate it). The result is capped at maxConversationHistoryBytes with
// most-recent turns winning, so token cost stays bounded. Returns "" when there
// is no prior context (first turn), preserving the pre-EFF-2 behavior exactly.
func (m Model) buildConversationHistory() string {
	msgs := m.messages
	// Drop the trailing user turn: it is the request being sent as the task.
	if n := len(msgs); n > 0 && msgs[n-1].Role == RoleUser {
		msgs = msgs[:n-1]
	}

	// Collect eligible turns as formatted lines, oldest-first.
	var lines []string
	for _, msg := range msgs {
		content := strings.TrimSpace(msg.Content)
		if content == "" {
			continue
		}
		switch msg.Role {
		case RoleUser:
			lines = append(lines, "user: "+singleLineHistory(content))
		case RoleAssistant:
			// Skip transient progress comments; keep substantive replies/plans.
			if msg.Kind == "comment" {
				continue
			}
			lines = append(lines, "assistant: "+singleLineHistory(content))
		case RoleSystem:
			// Only carry forward a compaction summary, not routine notices.
			if strings.HasPrefix(content, compactSummaryPrefix) {
				lines = append(lines, "summary: "+singleLineHistory(content))
			}
		}
	}
	if len(lines) == 0 {
		return ""
	}

	// Keep the most recent turns within the byte cap (oldest dropped first).
	kept := make([]string, 0, len(lines))
	total := 0
	for _, line := range slices.Backward(lines) {
		size := len(line) + 1 // +1 for the joining newline
		if total+size > maxConversationHistoryBytes && len(kept) > 0 {
			break
		}
		kept = append(kept, line)
		total += size
	}
	// kept is most-recent-first; reverse to oldest-first for the prompt.
	for l, r := 0, len(kept)-1; l < r; l, r = l+1, r-1 {
		kept[l], kept[r] = kept[r], kept[l]
	}
	return strings.Join(kept, "\n")
}

// compactedHistorySeed builds the structured conversation that replaces the
// carried history after a /compact: a user turn holding the summary and a
// short assistant acknowledgment. It starts with a user turn (a provider
// requirement) and gives every subsequent turn a stable prefix to extend, so
// prompt caching resumes immediately after the one unavoidable compaction miss.
func compactedHistorySeed(summary string) []provider.Message {
	summary = strings.TrimSpace(summary)
	if summary == "" {
		return nil
	}
	return []provider.Message{
		{Role: provider.RoleUser, Content: "Context from the conversation so far (compacted summary):\n" + summary},
		{Role: provider.RoleAssistant, Content: "Understood. I have the compacted context and will continue from there."},
	}
}

// singleLineHistory collapses a turn to a single line so the transcript stays
// compact and unambiguous in the prompt. Very long turns are truncated to keep
// any single entry from dominating the budget.
func singleLineHistory(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	const maxPerTurn = 4000
	if len(s) > maxPerTurn {
		s = s[:maxPerTurn] + " …(truncated)"
	}
	return s
}

func (m Model) runAgent(spec config.AgentSpec, input string, mentionedFiles []string, images []string) (tea.Model, tea.Cmd) {
	return m.runAgentApproved(spec, input, mentionedFiles, images, false)
}

func (m Model) runAgentApproved(spec config.AgentSpec, input string, mentionedFiles []string, images []string, approved bool) (tea.Model, tea.Cmd) {
	m.thinking = true
	m.beginRunIndicator()
	m.activeAgentID = spec.ID
	m.publishRemoteState("agent_start")
	m.refreshModifiedFiles()
	m.liveTools = nil
	m.currentTool = nil
	m.pendingAuth = nil
	m.progressNote = fmt.Sprintf("Okay, let me work on that with the %s agent.", spec.ID)
	m.activePrompt = &queuedPrompt{
		Input:          input,
		Prompt:         input,
		MentionedFiles: append([]string(nil), mentionedFiles...),
		Images:         append([]string(nil), images...),
	}
	m.startAgentActivity(spec.ID, input)
	// Mid-run steering: reuse the Model's queue so a goal run's iterations all
	// share it (guidance typed between iterations reaches the next one).
	if m.steering == nil {
		m.steering = agent.NewSteeringQueue()
	}
	toolCh := make(chan agent.ToolTrace, 64)
	m.toolCh = toolCh
	streamCh := make(chan agent.StreamChunk, 256)
	m.streamCh = streamCh
	usageCh := make(chan agent.UsageEvent, 16)
	m.usageCh = usageCh
	m.liveRunTokens = 0
	approvalCh := make(chan shellApprovalRequestMsg, 8)
	m.approvalCh = approvalCh
	askUserCh := make(chan askUserRequestMsg, 4)
	m.askUserCh = askUserCh
	ctx, cancel := context.WithCancel(context.Background())
	m.cancelAgent = cancel
	m.ensureSession()
	pm := m.providers
	providerName := m.cfg.ActiveProvider
	modelName := m.cfg.ActiveModel
	cwd := m.cwd
	store := m.store
	perm := m.cfg.Permission
	agentID := spec.ID

	manifest := m.manifest
	// Structured cross-turn history: the previous run's conversation is passed
	// back verbatim so the provider sees a byte-stable, growing prefix (prompt
	// caching) and keeps every tool call/result. The flattened transcript is
	// only the degraded fallback for the first turn after resuming a session,
	// where no structured context exists yet.
	convHistory := m.convHistory
	history := ""
	if len(convHistory) == 0 {
		history = m.buildConversationHistory()
	}
	// Checkpointing: before any file-modifying tool executes, the
	// runtime calls back so the working tree is committed to the shadow repo
	// together with the conversation as it stood when this run started. The
	// snapshot blob is captured now — the model value is immutable during the
	// run — and the checkpointer itself is thread-safe. The runtime also
	// prepares each step's snapshot while the model generates and claims it
	// at the first mutating call (checkpointPrepare).
	var checkpointFn func(string)
	var checkpointPrepare func() agent.PreparedCheckpoint
	if cp := m.ensureCheckpointer(); cp != nil {
		run := runCheckpoints{cp: cp, prompt: input, conversation: m.conversationSnapshot()}
		checkpointFn = run.snapshot
		checkpointPrepare = run.prepare
	}
	// Live permission: consulted before every approval decision so a
	// /permission change while this run executes applies immediately. It
	// mirrors the run-start rule below: a user level other than ask-first
	// overrides the agent spec's own permission, ask-first defers to it.
	var permissionFn func() config.PermissionLevel
	if live := m.livePerm; live != nil {
		specPerm := spec.Permission
		permissionFn = func() config.PermissionLevel {
			if p := live.get(); p != "" && p != config.PermissionAskFirst {
				return p
			}
			return specPerm
		}
	}
	// Goal iterations are step-bounded so the run yields back to advanceGoal
	// for the progress check; ordinary runs stay unlimited (0).
	maxSteps := 0
	if m.activeGoal != nil {
		maxSteps = resolveGoalIterationSteps(m.cfg)
	}
	a := agent.LLMAgent{
		Spec:            spec,
		ProviderManager: pm,
		ProviderName:    func() string { return providerName },
		ModelName:       func() string { return modelName },
		CWD:             cwd,
		MaxTokens:       m.cfg.TokenBudget,
		MaxOutputTokens: m.cfg.MaxOutputTokens,
		Thinking:        pm.ConfiguredThinking(providerName, modelName, m.cfg.ThinkingLevel),
		Ultra:           m.cfg.UltraActive(),
		RequiredReads:   mentionedFiles,
		Images:          images,
		History:         history,
		Messages:        convHistory,
		Checkpoint:      checkpointFn,
		Manifest:        &manifest,
		SandboxState:    m.sandboxState,
		SessionDir:      session.SessionDir(store.GlobalDir, m.sessionID),
		DelegationDepth: 0,
		// Goal-mode fields: set when a goal is active so the runtime uses
		// generous tool timeouts and recognizes goal-complete.
		GoalMode:        m.activeGoal != nil,
		MaxSteps:        maxSteps,
		ContextWindow:   resolveGoalContextWindow(m),
		Compact:         m.cfg.CompactConfig(),
		ShellTimeoutSec: m.cfg.GoalShellTimeoutSec,
		Steering:        m.steering,
		PermissionFn:    permissionFn,
		ToolCallback: func(t agent.ToolTrace) {
			// Guard the send against a cancelled run: after stopAgent() the TUI
			// stops draining toolCh, so an unguarded send from an in-flight
			// step could block the agent goroutine forever once the 64-slot
			// buffer fills.
			select {
			case toolCh <- t:
			case <-ctx.Done():
			}
		},
		StreamCallback: func(c agent.StreamChunk) {
			// Same cancellation guard as ToolCallback: never block the agent
			// goroutine on a stream send once the TUI stops draining.
			select {
			case streamCh <- c:
			case <-ctx.Done():
			}
		},
		UsageCallback: func(ev agent.UsageEvent) {
			select {
			case usageCh <- ev:
			case <-ctx.Done():
			}
		},
		ShellApproval: func(ctx context.Context, req agent.ShellApprovalRequest) (agent.ShellApprovalDecision, error) {
			respCh := make(chan shellApprovalResponse, 1)
			select {
			case approvalCh <- shellApprovalRequestMsg{request: req, response: respCh}:
			case <-ctx.Done():
				return agent.ShellApprovalDeny, ctx.Err()
			}
			select {
			case resp := <-respCh:
				if resp.err != nil {
					return agent.ShellApprovalDeny, resp.err
				}
				return resp.decision, nil
			case <-ctx.Done():
				return agent.ShellApprovalDeny, ctx.Err()
			}
		},
		// The whole form goes to the modal at once: it renders every question
		// as a tab and sends one answer per question back (dialog_question.go).
		AskUser: func(ctx context.Context, form agent.AskUserForm) ([]agent.AskUserAnswer, error) {
			respCh := make(chan askUserResponse, 1)
			select {
			case askUserCh <- askUserRequestMsg{form: form, response: respCh}:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			select {
			case resp := <-respCh:
				return resp.answers, resp.err
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		},
	}
	a.CheckpointPrepare = checkpointPrepare

	return m, tea.Batch(
		m.spin.Tick,
		waitForTool(toolCh),
		waitForStream(streamCh),
		waitForUsage(usageCh),
		waitForShellApproval(approvalCh),
		waitForAskUser(askUserCh),
		func() tea.Msg {
			runSpec := spec
			if approved || perm != config.PermissionAskFirst {
				if perm != config.PermissionAskFirst {
					runSpec.Permission = perm
				}
			}
			a.Spec = runSpec
			result, err := a.Run(ctx, input)
			close(toolCh)
			close(streamCh)
			close(usageCh)
			close(approvalCh)
			close(askUserCh)
			if err != nil {
				return agentDoneMsg{err: err}
			}
			if agentID == "plan" || spec.Mode == "planning" {
				_ = store.WriteProjectFile("PLAN.md", result.Content)
				return planDoneMsg{plan: result.Content, tools: result.Tools, tokensUsed: result.TokensUsed, contextTokens: result.ContextTokens, messages: result.Messages}
			}
			return agentDoneMsg{content: result.Content, tools: result.Tools, tokensUsed: result.TokensUsed, contextTokens: result.ContextTokens, meta: "", goalComplete: result.GoalComplete, goalSummary: result.GoalSummary, messages: result.Messages}
		},
	)
}

func (m Model) runCompact(focus string) (tea.Model, tea.Cmd) {
	return m.runCompactWithMode(focus, false)
}

func (m Model) runCompactWithMode(focus string, auto bool) (tea.Model, tea.Cmd) {
	if len(m.messages) == 0 {
		m.showBanner("nothing to compact", "info")
		return m, nil
	}
	m.thinking = true
	m.beginRunIndicator()
	m.autoCompactInFlight = auto
	pm := m.providers
	providerName := m.cfg.ActiveProvider
	modelName := m.cfg.ActiveModel
	ctx, cancel := context.WithCancel(context.Background())
	m.cancelAgent = cancel
	if len(m.convHistory) > 0 {
		// The structured history the model actually sees is compacted with
		// the same core as in-run compaction: the original task, the latest
		// user messages and the most recent tool calls stay verbatim, old
		// tool outputs are stubbed, and the rest becomes a structured summary.
		history := m.convHistory
		params := compact.Params{Window: resolveGoalContextWindow(m), Force: true, Focus: focus}
		if auto {
			params = m.autoCompactParams(history, focus)
		}
		return m, tea.Batch(
			m.spin.Tick,
			func() tea.Msg {
				return runStructuredCompact(ctx, pm, providerName, modelName, history, params)
			},
		)
	}
	// No structured history yet (the first turn after resuming a session):
	// summarize the visible transcript.
	var sb strings.Builder
	for _, msg := range m.messages {
		if msg.Role == RoleSystem {
			continue
		}
		sb.WriteString(string(msg.Role))
		sb.WriteString(": ")
		sb.WriteString(msg.Content)
		sb.WriteString("\n\n")
	}
	transcript := sb.String()
	return m, tea.Batch(
		m.spin.Tick,
		func() tea.Msg {
			compactPrompt := "Summarize the following conversation concisely, preserving all key decisions, facts, code snippets, and action items. Output only the summary, no preamble."
			if focus != "" {
				compactPrompt += " Focus especially on: " + focus + "."
			}
			resp, err := pm.Send(ctx, providerName, modelName, provider.Request{
				Prompt: compactPrompt + "\n\n" + transcript,
			})
			if err != nil {
				return compactDoneMsg{err: err}
			}
			return compactDoneMsg{summary: resp.Content}
		},
	)
}

// runStructuredCompact compacts the carried structured history with p (an
// explicit /compact forces a summary; auto-compaction goes cheapest first) and
// reports the result. A history too short to shrink is reported as a no-op,
// not a failure.
func runStructuredCompact(ctx context.Context, pm *provider.Manager, providerName, modelName string, history []provider.Message, p compact.Params) compactDoneMsg {
	send := func(ctx context.Context, req provider.Request) (provider.Response, error) {
		return pm.Send(ctx, providerName, modelName, req)
	}
	res, err := compact.Compact(ctx, send, history, p)
	if err != nil {
		return compactDoneMsg{err: err}
	}
	if !res.Compacted() {
		return compactDoneMsg{noop: true}
	}
	summary := res.Summary
	if summary == "" {
		summary = fmt.Sprintf("%d old tool outputs were replaced by re-readable stubs.", res.Pruned)
	}
	return compactDoneMsg{summary: summary, messages: res.Messages}
}

// autoCompactParams configures an automatic compaction of history. Unlike
// /compact it is not forced: it prunes old tool outputs before paying for a
// summary, and skips the summarizer when summarizing would free too little
// (the pressure then comes from the system prompt and tool schemas). The
// measure adds that overhead, the part of the last run's reported occupancy
// the history does not account for, so Compact sees the same pressure that
// fired the trigger.
func (m Model) autoCompactParams(history []provider.Message, focus string) compact.Params {
	overhead := max(m.contextTokens-compact.EstimateHistoryTokens("", history), 0)
	return compact.Params{
		Window: resolveGoalContextWindow(m),
		Policy: compact.Config{
			AutoEnabled:      m.cfg.AutoCompactEnabled,
			AutoThresholdPct: m.cfg.AutoCompactThresholdPct,
			MaxFailures:      m.cfg.AutoCompactMaxFailures,
		},
		Failures: m.autoCompactFailures,
		Measure: func(system string, msgs []provider.Message) int {
			return compact.EstimateHistoryTokens(system, msgs) + overhead
		},
		Focus: focus,
	}
}

func (m Model) runInit() (tea.Model, tea.Cmd) {
	// "docs" agent is read-only (no file-write); use "coding" so the file actually gets written.
	spec, ok := m.manifest.AgentByID("coding")
	if !ok {
		spec, ok = m.manifest.AgentByID("code")
		if !ok {
			m.showBanner("coding/code agent not found in manifest", "error")
			return m, nil
		}
	}
	return m.runAgent(spec, initTask, nil, nil)
}

// initTask is the /init prompt. The instruction file it produces is loaded
// into every future session's system prompt (capped at 16 KiB), so it asks for
// the facts an agent can't cheaply rediscover, kept short, for any project.
// Every AGENTS.md, CLAUDE.md and SPETTRO.md is loaded, so when the repo already
// has one it is updated in place rather than restated in a second file.
const initTask = `Analyze this codebase and write the project instruction file future agent sessions load into their context: write what a new engineer (or agent) needs to work here effectively, and keep it concise (well under 200 lines).

Which file: if the repository root already has AGENTS.md, CLAUDE.md or SPETTRO.md, read it and update that file in place, improving on it rather than starting over; do not create a second file that restates it, since every one of these files is loaded into every session. If several exist, update SPETTRO.md if present, otherwise AGENTS.md (when CLAUDE.md is a symlink to or copy of AGENTS.md, edit only AGENTS.md). Only when none exists, create SPETTRO.md.

Use glob, grep, file-read, and ls to explore first: the README, build files (Makefile, package.json, go.mod, pyproject.toml, Cargo.toml, ...), CI config, and the main source directories.

The file should contain:
- **Project overview**: what the project does, in 2–3 sentences
- **Build, test & lint**: the exact commands, including how to run a single test
- **Architecture**: key directories/packages/modules and their roles, one line each, and how the main pieces fit together
- **Entry points**: main binaries or apps and the startup flow
- **Configuration**: where config comes from and the settings that matter
- **Conventions**: code style, naming, error handling, testing patterns, and anything non-obvious a contributor must follow

Only include facts you verified in the code; skip sections that don't apply, and don't pad with generic advice. When updating an existing file, keep its structure and any rules the maintainers wrote.

CRITICAL: You MUST write the result to disk: edit the existing instruction file, or, when there is none, use file-write at path "SPETTRO.md" in the repository root. Do not just output the content — the file must exist after you finish.`
