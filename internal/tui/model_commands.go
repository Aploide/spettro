package tui

import (
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"spettro/internal/commands"
	"spettro/internal/config"
	"spettro/internal/jobs"
	"spettro/internal/provider"
	"spettro/internal/session"
)

func (m Model) handleCommand(input string) (tea.Model, tea.Cmd) {
	fields := strings.Fields(input)
	cmd := fields[0]
	m.recordCommandEvent(input)

	switch cmd {
	case "/help":
		m.pushSystemMsg(helpText + m.customCommandsHelp())
	case "/exit", "/quit":
		return m, tea.Quit
	case "/update":
		return m.runUpdateCommand()
	case "/mode", "/next":
		m.mode = nextAgent(m.manifest, m.mode)
		m.persistUIState()
		m.showBanner(fmt.Sprintf("switched to %s mode", m.mode), "info")
		m.publishRemoteState("mode_change")
	case "/login":
		return m.startLogin(false)
	case "/logout":
		return m.handleLogout()
	case "/connect":
		m = m.openConnect()
	case "/models":
		if len(fields) >= 2 {
			if strings.Contains(fields[1], ":") {
				parts := strings.SplitN(fields[1], ":", 2)
				if !m.providers.HasModel(parts[0], parts[1]) {
					m.showBanner("unknown model: "+fields[1], "error")
				} else {
					if len(fields) >= 3 {
						if err := config.SaveAPIKey(parts[0], fields[2]); err != nil {
							m.showBanner("failed to save API key: "+err.Error(), "error")
							return m, nil
						}
					}
					_ = m.updateConfig(func(cfg *config.UserConfig) error {
						cfg.ActiveProvider = parts[0]
						cfg.ActiveModel = parts[1]
						return nil
					})
					m.showBanner(fmt.Sprintf("model set to %s:%s", parts[0], parts[1]), "success")
				}
			} else {
				m = m.openSelector(fields[1])
			}
		} else {
			m = m.openSelector("")
		}
	case "/permission":
		if len(fields) < 2 {
			m.showBanner("usage: /permission <yolo|restricted|ask-first>", "info")
		} else {
			level := config.PermissionLevel(fields[1])
			switch level {
			case config.PermissionYOLO, config.PermissionRestricted, config.PermissionAskFirst:
				_ = m.updateConfig(func(cfg *config.UserConfig) error {
					cfg.Permission = level
					return nil
				})
				m.showBanner(fmt.Sprintf("permission set to %s", level), "success")
			default:
				m.showBanner("invalid permission: use yolo, restricted, or ask-first", "error")
			}
		}
	case "/budget":
		if len(fields) < 2 {
			if m.cfg.TokenBudget <= 0 {
				m.showBanner("token budget: unlimited  usage: /budget <n|0>", "info")
			} else {
				m.showBanner(fmt.Sprintf("token budget: %d  usage: /budget <n|0>", m.cfg.TokenBudget), "info")
			}
		} else {
			var n int
			if _, err := fmt.Sscanf(fields[1], "%d", &n); err != nil || n < 0 {
				m.showBanner("usage: /budget <n|0>", "error")
			} else {
				_ = m.updateConfig(func(cfg *config.UserConfig) error {
					cfg.TokenBudget = n
					return nil
				})
				if n == 0 {
					m.showBanner("token budget set to unlimited", "success")
				} else {
					m.showBanner(fmt.Sprintf("token budget set to %d", n), "success")
				}
			}
		}
	case "/thinking", "/think":
		// /think [off|low|medium|high|x-high|max] sets the normalized thinking
		// level: Anthropic gets a thinking token budget, OpenAI and
		// OpenAI-compatible backends get reasoning_effort. Without an argument
		// we report the current setting. Hidden (and refused) for models the
		// catalog does not flag as reasoning-capable.
		if !m.activeModelSupportsReasoning() {
			m.showBanner("the active model does not support reasoning — thinking levels are unavailable", "info")
			break
		}
		current := strings.TrimSpace(m.cfg.ThinkingLevel)
		if current == "" {
			current = "off"
		}
		if len(fields) < 2 {
			m.showBanner("thinking: "+current+"  usage: /think <off|low|medium|high|x-high|max>", "info")
		} else {
			level := strings.ToLower(strings.TrimSpace(fields[1]))
			if !provider.IsValidThinkingLevel(level) {
				m.showBanner("usage: /think <off|low|medium|high|x-high|max>", "error")
			} else {
				if level == "off" {
					level = ""
				}
				_ = m.updateConfig(func(cfg *config.UserConfig) error {
					cfg.ThinkingLevel = level
					return nil
				})
				display := level
				if display == "" {
					display = "off"
				}
				m.showBanner("thinking level set to "+display, "success")
			}
		}
	case "/ultra":
		// /ultra [on|off] toggles the Ultra swarm mode: the top-level agent
		// gains the ultra fan-out tool and guidance to decompose hard tasks
		// across many parallel sub-agents. Works with any model; takes effect
		// on the next run.
		next := !m.cfg.Ultra
		if len(fields) >= 2 {
			switch strings.ToLower(strings.TrimSpace(fields[1])) {
			case "on":
				next = true
			case "off":
				next = false
			default:
				m.showBanner("usage: /ultra [on|off]", "error")
				return m, nil
			}
		}
		// A swarm runs many sub-agents concurrently; per-action approval
		// prompts would flood the user, so Ultra requires restricted or yolo.
		if next && m.cfg.Permission == config.PermissionAskFirst {
			m.showBanner("ultra needs restricted or yolo permission — switch first with /permission", "error")
			return m, nil
		}
		_ = m.updateConfig(func(cfg *config.UserConfig) error {
			cfg.Ultra = next
			return nil
		})
		if next {
			m.showBanner("ultra on — hard tasks fan out across a swarm of parallel sub-agents", "success")
		} else {
			m.showBanner("ultra off", "success")
		}
	case "/approve":
		if m.pendingPlan == "" {
			m.showBanner("no pending plan — run a plan prompt first", "info")
		} else {
			spec, ok := m.manifest.AgentByID("coding")
			if !ok {
				m.showBanner("coding agent not found in manifest", "error")
			} else {
				plan := m.pendingPlan
				m.pendingPlan = ""
				return m.runAgentApproved(spec, plan, nil, nil, true)
			}
		}
	case "/init":
		return m.runInit()
	case "/compact":
		return m.handleCompactCommand(input)
	case "/clear":
		m.autoSave()
		m.messages = nil
		m.convHistory = nil
		// Spooled tool outputs are only reachable through the cleared
		// history's references; drop them with the conversation.
		jobs.Spool().Cleanup()
		m.sessionID = ""
		m.todos = nil
		// Occupancy resets with the conversation; keep the gauge honest.
		m.contextTokens = 0
		// Usage counters are per-conversation; a cleared session starts at zero.
		m.providers.ResetUsage()
		m.compactWarningLevel = 0
		m.pushSystemMsg("conversation cleared")
		m.refreshViewport()
	case "/stats":
		m.pushSystemMsg(m.renderStats())
	case "/diff":
		return m.handleDiffCommand(input)
	case "/tasks":
		return m.handleTasksCommand(input)
	case "/mcp":
		return m.handleMCPCommand(input)
	case "/skills", "/skill":
		return m.handleSkillsCommand(input)
	case "/hooks":
		return m.handleHooksCommand()
	case "/memory":
		return m.handleMemoryCommand(input)
	case "/jobs":
		return m.handleJobsCommand(input)
	case "/plan":
		return m.handlePlanCommand(input)
	case "/goal":
		return m.handleGoalCommand(input)
	case "/permissions":
		return m.handlePermissionsCommand(input)
	case "/rewind":
		return m.openRewind()
	case "/checkpoints":
		m.pushSystemMsg(m.renderCheckpointsInfo())
	case "/storage":
		return m.handleStorageCommand(input)
	case "/resume":
		items, err := session.List(m.store.GlobalDir, m.cwd)
		if err != nil || len(items) == 0 {
			m.showBanner("no saved conversations found", "info")
		} else {
			m.showResume = true
			m.resumeItems = items
			m.resumeCursor = 0
			m.resumeScroll = 0
		}
	case "/remote":
		return m.handleRemoteCommand(input)
	case "/telegram", "/tg":
		return m.handleTelegramCommand(input)
	default:
		if custom, ok := m.findCustomCommand(cmd); ok {
			args := strings.TrimSpace(strings.TrimPrefix(input, fields[0]))
			allowShell := m.cfg.Permission == config.PermissionYOLO
			expanded, err := commands.Expand(custom, args, m.cwd, allowShell)
			if err != nil {
				m.showBanner(err.Error(), "error")
				return m, nil
			}
			return m.handlePrompt(expanded)
		}
		m.showBanner("unknown command: "+cmd, "error")
	}

	m.refreshViewport()
	return m, nil
}

