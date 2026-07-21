package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"spettro/internal/telegram"
)

// telegramSubmitMsg is delivered to Update when an authorised Telegram chat
// posts a message that the relay routes into the TUI. The TUI must reply
// on req.Reply exactly once.
type telegramSubmitMsg struct {
	req telegram.SubmitRequest
}

// telegramInterruptMsg is sent when a Telegram user runs /cancel.
type telegramInterruptMsg struct{}

// waitForTelegramSubmit re-arms a cmd that consumes one inbound submission
// from the relay.
func waitForTelegramSubmit(r *telegram.Relay) tea.Cmd {
	if r == nil {
		return nil
	}
	ch := r.Submissions()
	if ch == nil {
		return nil
	}
	return func() tea.Msg {
		req, ok := <-ch
		if !ok {
			return nil
		}
		return telegramSubmitMsg{req: req}
	}
}

// waitForTelegramInterrupt re-arms a cmd that consumes one inbound
// interrupt signal.
func waitForTelegramInterrupt(r *telegram.Relay) tea.Cmd {
	if r == nil {
		return nil
	}
	ch := r.Interrupts()
	if ch == nil {
		return nil
	}
	return func() tea.Msg {
		_, ok := <-ch
		if !ok {
			return nil
		}
		return telegramInterruptMsg{}
	}
}

// telegramListenCmds returns the pair of cmds that pump submissions and
// interrupts from the relay into the bubbletea program loop.
func telegramListenCmds(r *telegram.Relay) []tea.Cmd {
	if r == nil {
		return nil
	}
	return []tea.Cmd{
		waitForTelegramSubmit(r),
		waitForTelegramInterrupt(r),
	}
}

// handleTelegramCommand implements `/telegram` (alias `/tg`).
//
//	/telegram                 → status
//	/telegram help            → help text
//	/telegram setup <token>   → save token + validate + autostart
//	/telegram token <token>   → replace stored token
//	/telegram start           → start polling with stored token
//	/telegram stop            → stop polling (keeps config)
//	/telegram restart         → stop + start
//	/telegram status          → URL/allowlist/bound chats
//	/telegram allow <id|@u>   → add to allowlist
//	/telegram deny <id|@u>    → remove from allowlist
//	/telegram remove <id|@u>  → alias of deny
//	/telegram list            → show allowlist
//	/telegram reset           → forget token + config
func (m Model) handleTelegramCommand(input string) (tea.Model, tea.Cmd) {
	fields := strings.Fields(input)
	sub := "status"
	if len(fields) >= 2 {
		sub = strings.ToLower(fields[1])
	}
	defer m.refreshViewport()
	switch sub {
	case "help", "?":
		m.pushSystemMsg(telegramHelpText)
		return m, nil
	case "status":
		m.printTelegramStatus()
		return m, nil
	case "setup", "token":
		return m.telegramSetToken(fields)
	case "start", "on":
		return m.telegramStart()
	case "stop", "off", "shutdown":
		return m.telegramStop(true)
	case "restart":
		stopped, _ := m.telegramStop(false)
		if nm, ok := stopped.(Model); ok {
			m = nm
		}
		return m.telegramStart()
	case "allow", "add":
		return m.telegramAllow(fields)
	case "deny", "remove", "revoke":
		return m.telegramDeny(fields)
	case "list", "ls", "allowlist":
		m.pushSystemMsg(m.telegramAllowlistText())
		return m, nil
	case "reset", "forget":
		return m.telegramReset()
	}
	m.showBanner("usage: /telegram <setup|start|stop|status|allow|deny|list|reset>", "info")
	return m, nil
}

// telegramSetToken saves the Bot API token (encrypted), runs a getMe probe
// to validate it, and persists the bot username.
func (m Model) telegramSetToken(fields []string) (tea.Model, tea.Cmd) {
	if len(fields) < 3 {
		m.showBanner("usage: /telegram setup <bot_token>", "info")
		return m, nil
	}
	token := strings.TrimSpace(fields[2])
	if token == "" {
		m.showBanner("token must not be empty", "error")
		return m, nil
	}
	if err := telegram.SaveToken(token); err != nil {
		m.showBanner("telegram: save token: "+err.Error(), "error")
		return m, nil
	}

	// Probe the token with a short-lived client so we can tell the user
	// the bot's username before they go set up the allowlist.
	probe := telegram.NewBotClient(token)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	me, err := probe.GetMe(ctx)
	if err != nil {
		m.showBanner("telegram: token rejected — "+err.Error(), "error")
		return m, nil
	}
	cfg, saveErr := telegram.UpdateConfig(func(cfg *telegram.PersistedConfig) error {
		cfg.BotUsername = me.Username
		return nil
	})
	if saveErr != nil {
		m.showBanner("telegram: save config: "+saveErr.Error(), "error")
		return m, nil
	}
	m.pushSystemMsg(strings.Join([]string{
		"telegram bot configured",
		fmt.Sprintf("  bot:        @%s (%s)", me.Username, strings.TrimSpace(me.FirstName)),
		fmt.Sprintf("  allowlist:  %s", telegramRenderAllowlist(cfg.Allowlist)),
		"  next:       /telegram allow <@username|chat_id>",
		"              /telegram start",
	}, "\n"))
	m.showBanner("telegram token saved", "success")
	return m, nil
}

