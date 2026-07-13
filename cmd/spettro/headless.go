package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"

	"spettro/internal/agent"
	"spettro/internal/config"
	"spettro/internal/models"
	"spettro/internal/provider"
	"spettro/internal/remote"
	"spettro/internal/sandbox"
	"spettro/internal/session"
	"spettro/internal/spettro"
	"spettro/internal/storage"
)

// spettroInfosToModels converts Spettro backend model entries into provider
// models tagged with the "spettro" provider.
func spettroInfosToModels(infos []spettro.ModelInfo) []provider.Model {
	out := make([]provider.Model, 0, len(infos))
	for _, mi := range infos {
		out = append(out, provider.Model{
			Provider:     spettro.ProviderID,
			ProviderName: spettro.ProviderName,
			Name:         mi.ID,
			DisplayName:  mi.ID,
			ToolCall:     true,
			Vision:       mi.Vision,
			Context:      mi.ContextWindow,
		})
	}
	return out
}

func runHeadless(cwd, bindHost string, port int, sandboxOverrides sandbox.Overrides) {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	store, err := storage.New(cwd)
	if err != nil {
		fatal("storage error: %v", err)
	}

	cfg, err := config.LoadFull()
	if err != nil {
		fatal("config error: %v", err)
	}

	pm := provider.NewManager()
	pm.SetAPIKeys(cfg.APIKeys)

	if cat, err := models.Load(); err == nil {
		pm.SetCatalog(cat)
	}
	for _, endpoint := range cfg.LocalEndpoints {
		if localModels, err := provider.ProbeLocalServer(context.Background(), endpoint); err == nil {
			pm.AddLocalModels(localModels)
		}
	}
	// Register the Spettro Subscription endpoint + models when signed in.
	if strings.TrimSpace(cfg.APIKeys[spettro.ProviderID]) != "" {
		pm.SetSpettro(spettro.InferenceBaseURL(), nil)
		if infos, err := spettro.ListModels(context.Background(), cfg.APIKeys[spettro.ProviderID]); err == nil {
			pm.SetSpettro(spettro.InferenceBaseURL(), spettroInfosToModels(infos))
		}
	}
	models.RefreshBackground(pm.SetCatalog)

	// Don't run with a model whose provider has no credentials (fresh install
	// or removed key): fall back to the best connected model.
	cfg.ActiveProvider, cfg.ActiveModel = pm.ResolveActive(cfg.ActiveProvider, cfg.ActiveModel, cfg.APIKeys)

	manifest, _ := config.LoadAgentManifestForProject(cwd)
	mode := manifest.DefaultAgent
	if mode == "" {
		mode = "plan"
	}

	// One SandboxState for the server lifetime, shared across submissions.
	sandboxPolicy, err := resolveSandboxPolicy(sandboxOverrides, manifest)
	if err != nil {
		fatal("sandbox error: %v", err)
	}
	sb := agent.NewSandboxState(sandboxPolicy)

	// Write-confine the server process itself as defense-in-depth (best-effort;
	// the model surface is confined at the shell and file-tool layers).
	if sandboxPolicy.Enabled() {
		writable := append([]string{store.GlobalDir, store.ProjectDir, cwd}, sandboxPolicy.ExtraWritable...)
		if err := sandbox.ConfineParent(writable); err != nil {
			fmt.Fprintf(os.Stderr, "warning: parent sandbox not applied: %v\n", err)
		}
	}

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
	server.Publish("remote_started", map[string]interface{}{
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
				server.Publish("remote_interrupt", map[string]interface{}{"thinking": true})
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

			// Reload config to pick up key changes made via /models.
			if freshCfg, ferr := config.LoadFull(); ferr == nil {
				cfg = freshCfg
				pm.SetAPIKeys(cfg.APIKeys)
			}

			msg := strings.TrimSpace(req.Message)

			if strings.HasPrefix(msg, "/") {
				reply, note := handleHeadlessCommand(msg, &mode, &cfg, pm, &manifest)
				req.Reply <- remote.SubmitResponse{Accepted: true, Note: note}
				server.Publish("remote_command", map[string]interface{}{
					"command": msg,
					"mode":    mode,
				})
				if reply != "" {
					server.Publish("comment", map[string]interface{}{
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
			server.Publish("state", map[string]interface{}{
				"thinking":       true,
				"mode":           mode,
				"session_id":     sessionID,
				"messages_count": msgCount,
				"tokens_used":    tokensUsed,
			})
			server.Publish("user_message", map[string]interface{}{
				"content": msg,
				"mode":    mode,
			})

			req.Reply <- remote.SubmitResponse{Accepted: true, Note: "running"}

			spec, ok := manifest.AgentByID(mode)
			if !ok {
				server.Publish("assistant_error", map[string]interface{}{
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
					Manifest:        &manifest,
					SandboxState:    sb,
					SessionDir:      sessionDir,
					ToolCallback: func(tr agent.ToolTrace) {
						data := map[string]interface{}{
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
					AskUser: func(sctx context.Context, ar agent.AskUserRequest) (string, error) {
						qid := fmt.Sprintf("q-%d", msgCount)
						return server.RequestAskUser(sctx, qid, ar.Question, ar.Options, ar.AllowFreeResponse)
					},
				}
				ag.Spec.Permission = cfg.Permission

				result, runErr := ag.Run(runCtx, msg)

				mu.Lock()
				cancelRun = nil
				mu.Unlock()
				runCancelFn()

				tokensUsed += result.TokensUsed
				if runErr != nil {
					server.Publish("assistant_error", map[string]interface{}{
						"error": runErr.Error(),
						"mode":  mode,
					})
				} else {
					server.Publish("assistant_message", map[string]interface{}{
						"content":     result.Content,
						"tokens_used": result.TokensUsed,
						"mode":        mode,
					})
				}
			}

			server.SetStatus(remote.Status{
				Thinking:      false,
				Mode:          mode,
				SessionID:     sessionID,
				MessagesCount: msgCount,
				TokensUsed:    tokensUsed,
			})
			server.Publish("state", map[string]interface{}{
				"thinking":       false,
				"mode":           mode,
				"session_id":     sessionID,
				"messages_count": msgCount,
				"tokens_used":    tokensUsed,
			})
		}
	}
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
		os.Exit(0)
		return "", ""

	default:
		return "unknown command; use /help", "unknown command"
	}
}

func nextHeadlessMode(current string, manifest *config.AgentManifest) string {
	modes := []string{"plan", "coding", "ask"}
	for _, a := range manifest.Agents {
		found := false
		for _, m := range modes {
			if m == a.ID {
				found = true
				break
			}
		}
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
