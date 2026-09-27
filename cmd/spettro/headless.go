package main

import (
	"context"
	"fmt"
	"os/signal"
	"slices"
	"strings"
	"sync"
	"syscall"

	"spettro/internal/agent"
	"spettro/internal/config"
	"spettro/internal/provider"
	"spettro/internal/remote"
	"spettro/internal/sandbox"
	"spettro/internal/session"
)

func runHeadless(cwd, bindHost string, port int, sandboxOverrides sandbox.Overrides) {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	// Whatever the submissions started must not outlive the server.
	defer releaseSessionResources()

	boot, err := bootstrapSession(cwd, sandboxOverrides)
	if err != nil {
		fatal("%v", err)
	}
	store, pm, manifest := boot.store, boot.providers, boot.manifest
	// The server reports ready without waiting for the network; the first
	// submission waits (bounded) for local endpoints and the subscription
	// model list instead.
	discovery := startModelDiscovery(ctx, boot.cfg, pm, true)
	cfg := boot.cfg
	resolveActiveModel(&cfg, pm)

	mode := manifest.DefaultAgent
	if mode == "" {
		mode = "plan"
	}
	// One SandboxState for the server lifetime, shared across submissions.
	sb := agent.NewSandboxState(boot.sandboxPolicy)

	server, err := remote.NewServer(remote.Options{BindHost: bindHost})
	if err != nil {
		fatal("remote server error: %v", err)
	}

	if _, _, err = server.Start(port); err != nil {
		fatal("server start error: %v", err)
	}
	defer server.Stop()

	server.SetStatus(remote.Status{
		Thinking: false,
		Mode:     mode,
	})
	server.Publish("remote_started", map[string]any{
		"cwd":  cwd,
		"mode": mode,
	})

	// Print token for the Android app to parse from stdout.
	fmt.Printf("SPETTRO_TOKEN=%s\nSPETTRO_PORT=%d\n", server.Token(), server.Port())

	sessionID := "headless-" + session.ProjectHash(cwd)
	sessionDir := session.SessionDir(store.GlobalDir, sessionID)

	var (
		mu         sync.Mutex
		cancelRun  context.CancelFunc
		tokensUsed int
		msgCount   int
		// pendingPlan is the answer of the last successful plan-mode run,
		// which /approve hands to the coding agent (see takeApprovedPlan).
		pendingPlan string
	)

	// Interrupt handler goroutine.
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case _, ok := <-server.Interrupts():
				if !ok {
					return
				}
				mu.Lock()
				if cancelRun != nil {
					cancelRun()
				}
				mu.Unlock()
				server.Publish("remote_interrupt", map[string]any{"thinking": true})
			}
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return
		case req, ok := <-server.Submissions():
			if !ok {
				return
			}

			discovery.Wait(sessionModelsWait)
			// Reload config to pick up key changes made via /models.
			if freshCfg, ferr := config.LoadFull(); ferr == nil {
				cfg = freshCfg
				pm.SetAPIKeys(cfg.APIKeys)
			}

			msg := strings.TrimSpace(req.Message)
			// run is what the agent receives; msg stays what the user sent
			// (a skill invocation or $mention expands into the skill's
			// instructions, which the event stream should not echo).
			run, isSkill, skillErr := resolveHeadlessPrompt(cwd, cfg, msg)
			if skillErr != nil {
				req.Reply <- remote.SubmitResponse{Accepted: true, Note: "skill failed"}
				server.Publish("assistant_error", map[string]any{"error": skillErr.Error(), "mode": mode})
				continue
			}

			if plan, ok := takeApprovedPlan(msg, &pendingPlan, &manifest); ok && !isSkill {
				// /approve runs the pending plan with the coding agent, as
				// the TUI's /approve does; the run below reports it like
				// any other prompt.
				mode = "coding"
				run = plan
			} else if strings.HasPrefix(msg, "/") && !isSkill {
				reply, note := handleHeadlessCommand(msg, &mode, &cfg, pm, &manifest)
				req.Reply <- remote.SubmitResponse{Accepted: true, Note: note}
				server.Publish("remote_command", map[string]any{
					"command": msg,
					"mode":    mode,
				})
				if reply != "" {
					server.Publish("comment", map[string]any{
						"message": reply,
						"mode":    mode,
					})
				}
				server.SetStatus(remote.Status{
					Thinking:      false,
					Mode:          mode,
					SessionID:     sessionID,
					MessagesCount: msgCount,
					TokensUsed:    tokensUsed,
				})
				continue
			}

			msgCount++
			server.SetStatus(remote.Status{
				Thinking:      true,
				Mode:          mode,
				SessionID:     sessionID,
				MessagesCount: msgCount,
				TokensUsed:    tokensUsed,
			})
			server.Publish("state", map[string]any{
				"thinking":       true,
				"mode":           mode,
				"session_id":     sessionID,
				"messages_count": msgCount,
				"tokens_used":    tokensUsed,
			})
			server.Publish("user_message", map[string]any{
				"content": msg,
				"mode":    mode,
			})

			req.Reply <- remote.SubmitResponse{Accepted: true, Note: "running"}

			spec, ok := manifest.AgentByID(mode)
			if !ok {
				server.Publish("assistant_error", map[string]any{
					"error": "agent not found: " + mode,
					"mode":  mode,
				})
			} else {
				runCtx, runCancelFn := context.WithCancel(ctx)
				mu.Lock()
				cancelRun = runCancelFn
				mu.Unlock()

				ag := agent.LLMAgent{
					Spec:            spec,
					ProviderManager: pm,
					ProviderName:    func() string { return cfg.ActiveProvider },
					ModelName:       func() string { return cfg.ActiveModel },
					CWD:             cwd,
					MaxTokens:       cfg.TokenBudget,
					MaxOutputTokens: cfg.MaxOutputTokens,
					Thinking:        configuredThinking(pm, cfg),
					Ultra:           cfg.UltraActive(),
					Manifest:        &manifest,
					SandboxState:    sb,
					SessionDir:      sessionDir,
					ContextWindow:   pm.ModelContext(cfg.ActiveProvider, cfg.ActiveModel),
					Compact:         cfg.CompactConfig(),
					ToolCallback: func(tr agent.ToolTrace) {
						data := map[string]any{
							"name":   tr.Name,
							"status": tr.Status,
							"agent":  tr.AgentID,
							"mode":   mode,
						}
						if tr.Args != "" {
							data["args"] = tr.Args
						}
						if tr.Output != "" {
							data["output"] = tr.Output
						}
						server.Publish("tool", data)
					},
					ShellApproval: func(sctx context.Context, ar agent.ShellApprovalRequest) (agent.ShellApprovalDecision, error) {
						if cfg.Permission == config.PermissionYOLO {
							return agent.ShellApprovalAllowOnce, nil
						}
						dec, err := server.RequestApproval(sctx, ar.ToolID, ar.Command, ar.Reason)
						if err != nil {
							return agent.ShellApprovalDeny, err
						}
						switch dec.Decision {
						case "allow-once":
							return agent.ShellApprovalAllowOnce, nil
						case "allow-always":
							return agent.ShellApprovalAllowAlways, nil
						default:
							if dec.Instead != "" {
								return agent.ShellApprovalDeny, fmt.Errorf("do this instead: %s", dec.Instead)
							}
							return agent.ShellApprovalDeny, nil
						}
					},
					// The whole form goes out in one versioned event. A client
					// that only understands the flat v1 shape answers its first
					// question; the rest come back skipped rather than
					// defaulted, which is what the model needs to be told.
					AskUser: func(sctx context.Context, form agent.AskUserForm) ([]agent.AskUserAnswer, error) {
						return headlessAskUser(sctx, server, nextQuestionID(), form, headlessAskUserWait())
					},
				}
				ag.Spec.Permission = cfg.Permission

				result, runErr := ag.Run(runCtx, run)

				mu.Lock()
				cancelRun = nil
				mu.Unlock()
				runCancelFn()

				tokensUsed += result.TokensUsed
				if runErr != nil {
					server.Publish("assistant_error", map[string]any{
						"error": runErr.Error(),
						"mode":  mode,
					})
				} else {
					server.Publish("assistant_message", map[string]any{
						"content":     result.Content,
						"tokens_used": result.TokensUsed,
						"mode":        mode,
					})
					if mode == "plan" && strings.TrimSpace(result.Content) != "" {
						pendingPlan = result.Content
					}
				}
			}

			server.SetStatus(remote.Status{
				Thinking:      false,
				Mode:          mode,
				SessionID:     sessionID,
				MessagesCount: msgCount,
				TokensUsed:    tokensUsed,
			})
			server.Publish("state", map[string]any{
				"thinking":       false,
				"mode":           mode,
				"session_id":     sessionID,
				"messages_count": msgCount,
				"tokens_used":    tokensUsed,
			})
		}
	}
}

