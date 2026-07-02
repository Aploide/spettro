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
		return reply("over ACP a goal runs inside a single prompt turn; use the editor's stop/cancel to interrupt it, or send a new prompt")
	}
	objective := rest

	b.mu.Lock()
	manifest := s.manifest
	cwd := s.cwd
	history := strings.Join(s.history, "\n")
	b.mu.Unlock()

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
		s.appendHistory("user: " + singleLineHistory("/goal "+objective))
		s.appendHistory("assistant: " + singleLineHistory(outcome))
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
			History:         history,
			Manifest:        &manifest,
			SandboxState:    b.opts.SandboxState,
			SessionDir:      session.SessionDir(b.opts.GlobalDir, s.id),
			GoalMode:        true,
			ContextWindow:   b.opts.Providers.ModelContext(cfg.ActiveProvider, cfg.ActiveModel),
			ShellTimeoutSec: cfg.GoalShellTimeoutSec,
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

		result, err := ag.Run(ctx, task)
		if err != nil {
			if ctx.Err() != nil {
				return acpsdk.PromptResponse{StopReason: acpsdk.StopReasonCancelled}, nil
			}
			retries++
			if retries > maxGoalRetries {
				return finish(fmt.Sprintf("⏹ goal stopped after %d consecutive errors: %v", retries, err))
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
