package acp

import (
	"context"
	"fmt"
	"strings"

	acpsdk "github.com/coder/acp-go-sdk"

	"spettro/internal/config"
	"spettro/internal/provider"
)

// acpAvailableCommands is the set of slash commands Spettro advertises to ACP
// clients via session/update (available_commands_update). Without this,
// clients like Zed intercept any "/word" the user types and reject it
// locally ("not a recognized command") before it ever reaches Prompt.
//
// Only commands that resolve from plain text are included: anything that
// needs a TUI dialog (skills, mcp, resume, ...) stays TUI-only for now,
// since ACP has no equivalent surface.
var acpAvailableCommands = []acpsdk.AvailableCommand{
	{Name: "help", Description: "show available commands"},
	{Name: "mode", Description: "switch agent mode", Input: hintInput("plan|coding|ask|...")},
	{Name: "models", Description: "show or set the active model", Input: hintInput("provider:model [api_key]")},
	{Name: "permission", Description: "set permission level", Input: hintInput("yolo|restricted|ask-first")},
	{Name: "budget", Description: "set token budget per request", Input: hintInput("<n|0>")},
	{Name: "thinking", Description: "set extended-thinking level", Input: hintInput("off|low|medium|high|x-high|max")},
	{Name: "goal", Description: "work autonomously toward an objective", Input: hintInput("<objective> | status")},
	{Name: "clear", Description: "clear conversation history"},
}

func hintInput(hint string) *acpsdk.AvailableCommandInput {
	return &acpsdk.AvailableCommandInput{Unstructured: &acpsdk.UnstructuredCommandInput{Hint: hint}}
}

// announceCommands pushes the available command list to the client right
// after a session is created, so ACP clients recognize Spettro's slash
// commands instead of rejecting them before they're ever sent.
func (b *bridge) announceCommands(ctx context.Context, sid acpsdk.SessionId) {
	_ = b.conn.SessionUpdate(ctx, acpsdk.SessionNotification{
		SessionId: sid,
		Update: acpsdk.SessionUpdate{
			AvailableCommandsUpdate: &acpsdk.SessionAvailableCommandsUpdate{
				AvailableCommands: acpAvailableCommands,
			},
		},
	})
}