// takeApprovedPlan reports whether msg is /approve with a plan pending and
// a coding agent to run it; it then returns the plan and clears it, so a
// plan runs at most once. /approve without a plan is left to
// handleHeadlessCommand, which says there is none.
func takeApprovedPlan(msg string, pendingPlan *string, manifest *config.AgentManifest) (string, bool) {
	fields := strings.Fields(msg)
	if len(fields) == 0 || fields[0] != "/approve" || strings.TrimSpace(*pendingPlan) == "" {
		return "", false
	}
	if _, ok := manifest.AgentByID("coding"); !ok {
		return "", false
	}
	plan := *pendingPlan
	*pendingPlan = ""
	return plan, true
}

func handleHeadlessCommand(
	cmd string,
	mode *string,
	cfg *config.UserConfig,
	pm *provider.Manager,
	manifest *config.AgentManifest,
) (reply, note string) {
	fields := strings.Fields(cmd)
	if len(fields) == 0 {
		return "", "empty command"
	}
	switch fields[0] {
	case "/help":
		return strings.Join([]string{
			"available commands:",
			"  /mode              switch agent mode",
			"  /models p:m [key]  set provider:model (optionally save API key)",
			"  /permission <yolo|restricted|ask-first>",
			"  /approve           run the pending plan",
			"  /help              show this help",
		}, "\n"), "help displayed"

	case "/approve":
		// takeApprovedPlan handles /approve when a plan is pending.
		return "no pending plan — run a prompt in plan mode first", "no pending plan"

	case "/mode", "/next":
		next := nextHeadlessMode(*mode, manifest)
		*mode = next
		if _, err := config.Update(func(c *config.UserConfig) error {
			c.LastAgentID = next
			return nil
		}); err == nil {
			*cfg, _ = config.LoadFull()
		}
		return "mode: " + next, "mode changed"

	case "/models":
		if len(fields) < 2 || !strings.Contains(fields[1], ":") {
			return "usage: /models provider:model [api_key]", "usage shown"
		}
		parts := strings.SplitN(fields[1], ":", 2)
		if len(parts) != 2 {
			return "invalid format", "error"
		}
		if len(fields) >= 3 {
			if err := config.SaveAPIKey(parts[0], fields[2]); err != nil {
				return "error saving key: " + err.Error(), "error"
			}
		}
		if _, err := config.Update(func(c *config.UserConfig) error {
			c.ActiveProvider = parts[0]
			c.ActiveModel = parts[1]
			return nil
		}); err != nil {
			return "error: " + err.Error(), "error"
		}
		if fresh, err := config.LoadFull(); err == nil {
			*cfg = fresh
			pm.SetAPIKeys(cfg.APIKeys)
		}
		return "model: " + fields[1], "model updated"

	case "/permission":
		if len(fields) < 2 {
			return "usage: /permission yolo|restricted|ask-first", "usage shown"
		}
		perm := config.PermissionLevel(fields[1])
		switch perm {
		case config.PermissionYOLO, config.PermissionRestricted, config.PermissionAskFirst:
		default:
			return "unknown permission: " + fields[1], "error"
		}
		if _, err := config.Update(func(c *config.UserConfig) error {
			c.Permission = perm
			return nil
		}); err != nil {
			return "error: " + err.Error(), "error"
		}
		if fresh, err := config.LoadFull(); err == nil {
			*cfg = fresh
		}
		return "permission: " + string(perm), "permission updated"

	case "/exit", "/quit":
		exitSession(0)
		return "", ""

	default:
		return "unknown command; use /help", "unknown command"
	}
}

func nextHeadlessMode(current string, manifest *config.AgentManifest) string {
	modes := []string{"plan", "coding", "ask"}
	for _, a := range manifest.Agents {
		found := slices.Contains(modes, a.ID)
		if !found && a.Enabled {
			modes = append(modes, a.ID)
		}
	}
	for i, m := range modes {
		if m == current {
			return modes[(i+1)%len(modes)]
		}
	}
	return modes[0]
}
