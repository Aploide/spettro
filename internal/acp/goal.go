package acp

import (
	"context"
	"fmt"
	"strings"
	"time"

	acpsdk "github.com/coder/acp-go-sdk"

	"spettro/internal/agent"
	"spettro/internal/config"
	"spettro/internal/provider"
	"spettro/internal/session"
)

// maxGoalRetries mirrors the TUI: consecutive hard run errors tolerated per
// goal before the loop gives up.
const maxGoalRetries = 2

// runGoalCommand implements /goal over ACP. Unlike the TUI, where the goal
// loop spans many agentDoneMsg round-trips, over ACP the whole autonomous
// loop runs inside this single prompt turn: iteration banners and tool calls
// stream as session updates, and the editor's stop/cancel button interrupts
// it through ctx. cfg is the fresh per-turn config snapshot from Prompt.
func (b *bridge) runGoalCommand(ctx context.Context, s *acpSession, cfg *config.UserConfig, turn *turnState, input string) (acpsdk.PromptResponse, error) {
	reply := func(text string) (acpsdk.PromptResponse, error) {
		turn.sessionUpdate(acpsdk.UpdateAgentMessageText(text))
		return acpsdk.PromptResponse{StopReason: acpsdk.StopReasonEndTurn}, nil
	}

	rest := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(input), "/goal"))
	switch strings.ToLower(rest) {
	case "":
		return reply("usage: /goal <objective>   (e.g. /goal update all dependencies)")
	case "status":
		b.mu.Lock()
		last := s.lastGoal
		b.mu.Unlock()
		if last == "" {
			return reply("no goal has run in this session")
		}
		return reply(last)
	case "stop", "resume":
		// A running goal never reaches here: Prompt intercepts "/goal stop"
		// while a turn is in flight and cancels it directly.
		return reply("no goal is running; over ACP a goal runs inside a single prompt turn — while it runs, /goal stop or the editor's stop/cancel interrupts it, and any other prompt steers it")
	}
	objective := rest

	b.mu.Lock()
	manifest := s.manifest
	cwd := s.cwd
	// Steering: prompts sent while this goal turn runs are queued by Prompt
	// and injected at the running iteration's next step boundary. The queue
	// is shared across iterations, so text arriving between iterations
	// reaches the next one.
	steering := s.steering
	// The goal loop starts from the session's carried conversation and then
	// threads each iteration's RunResult.Messages into the next, so every
	// iteration extends a byte-stable prompt prefix (cache hits) instead of
	// rediscovering the workspace from scratch.
	// Seed the live permission for this turn; /permission or a config-option
	// change while the goal runs overwrites it and takes effect at the next
	// approval decision.
	s.permission = cfg.Permission
	history := s.history
	b.mu.Unlock()

	livePermission := func() config.PermissionLevel {
		b.mu.Lock()
		defer b.mu.Unlock()
		return s.permission
	}

	spec, ok := manifest.AgentByID("coding")
	if !ok {
		return reply("coding agent not found in manifest")
	}
	spec.Permission = cfg.Permission
	spec.AllowedTools = appendUnique(spec.AllowedTools, "goal-complete")

	if cfg.Permission != config.PermissionYOLO {
		turn.sessionUpdate(acpsdk.UpdateAgentMessageText(fmt.Sprintf(
			"note: permission is %q, so goal mode will pause for approvals. For fully unattended runs use /permission yolo.\n",
			cfg.Permission)))
	}

	state := &agent.GoalState{
		Objective:       objective,
		StartedAt:       time.Now(),
		MaxIterations:   cfg.GoalMaxIterations,
		NoProgressLimit: goalNoProgressLimit(*cfg),
	}

	thinking := provider.ThinkingLevel("")
	if b.opts.Providers.SupportsReasoning(cfg.ActiveProvider, cfg.ActiveModel) {
		thinking = provider.ThinkingLevel(cfg.ThinkingLevel)
	}

	totalTokens := 0
	retries := 0
	finish := func(outcome string) (acpsdk.PromptResponse, error) {
		summary := fmt.Sprintf("goal: %s\noutcome: %s\niterations: %d, elapsed: %s",
			objective, outcome, state.Iteration, time.Since(state.StartedAt).Round(time.Second))
		b.mu.Lock()
		s.lastGoal = summary
		// Adopt the loop's final conversation so follow-up prompts in this
		// session keep the goal run's context and cache prefix.
		if len(history) > 0 {
			s.history = history
		}
		b.mu.Unlock()
		turn.sessionUpdate(acpsdk.UpdateAgentMessageText(outcome))
		return acpsdk.PromptResponse{
			StopReason: acpsdk.StopReasonEndTurn,
			Meta:       map[string]any{"spettro.dev/tokensUsed": totalTokens},
		}, nil
	}

	state.LastSignature = agent.WorkspaceSignature(cwd)
	for {
		if ctx.Err() != nil {
			return acpsdk.PromptResponse{StopReason: acpsdk.StopReasonCancelled}, nil
		}
		state.Iteration++
		turn.sessionUpdate(acpsdk.UpdateAgentMessageText(fmt.Sprintf(
			"↻ goal iteration %d — continuing autonomously toward: %s\n", state.Iteration, objective)))

		task := agent.GoalModePreamble + objective
		if state.Iteration > 1 {
			task = fmt.Sprintf(
				"%s%s\n\n(Continuing autonomously — iteration %d. Review what is already done in the workspace, then keep going. Call goal-complete only when the objective is fully met and verified.)",
				agent.GoalModePreamble, objective, state.Iteration)
		}

		ag := agent.LLMAgent{
			Spec:            spec,
			ProviderManager: b.opts.Providers,
			ProviderName:    func() string { return cfg.ActiveProvider },
			ModelName:       func() string { return cfg.ActiveModel },
			CWD:             cwd,
			MaxTokens:       cfg.TokenBudget,
			Thinking:        thinking,
			Messages:        history,
			Manifest:        &manifest,
			SandboxState:    b.opts.SandboxState,
			SessionDir:      session.SessionDir(b.opts.GlobalDir, s.id),
			GoalMode:        true,
			ContextWindow:   b.opts.Providers.ModelContext(cfg.ActiveProvider, cfg.ActiveModel),
			Compact:         cfg.CompactConfig(),
			ShellTimeoutSec: cfg.GoalShellTimeoutSec,
			Steering:        steering,
			StreamCallback:  turn.onStream,
			ToolCallback:    turn.onTool,
			PermissionFn:    livePermission,
			ShellApproval: func(sctx context.Context, ar agent.ShellApprovalRequest) (agent.ShellApprovalDecision, error) {
				if livePermission() == config.PermissionYOLO {
					return agent.ShellApprovalAllowOnce, nil
				}
				return turn.requestShellApproval(sctx, ar)
			},
			AskUser: turn.askUser,
		}

		result, err := ag.Run(ctx, task)
		// Adopt the run's structured conversation even when it failed or was
		// cancelled: the partial history (this iteration's tool calls and
		// results) is valid context for the retry / next iteration, and
		// dropping it would restart the goal's context from scratch.
		if len(result.Messages) > 0 {
			history = result.Messages
		}
		if err != nil {
			if ctx.Err() != nil {
				return acpsdk.PromptResponse{StopReason: acpsdk.StopReasonCancelled}, nil
			}
			retries++
			if retries > maxGoalRetries {
				// Don't kill the goal on repeated errors: log, count a
				// no-progress strike, and move to the next iteration. The
				// stall guard is the final backstop for persistent failures.
				retries = 0
				state.NoProgress++
				if state.NoProgress >= state.NoProgressLimit {
					return finish(fmt.Sprintf("⏹ goal stopped: %d iterations failed with no progress. Last error: %v", state.NoProgress, err))
				}
				turn.sessionUpdate(acpsdk.UpdateAgentMessageText(fmt.Sprintf(
					"⚠ goal iteration %d kept failing (%v) — logged and moving to the next iteration (%d/%d strikes)\n",
					state.Iteration, err, state.NoProgress, state.NoProgressLimit)))
				continue
			}
			turn.sessionUpdate(acpsdk.UpdateAgentMessageText(fmt.Sprintf(
				"⚠ goal iteration %d failed (retry %d/%d): %v\n", state.Iteration, retries, maxGoalRetries, err)))
			state.Iteration-- // retry the same iteration
			continue
		}
		retries = 0
		totalTokens += result.TokensUsed
		if result.Content != "" {
			turn.sessionUpdate(acpsdk.UpdateAgentMessageText(result.Content + "\n"))
		}

		decision, reason := agent.ShouldContinueGoal(state, result, cwd)
		switch decision {
		case agent.GoalDecisionComplete:
			return finish("✅ goal complete: " + reason)
		case agent.GoalDecisionMaxIterations:
			return finish(fmt.Sprintf("⏹ goal stopped: reached max iterations (%d). Run /goal again to keep going.", state.MaxIterations))
		case agent.GoalDecisionStalled:
			return finish(fmt.Sprintf("⏹ goal stalled: %d iterations with no detectable progress.", state.NoProgress))
		}
	}
}

// goalNoProgressLimit applies the config default (3) when unset, mirroring
// the TUI and headless goal runners.
func goalNoProgressLimit(cfg config.UserConfig) int {
	if cfg.GoalNoProgressLimit <= 0 {
		return 3
	}
	return cfg.GoalNoProgressLimit
}

// appendUnique appends s to slice only if it is not already present.
func appendUnique(slice []string, s string) []string {
	for _, v := range slice {
		if v == s {
			return slice
		}
	}
	return append(slice, s)
}
