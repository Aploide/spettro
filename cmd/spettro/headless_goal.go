package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"slices"
	"syscall"
	"time"

	"spettro/internal/agent"
	"spettro/internal/config"
	"spettro/internal/provider"
	"spettro/internal/sandbox"
	"spettro/internal/session"
)

// runHeadlessGoal runs the agent in goal mode without the TUI. It loops
// calling the agent until the goal is complete, stalled, or max iterations
// is reached. Exits with 0 on completion, 1 on stall/error.
func runHeadlessGoal(cwd string, objective string, sandboxOverrides sandbox.Overrides) {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	// Background jobs, PTY sessions and language servers the run started
	// must not outlive it. Every exit below goes through exitSession,
	// because os.Exit would skip this deferred call.
	defer releaseSessionResources()

	boot, err := bootstrapSession(cwd, sandboxOverrides)
	if err != nil {
		fatal("%v", err)
	}
	store, pm, manifest, cfg := boot.store, boot.providers, boot.manifest, boot.cfg

	// Headless goal mode defaults to yolo permission for unattended operation
	// unless explicitly overridden
	if cfg.Permission != config.PermissionYOLO {
		fmt.Fprintf(os.Stderr, "warning: forcing yolo permission mode for headless goal execution\n")
		cfg.Permission = config.PermissionYOLO
	}

	// A goal run starts working at once and needs the full model list (the
	// context window of a local or subscription model), so it waits for the
	// discovery; each request is bounded by its own client timeout.
	discovery := startModelDiscovery(ctx, cfg, pm, true)
	<-discovery.Done()
	resolveActiveModel(&cfg, pm, discovery)
	sb := agent.NewSandboxState(boot.sandboxPolicy)

	sessionID := "headless-goal-" + session.ProjectHash(cwd)
	sessionDir := session.SessionDir(store.GlobalDir, sessionID)

	// Resolve context window from active provider
	contextWindow := resolveContextWindow(pm, cfg.ActiveProvider, cfg.ActiveModel)

	// Resolve shell timeout from config
	shellTimeoutSec := max(cfg.GoalShellTimeoutSec, 0)

	// Get coding agent spec
	spec, ok := manifest.AgentByID("coding")
	if !ok {
		fmt.Fprintf(os.Stderr, "coding agent not found in manifest\n")
		exitSession(1)
	}

	// Append goal-complete to allowed tools
	spec.AllowedTools = appendUnique(spec.AllowedTools, "goal-complete")

	// Initialize goal state
	state := &agent.GoalState{
		Objective:       objective,
		Iteration:       0,
		NoProgress:      0,
		StartedAt:       time.Now(),
		MaxIterations:   resolveGoalMaxIterations(cfg),
		NoProgressLimit: resolveGoalNoProgressLimit(cfg),
	}

	fmt.Printf("Starting goal mode: %s\n", objective)
	fmt.Printf("Max iterations: %d (0=unlimited), No-progress limit: %d\n", state.MaxIterations, state.NoProgressLimit)

	// history carries the structured conversation across iterations so each
	// one extends a byte-stable prompt prefix (provider cache hits) instead of
	// re-exploring the workspace from a blank context. In-loop compaction
	// bounds its growth.
	var history []provider.Message

	// errorStrikes counts consecutive failed iterations; kept separate from
	// state.NoProgress so the workspace-signature stall guard stays intact.
	errorStrikes := 0

	// Outer loop
	for {
		select {
		case <-ctx.Done():
			fmt.Fprintf(os.Stderr, "\nInterrupted\n")
			exitSession(1)
		default:
		}

		state.Iteration++
		fmt.Printf("\n=== Iteration %d ===\n", state.Iteration)

		// Build task with preamble
		task := agent.GoalModePreamble + "\n\nOBJECTIVE:\n" + objective
		if state.Iteration > 1 {
			task = fmt.Sprintf("%s%s\n\n(Iteration %d. Review progress and continue working toward the objective. Call goal-complete only when fully done and verified.)",
				agent.GoalModePreamble, objective, state.Iteration)
		}

		// Build agent
		a := &agent.LLMAgent{
			Spec:            spec,
			ProviderManager: pm,
			ProviderName:    func() string { return cfg.ActiveProvider },
			ModelName:       func() string { return cfg.ActiveModel },
			CWD:             cwd,
			MaxTokens:       cfg.TokenBudget,
			MaxOutputTokens: cfg.MaxOutputTokens,
			Thinking:        configuredThinking(pm, cfg),
			Ultra:           cfg.UltraActive(),
			Messages:        history,
			Manifest:        &manifest,
			SandboxState:    sb,
			SessionDir:      sessionDir,
			GoalMode:        true,
			MaxSteps:        resolveGoalIterationSteps(cfg),
			ContextWindow:   contextWindow,
			Compact:         cfg.CompactConfig(),
			ShellTimeoutSec: shellTimeoutSec,
			ToolCallback: func(tr agent.ToolTrace) {
				status := "✓"
				if tr.Status == "error" {
					status = "✗"
				}
				fmt.Printf("  [%s] %s: %s\n", status, tr.Name, tr.Status)
			},
			ShellApproval: func(ctx context.Context, req agent.ShellApprovalRequest) (agent.ShellApprovalDecision, error) {
				// Auto-approve in headless goal mode (yolo)
				return agent.ShellApprovalAllowOnce, nil
			},
			AskUser: func(ctx context.Context, form agent.AskUserForm) ([]agent.AskUserAnswer, error) {
				// Nobody is attending a headless goal run; the ask-user tool
				// tells the agent to proceed on its own judgment.
				return nil, agent.ErrNoUserAvailable
			},
		}

		// Run agent
		result, err := a.Run(ctx, task)
		if err != nil {
			if ctx.Err() != nil {
				// Context cancelled, exit
				fmt.Fprintf(os.Stderr, "\nInterrupted during execution\n")
				exitSession(1)
			}
			// Continue to next iteration on transient errors, but count a
			// strike so a persistently failing provider still terminates
			// instead of looping forever.
			errorStrikes++
			if errorStrikes >= state.NoProgressLimit {
				fmt.Fprintf(os.Stderr, "\n✗ Goal stopped: %d consecutive iterations failed. Last error: %v\n", errorStrikes, err)
				exitSession(1)
			}
			fmt.Fprintf(os.Stderr, "Agent error: %v — continuing (%d/%d strikes)\n", err, errorStrikes, state.NoProgressLimit)
			continue
		}
		errorStrikes = 0

		if len(result.Messages) > 0 {
			history = result.Messages
		}

		if result.Content != "" {
			fmt.Printf("\n%s\n", result.Content)
		}

		// Evaluate whether to continue
		decision, reason := agent.ShouldContinueGoal(state, result, cwd)

		switch decision {
		case agent.GoalDecisionComplete:
			fmt.Printf("\n✓ Goal complete: %s\n", reason)
			fmt.Printf("Iterations: %d, Duration: %s\n", state.Iteration, time.Since(state.StartedAt).Round(time.Second))
			exitSession(0)

		case agent.GoalDecisionMaxIterations:
			fmt.Fprintf(os.Stderr, "\n✗ Goal stopped: %s (limit: %d)\n", reason, state.MaxIterations)
			exitSession(1)

		case agent.GoalDecisionStalled:
			fmt.Fprintf(os.Stderr, "\n✗ Goal stalled: %s (no progress for %d iterations)\n", reason, state.NoProgress)
			exitSession(1)

		case agent.GoalDecisionContinue:
			fmt.Printf("Continuing (no-progress: %d/%d)...\n", state.NoProgress, state.NoProgressLimit)
		}
	}
}

