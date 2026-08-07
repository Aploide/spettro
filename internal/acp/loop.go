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

// maxLoopFailures is the number of consecutive failed iterations tolerated
// before a /loop run gives up.
const maxLoopFailures = 3

const loopUsage = "usage: /loop <interval> <prompt>   (e.g. /loop 5m check whether CI is green)"

// runLoopCommand implements /loop over ACP. Like /goal, the whole recurring
// schedule runs inside this single prompt turn: each firing streams as
// session updates, the wait between firings honours ctx, and the editor's
// stop/cancel button (or /loop stop) interrupts it. cfg is the fresh per-turn
// config snapshot from Prompt.
func (b *bridge) runLoopCommand(ctx context.Context, s *acpSession, cfg *config.UserConfig, turn *turnState, input string) (acpsdk.PromptResponse, error) {
	reply := func(text string) (acpsdk.PromptResponse, error) {
		turn.sessionUpdate(acpsdk.UpdateAgentMessageText(text))
		return acpsdk.PromptResponse{StopReason: acpsdk.StopReasonEndTurn}, nil
	}

	rest := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(input), "/loop"))
	switch strings.ToLower(rest) {
	case "":
		return reply(loopUsage)
	case "status":
		b.mu.Lock()
		last := s.lastLoop
		b.mu.Unlock()
		if last == "" {
			return reply("no loop has run in this session")
		}
		return reply(last)
	case "stop":
		// A running loop never reaches here: Prompt intercepts "/loop stop"
		// while a turn is in flight and cancels it directly.
		return reply("no loop is running; over ACP a loop runs inside a single prompt turn — while it runs, /loop stop or the editor's stop/cancel interrupts it, and any other prompt steers it")
	}

	intervalArg, prompt, ok := agent.SplitLoopArgs(rest)
	if !ok {
		return reply(loopUsage)
	}
	interval, err := agent.ParseLoopInterval(intervalArg)
	if err != nil {
		return reply("loop: " + err.Error())
	}
	if strings.HasPrefix(prompt, "/loop") {
		return reply("loop: the looped prompt cannot itself be /loop")
	}

	b.mu.Lock()
	manifest := s.manifest
	cwd := s.cwd
	agentID := s.agentID
	// Steering: prompts sent while this loop turn runs are queued by Prompt
	// and injected at the running iteration's next step boundary. The queue
	// is shared across iterations, so text arriving between firings reaches
	// the next one.
	steering := s.steering
	// Each iteration threads the previous one's RunResult.Messages forward,
	// so the loop extends a byte-stable prompt prefix (cache hits) and every
	// firing sees what earlier firings found.
	// Seed the live permission for this turn; /permission or a config-option
	// change while the loop runs overwrites it and takes effect at the next
	// approval decision.
	s.permission = cfg.Permission
	history := s.history
	b.mu.Unlock()

	livePermission := func() config.PermissionLevel {
		b.mu.Lock()
		defer b.mu.Unlock()
		return s.permission
	}

	spec, ok := manifest.AgentByID(agentID)
	if !ok {
		return reply("agent not found: " + agentID)
	}
	spec.Permission = cfg.Permission

	if cfg.Permission != config.PermissionYOLO {
		turn.sessionUpdate(acpsdk.UpdateAgentMessageText(fmt.Sprintf(
			"note: permission is %q, so loop iterations will pause for approvals. For fully unattended runs use /permission yolo.\n",
			cfg.Permission)))
	}

	thinking := provider.ThinkingLevel("")
	if b.opts.Providers.SupportsReasoning(cfg.ActiveProvider, cfg.ActiveModel) {
		thinking = provider.ThinkingLevel(cfg.ThinkingLevel)
	}

	startedAt := time.Now()
	iteration := 0
	failures := 0
	totalTokens := 0
	finish := func(outcome string, stop acpsdk.StopReason) (acpsdk.PromptResponse, error) {
		summary := fmt.Sprintf("loop: %s\nevery: %s\noutcome: %s\niterations: %d, elapsed: %s",
			prompt, interval, outcome, iteration, time.Since(startedAt).Round(time.Second))
		b.mu.Lock()
		s.lastLoop = summary
		// Adopt the loop's final conversation so follow-up prompts in this
		// session keep the run's context and cache prefix.
		if len(history) > 0 {
			s.history = history
		}
		b.mu.Unlock()
		if stop == acpsdk.StopReasonEndTurn {
			turn.sessionUpdate(acpsdk.UpdateAgentMessageText(outcome))
		}
		return acpsdk.PromptResponse{
			StopReason: stop,
			Meta:       map[string]any{"spettro.app/tokensUsed": totalTokens},
		}, nil
	}

	turn.sessionUpdate(acpsdk.UpdateAgentMessageText(fmt.Sprintf(
		"starting loop — running %q every %s (stop with /loop stop or the editor's cancel)\n", prompt, interval)))

	for {
		if ctx.Err() != nil {
			return finish("stopped by user", acpsdk.StopReasonCancelled)
		}
		iteration++
		turn.sessionUpdate(acpsdk.UpdateAgentMessageText(fmt.Sprintf(
			"↻ loop iteration %d — %s\n", iteration, prompt)))

		ag := agent.LLMAgent{
			Spec:            spec,
			ProviderManager: b.opts.Providers,
			ProviderName:    func() string { return cfg.ActiveProvider },
			ModelName:       func() string { return cfg.ActiveModel },
			CWD:             cwd,
			MaxTokens:       cfg.TokenBudget,
			Thinking:        thinking,
			Ultra:           cfg.UltraActive(),
			Messages:        history,
			Manifest:        &manifest,
			SandboxState:    b.opts.SandboxState,
			SessionDir:      session.SessionDir(b.opts.GlobalDir, s.id),
			ContextWindow:   b.opts.Providers.ModelContext(cfg.ActiveProvider, cfg.ActiveModel),
			Compact:         cfg.CompactConfig(),
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
			AskUser: turn.askForm,
		}

		result, err := ag.Run(ctx, prompt)
		// Adopt the run's structured conversation even when it failed or was
		// cancelled: the partial history (this iteration's tool calls and
		// results) is valid context for the next firing, and dropping it
		// would restart the loop's context from scratch.
		if len(result.Messages) > 0 {
			history = result.Messages
		}
		if err != nil {
			if ctx.Err() != nil {
				return finish("stopped by user", acpsdk.StopReasonCancelled)
			}
			failures++
			if failures >= maxLoopFailures {
				return finish(fmt.Sprintf("⏹ loop stopped: %d consecutive iterations failed. Last error: %v", failures, err), acpsdk.StopReasonEndTurn)
			}
			turn.sessionUpdate(acpsdk.UpdateAgentMessageText(fmt.Sprintf(
				"⚠ loop iteration %d failed (%d/%d consecutive failures): %v\n", iteration, failures, maxLoopFailures, err)))
		} else {
			failures = 0
			totalTokens += result.TokensUsed
			if result.Content != "" {
				turn.sessionUpdate(acpsdk.UpdateAgentMessageText(result.Content + "\n"))
			}
		}

		turn.sessionUpdate(acpsdk.UpdateAgentMessageText(fmt.Sprintf(
			"⏸ loop waiting %s until iteration %d (stop with /loop stop)\n", interval, iteration+1)))
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return finish("stopped by user", acpsdk.StopReasonCancelled)
		case <-timer.C:
		}
	}
}