// telegramStart spins up the relay's polling goroutine.
func (m Model) telegramStart() (tea.Model, tea.Cmd) {
	if m.telegramRelay != nil && m.telegramRelay.IsRunning() {
		m.showBanner("telegram already running", "info")
		return m, nil
	}
	token, err := telegram.LoadToken()
	if err != nil {
		m.showBanner("telegram: read token: "+err.Error(), "error")
		return m, nil
	}
	if strings.TrimSpace(token) == "" {
		m.showBanner("no telegram token — run /telegram setup <bot_token>", "error")
		return m, nil
	}
	cfg, err := telegram.LoadConfig()
	if err != nil {
		m.showBanner("telegram: load config: "+err.Error(), "error")
		return m, nil
	}
	relay, err := telegram.NewRelay(telegram.Options{
		Token:       token,
		BotUsername: cfg.BotUsername,
		Config:      cfg,
	})
	if err != nil {
		m.showBanner("telegram: "+err.Error(), "error")
		return m, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := relay.Start(ctx); err != nil {
		m.showBanner("telegram: start: "+err.Error(), "error")
		return m, nil
	}
	// Persist autostart=true so the next launch resumes automatically.
	if _, err := relay.Mutate(func(cfg *telegram.PersistedConfig) {
		cfg.AutoStart = true
	}); err != nil {
		m.showBanner("telegram: persist autostart: "+err.Error(), "warn")
	}
	m.telegramRelay = relay
	bot := relay.BotUsername()
	if bot == "" {
		bot = "<unknown>"
	}
	m.pushSystemMsg(strings.Join([]string{
		"telegram relay started",
		"  bot:       @" + bot,
		"  allowlist: " + telegramRenderAllowlist(relay.Config().Allowlist),
		"  stop:      /telegram stop",
	}, "\n"))
	m.showBanner("telegram listening for messages", "success")
	cmds := telegramListenCmds(relay)
	return m, tea.Batch(cmds...)
}

// telegramStop halts the poll loop. persistAutoStart controls whether the
// AutoStart flag flips off (we want it true for restart, false for stop).
func (m Model) telegramStop(persistAutoStart bool) (tea.Model, tea.Cmd) {
	if m.telegramRelay == nil {
		m.showBanner("telegram not running", "info")
		return m, nil
	}
	m.telegramRelay.Stop()
	if persistAutoStart {
		if _, err := telegram.UpdateConfig(func(cfg *telegram.PersistedConfig) error {
			cfg.AutoStart = false
			return nil
		}); err != nil {
			m.showBanner("telegram: persist autostart: "+err.Error(), "warn")
		}
	}
	m.telegramRelay = nil
	m.showBanner("telegram stopped", "success")
	return m, nil
}

func (m Model) telegramAllow(fields []string) (tea.Model, tea.Cmd) {
	if len(fields) < 3 {
		m.showBanner("usage: /telegram allow <@username|chat_id>", "info")
		return m, nil
	}
	target := strings.Join(fields[2:], " ")
	username, id, err := telegram.ParseChatTarget(target)
	if err != nil {
		m.showBanner("telegram: "+err.Error(), "error")
		return m, nil
	}
	entry := telegram.AllowEntry{Username: username, ChatID: id}
	cfg, changed, applyErr := m.telegramUpdateConfig(func(cfg *telegram.PersistedConfig) bool {
		updated, added := telegram.AddAllowEntry(*cfg, entry)
		*cfg = updated
		return added
	})
	if applyErr != nil {
		m.showBanner("telegram: save: "+applyErr.Error(), "error")
		return m, nil
	}
	if !changed {
		m.showBanner("telegram: "+entry.String()+" already allowed", "info")
		return m, nil
	}
	m.pushSystemMsg("telegram: allowed " + entry.String() + "\n  current allowlist: " + telegramRenderAllowlist(cfg.Allowlist))
	m.showBanner("telegram allowlist updated", "success")
	return m, nil
}

func (m Model) telegramDeny(fields []string) (tea.Model, tea.Cmd) {
	if len(fields) < 3 {
		m.showBanner("usage: /telegram deny <@username|chat_id>", "info")
		return m, nil
	}
	target := strings.Join(fields[2:], " ")
	username, id, err := telegram.ParseChatTarget(target)
	if err != nil {
		m.showBanner("telegram: "+err.Error(), "error")
		return m, nil
	}
	cfg, changed, applyErr := m.telegramUpdateConfig(func(cfg *telegram.PersistedConfig) bool {
		updated, removed := telegram.RemoveAllowEntry(*cfg, username, id)
		*cfg = updated
		return removed > 0
	})
	if applyErr != nil {
		m.showBanner("telegram: save: "+applyErr.Error(), "error")
		return m, nil
	}
	if !changed {
		m.showBanner("telegram: no matching allowlist entry", "info")
		return m, nil
	}
	m.pushSystemMsg("telegram: removed " + telegram.FormatChatTarget(username, id) + "\n  current allowlist: " + telegramRenderAllowlist(cfg.Allowlist))
	m.showBanner("telegram allowlist updated", "success")
	return m, nil
}

// telegramUpdateConfig is a helper that runs mut against the persisted
// config (whether or not the relay is running) and refreshes the relay's
// in-memory snapshot on success.
//
// mut returns true iff anything actually changed; the function bubbles
// that flag back up so the caller can decide whether to log a no-op.
func (m Model) telegramUpdateConfig(mut func(*telegram.PersistedConfig) bool) (telegram.PersistedConfig, bool, error) {
	var changed bool
	if m.telegramRelay != nil {
		cfg, err := m.telegramRelay.Mutate(func(cfg *telegram.PersistedConfig) {
			changed = mut(cfg)
		})
		return cfg, changed, err
	}
	cfg, err := telegram.UpdateConfig(func(cfg *telegram.PersistedConfig) error {
		changed = mut(cfg)
		return nil
	})
	return cfg, changed, err
}

func (m Model) telegramReset() (tea.Model, tea.Cmd) {
	if m.telegramRelay != nil {
		m.telegramRelay.Stop()
		m.telegramRelay = nil
	}
	if err := telegram.SaveToken(""); err != nil {
		m.showBanner("telegram: clear token: "+err.Error(), "error")
		return m, nil
	}
	if _, err := telegram.UpdateConfig(func(cfg *telegram.PersistedConfig) error {
		*cfg = telegram.PersistedConfig{}
		return nil
	}); err != nil {
		m.showBanner("telegram: clear config: "+err.Error(), "error")
		return m, nil
	}
	m.pushSystemMsg("telegram: token and config cleared")
	m.showBanner("telegram reset", "success")
	return m, nil
}

func (m Model) printTelegramStatus() {
	if m.telegramRelay == nil {
		token, _ := telegram.LoadToken()
		cfg, _ := telegram.LoadConfig()
		state := "stopped"
		if strings.TrimSpace(token) == "" {
			state = "not configured"
		}
		lines := []string{
			"telegram: " + state,
		}
		if cfg.BotUsername != "" {
			lines = append(lines, "  bot:       @"+cfg.BotUsername)
		}
		lines = append(lines, "  allowlist: "+telegramRenderAllowlist(cfg.Allowlist))
		if strings.TrimSpace(token) == "" {
			lines = append(lines, "  next:      /telegram setup <bot_token>")
		} else {
			lines = append(lines, "  next:      /telegram start")
		}
		m.pushSystemMsg(strings.Join(lines, "\n"))
		return
	}
	cfg := m.telegramRelay.Config()
	bound := m.telegramRelay.BoundChats()
	lines := []string{
		"telegram: running",
		"  bot:        @" + m.telegramRelay.BotUsername(),
		"  allowlist:  " + telegramRenderAllowlist(cfg.Allowlist),
		"  bound:      " + telegramRenderChatIDs(bound),
	}
	if err := m.telegramRelay.LastError(); err != nil {
		lines = append(lines, "  last error: "+err.Error())
	}
	if err := m.telegramRelay.LastSendError(); err != nil {
		lines = append(lines, "  last send:  "+err.Error())
	}
	m.pushSystemMsg(strings.Join(lines, "\n"))
}

func (m Model) telegramAllowlistText() string {
	var cfg telegram.PersistedConfig
	if m.telegramRelay != nil {
		cfg = m.telegramRelay.Config()
	} else {
		cfg, _ = telegram.LoadConfig()
	}
	return "telegram allowlist: " + telegramRenderAllowlist(cfg.Allowlist)
}

func telegramRenderAllowlist(entries []telegram.AllowEntry) string {
	if len(entries) == 0 {
		return "(empty)"
	}
	parts := make([]string, 0, len(entries))
	for _, e := range entries {
		parts = append(parts, e.String())
	}
	return strings.Join(parts, ", ")
}

func telegramRenderChatIDs(ids []int64) string {
	if len(ids) == 0 {
		return "(none yet — waiting for an authorised chat to message the bot)"
	}
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		parts = append(parts, fmt.Sprintf("%d", id))
	}
	return strings.Join(parts, ", ")
}

