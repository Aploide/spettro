package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"spettro/internal/telegram"
)

// dispatchTelegramEvent decides which observability events get mirrored to
// every bound Telegram chat. The default is concise: final assistant
// output, plans, comments, errors, banners, ask-user / approval notices.
// Tool traces are suppressed (they would spam the chat).
//
// Called from publishRemote. All outbound HTTP work happens on a worker
// goroutine inside telegramBroadcastAsync so the bubbletea Update loop is
// never blocked by a Bot API round trip.
func (m *Model) dispatchTelegramEvent(kind string, data map[string]any) {
	if m.telegramRelay == nil {
		return
	}
	if !m.telegramRelay.AnySubscriber() {
		return
	}
	switch kind {
	case "assistant_message":
		content, _ := data["content"].(string)
		if strings.TrimSpace(content) != "" {
			m.telegramBroadcastAsync(telegram.Prefix("◆", content))
		}
	case "assistant_error":
		errStr, _ := data["error"].(string)
		if errStr != "" {
			m.telegramBroadcastAsync(telegram.Prefix("⚠ error", errStr))
		}
	case "plan":
		plan, _ := data["plan"].(string)
		if strings.TrimSpace(plan) != "" {
			m.telegramBroadcastAsync(telegram.Prefix("▤ plan", plan))
		}
	case "plan_error":
		errStr, _ := data["error"].(string)
		if errStr != "" {
			m.telegramBroadcastAsync(telegram.Prefix("⚠ plan error", errStr))
		}
	case "comment":
		msg, _ := data["message"].(string)
		if strings.TrimSpace(msg) != "" {
			m.telegramBroadcastAsync(telegram.Prefix("❯", msg))
		}
	case "banner":
		level, _ := data["level"].(string)
		switch level {
		case "warn", "error":
			text, _ := data["text"].(string)
			if text != "" {
				m.telegramBroadcastAsync(telegram.Prefix("⚠", text))
			}
		}
	case "ask_user":
		question, _ := data["question"].(string)
		options, _ := data["options"].([]string)
		ctxStr, _ := data["context"].(string)
		def, _ := data["default"].(string)
		parts := []string{telegramQuestionHeading(data), question}
		if ctxStr != "" {
			parts = append(parts, "", ctxStr)
		}
		if len(options) > 0 {
			parts = append(parts, "", "Options:")
			for _, opt := range options {
				marker := "•"
				if def != "" && opt == def {
					marker = "★"
				}
				parts = append(parts, "  "+marker+" "+opt)
			}
		}
		parts = append(parts, "", "Reply here with your answer (free-text).")
		m.telegramBroadcastAsync(strings.Join(parts, "\n"))
		// Arm the answer router for every bound chat: the next non-slash
		// message resolves the dialog. ExpectAnswer is cheap (in-memory
		// map under a mutex) so we run it inline rather than on the
		// outbound worker goroutine.
		for _, chatID := range m.telegramRelay.BoundChats() {
			m.telegramRelay.ExpectAnswer(chatID, true)
		}
	case "approval_request":
		cmd, _ := data["command"].(string)
		reason, _ := data["reason"].(string)
		text := "‼ shell approval required\n  command: " + telegram.Truncate(cmd, 1000)
		if reason != "" {
			text += "\n  reason:  " + reason
		}
		text += "\n\nApprove or deny inside the TUI."
		m.telegramBroadcastAsync(text)
	case "commit":
		msg, _ := data["message"].(string)
		if msg != "" {
			m.telegramBroadcastAsync(telegram.Prefix("✓ commit", msg))
		}
	case "commit_error":
		errStr, _ := data["error"].(string)
		if errStr != "" {
			m.telegramBroadcastAsync(telegram.Prefix("✗ commit error", errStr))
		}
	case "state":
		if m.telegramRelay.Config().Verbose {
			reason, _ := data["reason"].(string)
			if reason != "" {
				m.telegramBroadcastAsync("[state] " + reason)
			}
		}
	case "tool":
		if !m.telegramRelay.Config().Verbose {
			return
		}
		name, _ := data["name"].(string)
		status, _ := data["status"].(string)
		if name == "" || status == "" {
			return
		}
		m.telegramBroadcastAsync(fmt.Sprintf("⚙ %s — %s", name, status))
	}
}

// telegramQuestionHeading is the first line of a forwarded ask-user prompt.
// The event carries the whole form, but this relay takes one answer at a time,
// so the heading says which question the chat is looking at — otherwise a
// three-question form reads as three unrelated interruptions.
func telegramQuestionHeading(data map[string]any) string {
	count, _ := data["count"].(int)
	if count <= 1 {
		return "❓ Spettro is asking:"
	}
	active, _ := data["active"].(int)
	return fmt.Sprintf("❓ Spettro is asking (question %d of %d):", active+1, count)
}

// telegramBroadcastAsync sends text on a worker goroutine so the TUI's
// Update loop is never blocked by a Bot API round trip. Errors surface
// through relay.LastSendError() and `/telegram status`.
func (m *Model) telegramBroadcastAsync(text string) {
	relay := m.telegramRelay
	if relay == nil || strings.TrimSpace(text) == "" {
		return
	}
	go relay.Broadcast(text)
}

// telegramClearAnswerExpectations is called when the pending ask-user
// dialog resolves (either via TUI input or via a Telegram answer), so the
// relay stops routing non-slash text as answers.
func (m *Model) telegramClearAnswerExpectations() {
	if m.telegramRelay == nil {
		return
	}
	for _, chatID := range m.telegramRelay.BoundChats() {
		m.telegramRelay.ExpectAnswer(chatID, false)
	}
}

// telegramAutostartDoneMsg delivers the result of the asynchronous relay
// startup (relay.Start performs a blocking getMe round-trip).
type telegramAutostartDoneMsg struct {
	relay *telegram.Relay
	err   error
}

// autostartTelegram is called once during New() when the user has previously
// enabled the relay. It constructs the relay (cheap, no network) and returns a
// tea.Cmd that performs the blocking relay.Start off the UI thread, delivering
// a telegramAutostartDoneMsg. Returns nil when autostart is disabled or no
// token is configured. Previously this ran relay.Start synchronously, freezing
// the very first paint for up to 15s on a slow getMe.
func (m *Model) autostartTelegram() tea.Cmd {
	cfg, err := telegram.LoadConfig()
	if err != nil {
		return nil
	}
	if !cfg.AutoStart {
		return nil
	}
	token, err := telegram.LoadToken()
	if err != nil || strings.TrimSpace(token) == "" {
		return nil
	}
	relay, err := telegram.NewRelay(telegram.Options{
		Token:       token,
		BotUsername: cfg.BotUsername,
		Config:      cfg,
	})
	if err != nil {
		return nil
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := relay.Start(ctx); err != nil {
			return telegramAutostartDoneMsg{err: err}
		}
		return telegramAutostartDoneMsg{relay: relay}
	}
}

// handleTelegramAutostartDone applies the async relay startup result: on
// success it adopts the relay and pumps its channels into the loop; on failure
// it surfaces a quiet system message (the user can still run /telegram start).
func (m Model) handleTelegramAutostartDone(msg telegramAutostartDoneMsg) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		m.pushSystemMsg("telegram autostart failed: " + msg.err.Error())
		m.refreshViewport()
		return m, nil
	}
	m.telegramRelay = msg.relay
	m.pushSystemMsg("telegram relay resumed — bot @" + msg.relay.BotUsername())
	m.refreshViewport()
	return m, tea.Batch(telegramListenCmds(msg.relay)...)
}