func (m Model) handlePrompt(input string) (tea.Model, tea.Cmd) {
	eval := m.evaluateCompact()
	if eval.IsBlocking {
		m.showBanner("context limit reached; run /compact before sending new prompts", "error")
		return m, nil
	}
	mentionedFiles := m.extractMentionedFiles(input)
	prompt := injectMentionGuidance(input, mentionedFiles)
	prompt = m.injectAttachments(prompt)
	// Collect image paths (Kind="image") to send via the vision channel.
	var imagePaths []string
	for _, att := range m.attachments {
		if att.Kind == "image" {
			imagePaths = append(imagePaths, att.Path)
		}
	}
	sentAttachments := append([]attachmentItem(nil), m.attachments...)
	m.attachments = nil
	if m.thinking {
		m.queuePrompt(input, prompt, mentionedFiles, imagePaths)
		m.pushSystemMsg(fmt.Sprintf("queued request: %s", truncateLabel(input, 140)))
		if len(sentAttachments) > 0 {
			m.pushSystemMsg(fmt.Sprintf("(with %d attachment(s))", len(sentAttachments)))
		}
		m.showBanner("request queued for when the current run finishes", "info")
		m.refreshViewport()
		return m, nil
	}
	return m.startPromptRun(queuedPrompt{
		Input:          input,
		Prompt:         prompt,
		MentionedFiles: mentionedFiles,
		Images:         imagePaths,
	})
}