const telegramHelpText = `telegram relay commands:

setup
  /telegram setup <bot_token>   save Bot API token, validate, persist bot username
  /telegram token <bot_token>   replace stored token
  /telegram allow <@u|id>       add username or chat ID to allowlist
  /telegram deny  <@u|id>       remove username or chat ID
  /telegram list                show current allowlist
  /telegram reset               forget token + config

lifecycle
  /telegram start               start the long-poll worker
  /telegram stop                stop polling (keeps config)
  /telegram restart             stop then start
  /telegram status              show running state, bot, allowlist, bound chats

inside Telegram (sent from your phone):
  any plain text                  → new prompt for the active agent
  any /-command                   → executed as a Spettro slash command
  /cancel                         → interrupt the running agent
  /help, /whoami                  → bot-side info
  plain text while a question is pending → answers the ask-user dialog

alias: /tg works everywhere /telegram does.`

// handleTelegramSubmission processes a message that arrived from the relay.
// Mirrors handleRemoteSubmission's contract: must reply on req.Reply
// exactly once.
func (m Model) handleTelegramSubmission(req telegram.SubmitRequest) (tea.Model, tea.Cmd) {
	reply := req.Reply
	text := strings.TrimSpace(req.Message)
	if text == "" {
		sendTelegramReply(reply, telegram.SubmitResponse{Accepted: false, Error: "empty message"})
		return m, nil
	}
	m.publishRemote("telegram_inbound", map[string]interface{}{
		"chat_id": req.ChatID,
		"user_id": req.UserID,
		"kind":    string(req.Kind),
		"from":    req.From,
		"preview": telegram.Truncate(text, 200),
	})

	// Free-text answer to a pending ask-user dialog.
	if req.Kind == telegram.SubmitAnswer && m.pendingQuestion != nil {
		m = m.resolveAskUser(text, fmt.Sprintf("answered via Telegram (%s)", req.From))
		sendTelegramReply(reply, telegram.SubmitResponse{Accepted: true, Note: "answered ask-user dialog"})
		m.refreshViewport()
		return m, nil
	}

	// Slash command path.
	if strings.HasPrefix(text, "/") {
		if m.thinking {
			sendTelegramReply(reply, telegram.SubmitResponse{
				Accepted: false,
				Error:    "commands cannot be queued while an agent is running",
			})
			return m, nil
		}
		sendTelegramReply(reply, telegram.SubmitResponse{Accepted: true, Note: "command dispatched"})
		return m.handleCommand(text)
	}

	// Plain prompt path.
	if m.thinking {
		mentionedFiles := m.extractMentionedFiles(text)
		prompt := injectMentionGuidance(text, mentionedFiles)
		m.queuePrompt(text, prompt, mentionedFiles, nil)
		m.pushSystemMsg(fmt.Sprintf("queued telegram request from %s: %s", req.From, truncateLabel(text, 140)))
		m.showBanner("telegram request queued", "info")
		sendTelegramReply(reply, telegram.SubmitResponse{Accepted: true, Queued: true, Note: "queued behind active run"})
		m.refreshViewport()
		return m, nil
	}

	sendTelegramReply(reply, telegram.SubmitResponse{Accepted: true, Note: "running"})
	return m.handlePrompt(text)
}

func sendTelegramReply(ch chan<- telegram.SubmitResponse, resp telegram.SubmitResponse) {
	if ch == nil {
		return
	}
	select {
	case ch <- resp:
	default:
	}
}
