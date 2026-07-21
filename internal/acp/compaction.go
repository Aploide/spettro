package acp

import (
	"context"
	"fmt"
	"strings"

	acpsdk "github.com/coder/acp-go-sdk"

	"spettro/internal/compact"
	"spettro/internal/config"
	"spettro/internal/provider"
)

// sessionWindow resolves the active model's context window, falling back to
// the same default the runtime's in-loop compaction assumes, so pre-turn and
// in-loop pressure checks agree.
func (b *bridge) sessionWindow(cfg *config.UserConfig) int {
	if w := b.opts.Providers.ModelContext(cfg.ActiveProvider, cfg.ActiveModel); w > 0 {
		return w
	}
	return 128000
}

// evaluateSessionContext measures the session's carried history against the
// compaction policy. Caller must NOT hold b.mu.
func (b *bridge) evaluateSessionContext(s *acpSession, cfg *config.UserConfig) (compact.Evaluation, int) {
	b.mu.Lock()
	tokens := compact.EstimateHistoryTokens("", s.history)
	failures := s.autoCompactFailures
	b.mu.Unlock()
	eval := compact.Evaluate(b.sessionWindow(cfg), compact.Config{
		AutoEnabled:      cfg.AutoCompactEnabled,
		AutoThresholdPct: cfg.AutoCompactThresholdPct,
		MaxFailures:      cfg.AutoCompactMaxFailures,
	}, compact.State{TokensUsed: tokens, ConsecutiveFailures: failures})
	return eval, tokens
}

// ensureContextHeadroom runs before every prompt/goal turn, once the run slot
// is claimed. If the carried history already crowds the context window it
// compacts automatically (when auto compaction is enabled), or — when auto is
// off or paused after repeated failures — asks the user via
// session/request_permission BEFORE the window overflows, since past that
// point the compaction request itself no longer fits. Failures never block
// the turn: the runtime's in-loop compaction remains the last line of defense.
func (b *bridge) ensureContextHeadroom(ctx context.Context, s *acpSession, cfg *config.UserConfig, turn *turnState) {
	eval, tokens := b.evaluateSessionContext(s, cfg)
	switch {
	case eval.ShouldAutoCompact:
		turn.sessionUpdate(acpsdk.UpdateAgentMessageText(fmt.Sprintf(
			"context at ~%d/%d tokens — auto-compacting conversation history…", tokens, eval.EffectiveWindow)))
		turn.sessionUpdate(acpsdk.UpdateAgentMessageText(b.compactSession(ctx, s, cfg, false)))
	case eval.IsError:
		// Auto compaction disabled or paused, and the history is close enough
		// to the window that the next turn may not fit: this is the last
		// moment a compaction is still possible, so ask now.
		if b.askCompactPermission(ctx, turn, tokens, eval.EffectiveWindow) {
			turn.sessionUpdate(acpsdk.UpdateAgentMessageText(b.compactSession(ctx, s, cfg, true)))
		} else {
			turn.sessionUpdate(acpsdk.UpdateAgentMessageText(fmt.Sprintf(
				"⚠ continuing without compaction (%s) — the next turn may exceed the context window; run /compact to free space",
				eval.AutoDisabledReason)))
		}
	case eval.IsWarning && !cfg.AutoCompactEnabled:
		turn.sessionUpdate(acpsdk.UpdateAgentMessageText(fmt.Sprintf(
			"⚠ context at ~%d/%d tokens and auto compact is off — run /compact soon or /compact auto on", tokens, eval.EffectiveWindow)))
	}
}

// askCompactPermission surfaces the compact-or-continue decision through the
// editor's native permission prompt. Any error or cancellation means
// "continue without compacting" — never block the turn on it.
func (b *bridge) askCompactPermission(ctx context.Context, turn *turnState, tokens, window int) bool {
	resp, err := b.conn.RequestPermission(ctx, acpsdk.RequestPermissionRequest{
		SessionId: turn.sessionID,
		ToolCall: acpsdk.ToolCallUpdate{
			ToolCallId: turn.nextToolCallID("compact"),
			Title:      acpsdk.Ptr(fmt.Sprintf("Context nearly full (~%d/%d tokens). Compact conversation history now?", tokens, window)),
			Kind:       acpsdk.Ptr(acpsdk.ToolKindThink),
			Status:     acpsdk.Ptr(acpsdk.ToolCallStatusPending),
		},
		Options: []acpsdk.PermissionOption{
			{OptionId: "compact", Name: "Compact now", Kind: acpsdk.PermissionOptionKindAllowOnce},
			{OptionId: "continue", Name: "Continue without compacting", Kind: acpsdk.PermissionOptionKindRejectOnce},
		},
	})
	if err != nil || resp.Outcome.Cancelled != nil || resp.Outcome.Selected == nil {
		return false
	}
	return string(resp.Outcome.Selected.OptionId) == "compact"
}

