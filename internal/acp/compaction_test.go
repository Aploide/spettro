package acp

import (
	"context"
	"strings"
	"testing"

	"spettro/internal/config"
	"spettro/internal/provider"
)

func testBridge(t *testing.T) *bridge {
	t.Helper()
	// compactAutoReply persists via config.Update; sandbox HOME so tests
	// never touch the developer's real config.
	t.Setenv("HOME", t.TempDir())
	return newBridge(Options{Providers: provider.NewManager()})
}

// bigHistory builds a structured history whose token estimate exceeds the
// default 128k window's auto-compact threshold.
func bigHistory(turns, charsPerTurn int) []provider.Message {
	msgs := make([]provider.Message, 0, turns)
	for i := 0; i < turns; i++ {
		role := provider.RoleUser
		if i%2 == 1 {
			role = provider.RoleAssistant
		}
		msgs = append(msgs, provider.Message{Role: role, Content: strings.Repeat("x", charsPerTurn)})
	}
	return msgs
}

func TestEvaluateSessionContext_SmallHistoryNoPressure(t *testing.T) {
	b := testBridge(t)
	s := &acpSession{history: bigHistory(6, 400)}
	cfg := config.Default()
	eval, _ := b.evaluateSessionContext(s, &cfg)
	if eval.ShouldAutoCompact || eval.IsWarning || eval.IsError {
		t.Fatalf("expected no context pressure on a small history, got %+v", eval)
	}
}

func TestEvaluateSessionContext_AutoCompactFiresBeforeOverflow(t *testing.T) {
	b := testBridge(t)
	// ~96k estimated tokens against the 128k default window (108k effective):
	// above the 85% auto threshold but still inside the window — compaction
	// is possible.
	s := &acpSession{history: bigHistory(24, 16_000)}
	cfg := config.Default()
	eval, tokens := b.evaluateSessionContext(s, &cfg)
	if !eval.ShouldAutoCompact {
		t.Fatalf("expected ShouldAutoCompact at ~%d/%d tokens", tokens, eval.EffectiveWindow)
	}
	if tokens >= eval.EffectiveWindow {
		t.Fatalf("guard must fire BEFORE the window is exceeded: %d >= %d", tokens, eval.EffectiveWindow)
	}
}

func TestEvaluateSessionContext_AutoOffStillReportsError(t *testing.T) {
	b := testBridge(t)
	s := &acpSession{history: bigHistory(24, 20_000)}
	cfg := config.Default()
	cfg.AutoCompactEnabled = false
	eval, _ := b.evaluateSessionContext(s, &cfg)
	if eval.ShouldAutoCompact {
		t.Fatal("auto compact must not fire when disabled")
	}
	if !eval.IsError {
		t.Fatal("expected error-level pressure so the user gets asked before overflow")
	}
}

func TestEvaluateSessionContext_PausedAfterRepeatedFailures(t *testing.T) {
	b := testBridge(t)
	cfg := config.Default()
	s := &acpSession{history: bigHistory(24, 20_000), autoCompactFailures: cfg.AutoCompactMaxFailures}
	eval, _ := b.evaluateSessionContext(s, &cfg)
	if eval.ShouldAutoCompact {
		t.Fatal("auto compact must pause after repeated failures")
	}
	if eval.AutoDisabledReason == "" {
		t.Fatal("expected a disabled reason for the user-facing warning")
	}
}

func TestCompactSession_EmptyHistory(t *testing.T) {
	b := testBridge(t)
	s := &acpSession{}
	cfg := config.Default()
	if got := b.compactSession(context.Background(), s, &cfg, true); got != "nothing to compact" {
		t.Fatalf("unexpected reply: %q", got)
	}
}

func TestCompactSession_FailureCountsAndKeepsHistory(t *testing.T) {
	b := testBridge(t)
	s := &acpSession{history: bigHistory(10, 400)}
	cfg := config.Default() // no provider keys: the summarizer call must fail
	got := b.compactSession(context.Background(), s, &cfg, true)
	if !strings.HasPrefix(got, "compaction failed:") {
		t.Fatalf("expected failure reply, got %q", got)
	}
	if s.autoCompactFailures != 1 {
		t.Fatalf("expected failure counter 1, got %d", s.autoCompactFailures)
	}
	if len(s.history) != 10 {
		t.Fatalf("history must be untouched on failure, got %d messages", len(s.history))
	}
}

func TestCompactAutoReply_ToggleAndStatus(t *testing.T) {
	b := testBridge(t)
	s := &acpSession{autoCompactFailures: 2}
	cfg := config.Default()

	if got := b.compactAutoReply(s, &cfg, nil); !strings.Contains(got, "auto compact: on") {
		t.Fatalf("unexpected status reply: %q", got)
	}
	if got := b.compactAutoReply(s, &cfg, []string{"off"}); !strings.Contains(got, "disabled") {
		t.Fatalf("unexpected off reply: %q", got)
	}
	if cfg.AutoCompactEnabled {
		t.Fatal("expected AutoCompactEnabled=false after /compact auto off")
	}
	if got := b.compactAutoReply(s, &cfg, []string{"on"}); got != "auto compact enabled" {
		t.Fatalf("unexpected on reply: %q", got)
	}
	if !cfg.AutoCompactEnabled || s.autoCompactFailures != 0 {
		t.Fatalf("expected enabled=true and failures reset, got enabled=%v failures=%d",
			cfg.AutoCompactEnabled, s.autoCompactFailures)
	}
	// The toggle must persist for future sessions/turns.
	if fresh, err := config.LoadFull(); err != nil || !fresh.AutoCompactEnabled {
		t.Fatalf("expected persisted AutoCompactEnabled=true, err=%v", err)
	}
	if got := b.compactAutoReply(s, &cfg, []string{"bogus"}); !strings.Contains(got, "usage:") {
		t.Fatalf("unexpected reply for bogus arg: %q", got)
	}
}