func (m Model) startPromptRun(req queuedPrompt) (tea.Model, tea.Cmd) {
	// Discard stale steering left over from an interrupted run: the user is
	// starting fresh with a full prompt, so old mid-run guidance no longer
	// applies (undelivered steering from a *completed* run was already
	// re-queued as prompts by the agentDoneMsg handler).
	m.steering.Drain()
	m.parallelAgents = nil
	m.ensureSession()
	m.messages = append(m.messages, ChatMessage{
		Role:    RoleUser,
		Content: req.Input,
		Images:  req.Images,
		At:      time.Now(),
	})
	m.awaitingInstead = false
	m.publishRemote("user_message", map[string]any{
		"content":         req.Input,
		"mentioned_files": req.MentionedFiles,
	})
	m.publishRemoteState("user_message")
	// Persist the user turn immediately so a crash mid-run never loses it.
	m.autoSave()
	m.refreshViewport()

	spec, ok := m.manifest.AgentByID(m.mode)
	if !ok {
		m.showBanner("unknown agent: "+m.mode, "error")
		return m, nil
	}
	return m.runAgent(spec, req.Prompt, req.MentionedFiles, req.Images)
}

func (m Model) maybeRunNextQueuedPrompt() (tea.Model, tea.Cmd) {
	if m.thinking || m.awaitingInstead {
		return m, nil
	}
	next, ok := m.nextQueuedPrompt()
	if !ok {
		return m, nil
	}
	m.pushSystemMsg(fmt.Sprintf("continuing with queued request: %s", truncateLabel(next.Input, 140)))
	return m.startPromptRun(next)
}