// compactSession summarizes the session's carried structured history through
// the shared compact.CompactHistory core (the same cut/summarize logic the
// runtime uses in-loop) and swaps it in. Returns a user-facing result line.
func (b *bridge) compactSession(ctx context.Context, s *acpSession, cfg *config.UserConfig, force bool) string {
	b.mu.Lock()
	history := s.history
	failures := s.autoCompactFailures
	b.mu.Unlock()
	if len(history) == 0 {
		return "nothing to compact"
	}
	before := compact.EstimateHistoryTokens("", history)
	send := func(ctx context.Context, req provider.Request) (provider.Response, error) {
		return b.opts.Providers.Send(ctx, cfg.ActiveProvider, cfg.ActiveModel, req)
	}
	compacted, did, err := compact.CompactHistoryWithPolicy(ctx, send, "", history, b.sessionWindow(cfg), force, cfg.CompactConfig(), failures)
	if err != nil {
		b.mu.Lock()
		s.autoCompactFailures++
		b.mu.Unlock()
		return "compaction failed: " + err.Error()
	}
	if !did {
		return "history is small enough already; nothing was compacted"
	}
	b.mu.Lock()
	s.history = compacted
	s.autoCompactFailures = 0
	b.mu.Unlock()
	after := compact.EstimateHistoryTokens("", compacted)
	return fmt.Sprintf("compacted conversation history: ~%d → ~%d tokens", before, after)
}

// compactAutoReply resolves "/compact auto [status|on|off]" to its reply
// text, persisting the toggle to config (mirrors the TUI's
// handleCompactCommand). Split from handleCompactCommand so it is testable
// without an ACP connection.
func (b *bridge) compactAutoReply(s *acpSession, cfg *config.UserConfig, args []string) string {
	sub := "status"
	if len(args) > 0 {
		sub = strings.ToLower(args[0])
	}
	switch sub {
	case "status":
		state := "off"
		if cfg.AutoCompactEnabled {
			state = "on"
		}
		b.mu.Lock()
		failures := s.autoCompactFailures
		b.mu.Unlock()
		return fmt.Sprintf("auto compact: %s (threshold: %d%%, failures: %d/%d)",
			state, cfg.AutoCompactThresholdPct, failures, cfg.AutoCompactMaxFailures)
	case "on", "off":
		enabled := sub == "on"
		if _, err := config.Update(func(c *config.UserConfig) error {
			c.AutoCompactEnabled = enabled
			return nil
		}); err != nil {
			return "error: " + err.Error()
		}
		cfg.AutoCompactEnabled = enabled
		if enabled {
			b.mu.Lock()
			s.autoCompactFailures = 0
			b.mu.Unlock()
			return "auto compact enabled"
		}
		return "auto compact disabled — you will be asked before the context window fills"
	default:
		return "usage: /compact [auto <status|on|off>]"
	}
}

// handleCompactCommand implements /compact over ACP: bare "/compact" forces a
// summarization of the carried history inside the session's run slot;
// "/compact auto <status|on|off>" manages the auto-compaction toggle
// (mirrors the TUI's handleCompactCommand).
func (b *bridge) handleCompactCommand(ctx context.Context, s *acpSession, cfg *config.UserConfig, turn *turnState, input string) (acpsdk.PromptResponse, error) {
	reply := func(text string) (acpsdk.PromptResponse, error) {
		turn.sessionUpdate(acpsdk.UpdateAgentMessageText(text))
		return acpsdk.PromptResponse{StopReason: acpsdk.StopReasonEndTurn}, nil
	}

	args := strings.Fields(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(input), "/compact")))
	if len(args) > 0 && strings.EqualFold(args[0], "auto") {
		return reply(b.compactAutoReply(s, cfg, args[1:]))
	}

	// Forced compaction runs a provider call, so it needs the session's run
	// slot; mid-turn the in-flight run's in-loop compaction already manages
	// pressure, and racing it here would clobber its history.
	runCtx, finish, ok := b.beginRun(ctx, s)
	if !ok {
		return reply("a turn is running — the active run compacts automatically; retry /compact when it finishes")
	}
	defer finish()
	turn.ctx = runCtx
	return reply(b.compactSession(runCtx, s, cfg, true))
}