// handleSlashCommand executes the ACP-safe subset of Spettro's slash
// commands (see acpAvailableCommands) directly against cfg/session state,
// mirroring internal/tui/model.go's handleCommand and cmd/spettro/headless.go's
// handleHeadlessCommand: ACP has no dialog surface, so these must resolve in
// one turn from plain text. handled=false means the input isn't one of ours
// and should fall through to the LLM as an ordinary prompt. /goal is NOT
// handled here — it needs the turn's streaming machinery (see goal.go).
func handleSlashCommand(s *acpSession, cfg *config.UserConfig, pm *provider.Manager, input string) (reply string, modeChanged bool, handled bool) {
	fields := strings.Fields(input)
	if len(fields) == 0 {
		return "", false, false
	}
	switch strings.ToLower(fields[0]) {
	case "/help":
		return acpHelpText, false, true

	case "/mode", "/next":
		if len(fields) < 2 {
			return "usage: /mode <agent-id>", false, true
		}
		id := fields[1]
		if _, ok := s.manifest.AgentByID(id); !ok {
			return "unknown mode: " + id, false, true
		}
		s.agentID = id
		return "mode: " + id, true, true

	case "/models", "/model":
		if len(fields) < 2 {
			return modelsText(pm, cfg), false, true
		}
		parts := strings.SplitN(fields[1], ":", 2)
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			return "usage: /models <provider:model> [api_key]", false, true
		}
		if !pm.HasModel(parts[0], parts[1]) {
			return "unknown model: " + fields[1] + "\n\n" + modelsText(pm, cfg), false, true
		}
		if len(fields) >= 3 {
			if err := config.SaveAPIKey(parts[0], fields[2]); err != nil {
				return "error saving API key: " + err.Error(), false, true
			}
		}
		if _, err := config.Update(func(c *config.UserConfig) error {
			c.ActiveProvider = parts[0]
			c.ActiveModel = parts[1]
			return nil
		}); err != nil {
			return "error: " + err.Error(), false, true
		}
		if fresh, err := config.LoadFull(); err == nil {
			*cfg = fresh
			pm.SetAPIKeys(cfg.APIKeys)
		}
		return "model set to " + fields[1], false, true

	case "/permission", "/permissions":
		if len(fields) < 2 {
			return "usage: /permission <yolo|restricted|ask-first>  (current: " + string(cfg.Permission) + ")", false, true
		}
		level := config.PermissionLevel(fields[1])
		switch level {
		case config.PermissionYOLO, config.PermissionRestricted, config.PermissionAskFirst:
		default:
			return "invalid permission: use yolo, restricted, or ask-first", false, true
		}
		if _, err := config.Update(func(c *config.UserConfig) error {
			c.Permission = level
			return nil
		}); err != nil {
			return "error: " + err.Error(), false, true
		}
		cfg.Permission = level
		return "permission set to " + string(level), false, true

	case "/budget":
		if len(fields) < 2 {
			if cfg.TokenBudget <= 0 {
				return "token budget: unlimited  usage: /budget <n|0>", false, true
			}
			return fmt.Sprintf("token budget: %d  usage: /budget <n|0>", cfg.TokenBudget), false, true
		}
		var n int
		if _, err := fmt.Sscanf(fields[1], "%d", &n); err != nil || n < 0 {
			return "usage: /budget <n|0>", false, true
		}
		if _, err := config.Update(func(c *config.UserConfig) error {
			c.TokenBudget = n
			return nil
		}); err != nil {
			return "error: " + err.Error(), false, true
		}
		cfg.TokenBudget = n
		if n == 0 {
			return "token budget set to unlimited", false, true
		}
		return fmt.Sprintf("token budget set to %d", n), false, true

	case "/thinking", "/think":
		current := strings.TrimSpace(cfg.ThinkingLevel)
		if current == "" {
			current = "off"
		}
		if len(fields) < 2 {
			return "thinking: " + current + "  usage: /think <off|low|medium|high|x-high|max>", false, true
		}
		level := strings.ToLower(strings.TrimSpace(fields[1]))
		if !provider.IsValidThinkingLevel(level) {
			return "usage: /think <off|low|medium|high|x-high|max>", false, true
		}
		if level == "off" {
			level = ""
		}
		if _, err := config.Update(func(c *config.UserConfig) error {
			c.ThinkingLevel = level
			return nil
		}); err != nil {
			return "error: " + err.Error(), false, true
		}
		cfg.ThinkingLevel = level
		display := level
		if display == "" {
			display = "off"
		}
		return "thinking level set to " + display, false, true

	case "/clear":
		s.history = nil
		return "conversation history cleared", false, true
	}
	return "", false, false
}

// modelsText renders the current model plus the connected roster, the ACP
// text stand-in for the TUI's model selector dialog.
func modelsText(pm *provider.Manager, cfg *config.UserConfig) string {
	var b strings.Builder
	fmt.Fprintf(&b, "current model: %s:%s\n", cfg.ActiveProvider, cfg.ActiveModel)
	if connected := pm.ConnectedModels(cfg.APIKeys); len(connected) > 0 {
		b.WriteString("connected models:\n")
		for _, m := range connected {
			fmt.Fprintf(&b, "  %s:%s\n", m.Provider, m.Name)
		}
	} else {
		b.WriteString("no connected models (add an API key: /models <provider:model> <api_key>)\n")
	}
	b.WriteString("usage: /models <provider:model> [api_key]")
	return b.String()
}

const acpHelpText = `commands:
  /help                 this message
  /mode <agent-id>      switch agent mode (plan, coding, ask, ...)
  /models [p:m [key]]   show connected models or set the active one
  /permission <level>   set permission: yolo | restricted | ask-first
  /budget [n|0]         set token budget per request (0 = unlimited)
  /think <level>        set extended-thinking level (off|low|medium|high|x-high|max)
  /goal <objective>     work autonomously until the objective is met (/goal status)
  /clear                clear conversation history`