// configuredThinking returns the user's thinking_level for the active model,
// or "" (no thinking parameter) when the model does not support reasoning —
// the same rule the TUI and the ACP bridge apply.
func configuredThinking(pm *provider.Manager, cfg config.UserConfig) provider.ThinkingLevel {
	return pm.ConfiguredThinking(cfg.ActiveProvider, cfg.ActiveModel, cfg.ThinkingLevel)
}

// resolveContextWindow looks up the context window size for the active model.
func resolveContextWindow(pm *provider.Manager, providerName, modelName string) int {
	ctx := pm.ModelContext(providerName, modelName)
	if ctx > 0 {
		return ctx
	}
	// Fallback to a reasonable default
	return 100000
}

// resolveGoalMaxIterations returns the max iterations from config, or 0 for unlimited.
func resolveGoalMaxIterations(cfg config.UserConfig) int {
	return cfg.GoalMaxIterations
}

// resolveGoalNoProgressLimit returns the no-progress limit from config, defaulting to 3.
func resolveGoalNoProgressLimit(cfg config.UserConfig) int {
	if cfg.GoalNoProgressLimit > 0 {
		return cfg.GoalNoProgressLimit
	}
	return 3
}

// resolveGoalIterationSteps returns the per-iteration step cap, defaulting to
// 25 so each agent run yields back to the outer goal loop.
func resolveGoalIterationSteps(cfg config.UserConfig) int {
	if cfg.GoalIterationSteps > 0 {
		return cfg.GoalIterationSteps
	}
	return 25
}

// appendUnique appends a string to a slice only if it's not already present.
func appendUnique(slice []string, s string) []string {
	if slices.Contains(slice, s) {
		return slice
	}
	return append(slice, s)
}
