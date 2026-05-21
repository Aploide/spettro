package telegram_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"spettro/internal/telegram"
)

// startRelay builds a relay wired to the fake Bot API server and starts it.
// Each test is responsible for stopping the relay before it exits (we use
// t.Cleanup so a panic does not leak the poll goroutine).
func startRelay(t *testing.T, fb *fakeBot, cfg telegram.PersistedConfig) *telegram.Relay {
	t.Helper()
	client := telegram.NewBotClient("test-token", telegram.WithBaseURL(fb.URL()))
	r, err := telegram.NewRelay(telegram.Options{
		Token:  "test-token",
		Client: client,
		Config: cfg,
	})
	if err != nil {
		t.Fatalf("NewRelay: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := r.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(r.Stop)
	return r
}

// TestRelay_PromptFromAllowlistedUser exercises the happy path: an
// authorised user messages the bot, the relay forwards a SubmitRequest,
// the TUI stand-in ACKs it, and the bot replies with a confirmation.
func TestRelay_PromptFromAllowlistedUser(t *testing.T) {
	fb := newFakeBot(t)
	cfg := telegram.PersistedConfig{
		Allowlist: []telegram.AllowEntry{{Username: "carlo"}},
	}
	r := startRelay(t, fb, cfg)

	// Queue one inbound update.
	fb.pushUpdate(telegram.Update{
		UpdateID: 1,
		Message: &telegram.Message{
			MessageID: 100,
			Date:      time.Now().Unix(),
			Text:      "explain the budget package",
			From:      &telegram.User{ID: 999, Username: "Carlo"},
			Chat:      &telegram.Chat{ID: 999, Type: "private"},
		},
	})

	select {
	case req := <-r.Submissions():
		if req.Message != "explain the budget package" {
			t.Fatalf("unexpected message: %q", req.Message)
		}
		if req.Kind != telegram.SubmitPrompt {
			t.Fatalf("kind = %s, want %s", req.Kind, telegram.SubmitPrompt)
		}
		req.Reply <- telegram.SubmitResponse{Accepted: true, Note: "running"}
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for submission")
	}

	// Wait for the relay to send the ack.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if len(fb.sentMessages()) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	sent := fb.sentMessages()
	if len(sent) == 0 {
		t.Fatal("expected an ack to be sent")
	}
	if sent[0].ChatID != 999 {
		t.Fatalf("ack went to wrong chat: %d", sent[0].ChatID)
	}
	if !strings.Contains(sent[0].Text, "running") {
		t.Fatalf("ack text did not include 'running': %q", sent[0].Text)
	}
}

// TestRelay_DeniedUser ensures messages from non-allowlisted senders are
// rejected without leaking to the TUI.
func TestRelay_DeniedUser(t *testing.T) {
	fb := newFakeBot(t)
	r := startRelay(t, fb, telegram.PersistedConfig{
		Allowlist: []telegram.AllowEntry{{Username: "carlo"}},
	})

	fb.pushUpdate(telegram.Update{
		UpdateID: 1,
		Message: &telegram.Message{
			MessageID: 100,
			Text:      "should be rejected",
			From:      &telegram.User{ID: 17, Username: "intruder"},
			Chat:      &telegram.Chat{ID: 17, Type: "private"},
		},
	})

	select {
	case req := <-r.Submissions():
		t.Fatalf("unexpected submission from denied chat: %+v", req)
	case <-time.After(500 * time.Millisecond):
		// Expected: no submission.
	}
	sent := fb.sentMessages()
	if len(sent) == 0 {
		t.Fatal("expected a refusal reply")
	}
	if !strings.Contains(sent[0].Text, "not allowed") {
		t.Fatalf("refusal text unexpected: %q", sent[0].Text)
	}
}

// TestRelay_BotCommand_Cancel ensures /cancel never reaches the TUI but
// produces an interrupt signal.
func TestRelay_BotCommand_Cancel(t *testing.T) {
	fb := newFakeBot(t)
	r := startRelay(t, fb, telegram.PersistedConfig{
		Allowlist: []telegram.AllowEntry{{ChatID: 500}},
	})

	fb.pushUpdate(telegram.Update{
		UpdateID: 7,
		Message: &telegram.Message{
			Text: "/cancel",
			From: &telegram.User{ID: 500},
			Chat: &telegram.Chat{ID: 500, Type: "private"},
		},
	})

	select {
	case <-r.Interrupts():
		// Expected.
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for interrupt")
	}
	select {
	case req := <-r.Submissions():
		t.Fatalf("/cancel should not become a submission: %+v", req)
	case <-time.After(200 * time.Millisecond):
	}
}

// TestRelay_BotCommand_Help replies with help text and never submits.
func TestRelay_BotCommand_Help(t *testing.T) {
	fb := newFakeBot(t)
	r := startRelay(t, fb, telegram.PersistedConfig{
		Allowlist: []telegram.AllowEntry{{ChatID: 500}},
	})

	fb.pushUpdate(telegram.Update{
		UpdateID: 7,
		Message: &telegram.Message{
			Text: "/help",
			From: &telegram.User{ID: 500},
			Chat: &telegram.Chat{ID: 500, Type: "private"},
		},
	})
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(fb.sentMessages()) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	sent := fb.sentMessages()
	if len(sent) == 0 {
		t.Fatal("expected a help reply")
	}
	if !strings.Contains(sent[0].Text, "Spettro Telegram relay") {
		t.Fatalf("unexpected help text: %q", sent[0].Text)
	}
	select {
	case req := <-r.Submissions():
		t.Fatalf("/help should not become a submission: %+v", req)
	case <-time.After(100 * time.Millisecond):
		// Expected.
	}
	_ = r
}

// TestRelay_FreeTextAnswer routes plain text to SubmitAnswer when
// ExpectAnswer was armed.
func TestRelay_FreeTextAnswer(t *testing.T) {
	fb := newFakeBot(t)
	r := startRelay(t, fb, telegram.PersistedConfig{
		Allowlist: []telegram.AllowEntry{{ChatID: 8}},
	})

	// Bind the chat by pushing a first message + ACK; this also makes the
	// chat eligible for ExpectAnswer (we key on bound chats).
	fb.pushUpdate(telegram.Update{
		UpdateID: 1,
		Message: &telegram.Message{
			Text: "first prompt",
			From: &telegram.User{ID: 8},
			Chat: &telegram.Chat{ID: 8, Type: "private"},
		},
	})
	select {
	case req := <-r.Submissions():
		req.Reply <- telegram.SubmitResponse{Accepted: true}
	case <-time.After(2 * time.Second):
		t.Fatal("no first submission")
	}

	r.ExpectAnswer(8, true)

	fb.pushUpdate(telegram.Update{
		UpdateID: 2,
		Message: &telegram.Message{
			Text: "yes please",
			From: &telegram.User{ID: 8},
			Chat: &telegram.Chat{ID: 8, Type: "private"},
		},
	})
	select {
	case req := <-r.Submissions():
		if req.Kind != telegram.SubmitAnswer {
			t.Fatalf("expected SubmitAnswer, got %s", req.Kind)
		}
		if req.Message != "yes please" {
			t.Fatalf("unexpected text: %q", req.Message)
		}
		req.Reply <- telegram.SubmitResponse{Accepted: true, Note: "answered"}
	case <-time.After(2 * time.Second):
		t.Fatal("no answer submission")
	}
}

// TestRelay_PersistsLastUpdateID writes back the offset after each update.
func TestRelay_PersistsLastUpdateID(t *testing.T) {
	fb := newFakeBot(t)
	r := startRelay(t, fb, telegram.PersistedConfig{
		Allowlist: []telegram.AllowEntry{{ChatID: 1}},
	})

	fb.pushUpdate(telegram.Update{
		UpdateID: 42,
		Message: &telegram.Message{
			Text: "hi",
			From: &telegram.User{ID: 1},
			Chat: &telegram.Chat{ID: 1, Type: "private"},
		},
	})

	select {
	case req := <-r.Submissions():
		req.Reply <- telegram.SubmitResponse{Accepted: true}
	case <-time.After(2 * time.Second):
		t.Fatal("no submission")
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if r.Config().LastUpdateID == 42 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("last update id never updated: got %d", r.Config().LastUpdateID)
}

// TestRelay_AllowlistMutate is a unit check on add/remove semantics.
func TestRelay_AllowlistMutate(t *testing.T) {
	cfg := telegram.PersistedConfig{}
	cfg, added := telegram.AddAllowEntry(cfg, telegram.AllowEntry{Username: "Alice"})
	if !added || len(cfg.Allowlist) != 1 {
		t.Fatalf("first add failed: %#v", cfg.Allowlist)
	}
	cfg, added = telegram.AddAllowEntry(cfg, telegram.AllowEntry{Username: "alice"})
	if added {
		t.Fatalf("duplicate add should be a no-op")
	}
	cfg, added = telegram.AddAllowEntry(cfg, telegram.AllowEntry{ChatID: 99})
	if !added || len(cfg.Allowlist) != 2 {
		t.Fatalf("second add failed: %#v", cfg.Allowlist)
	}
	cfg, removed := telegram.RemoveAllowEntry(cfg, "ALICE", 0)
	if removed != 1 || len(cfg.Allowlist) != 1 {
		t.Fatalf("remove failed: removed=%d cfg=%#v", removed, cfg.Allowlist)
	}
	cfg, removed = telegram.RemoveAllowEntry(cfg, "", 99)
	if removed != 1 || len(cfg.Allowlist) != 0 {
		t.Fatalf("remove by id failed: removed=%d cfg=%#v", removed, cfg.Allowlist)
	}
}

// TestRelay_IsAllowed_MatchModes covers the three identity sources.
func TestRelay_IsAllowed_MatchModes(t *testing.T) {
	cfg := telegram.PersistedConfig{
		Allowlist: []telegram.AllowEntry{
			{Username: "carlo"},
			{ChatID: -10012345},
		},
	}
	if !telegram.IsAllowed(cfg, "carlo", 1, 1) {
		t.Fatal("username match failed")
	}
	if !telegram.IsAllowed(cfg, "Carlo", 0, 0) {
		t.Fatal("case-insensitive username match failed")
	}
	if !telegram.IsAllowed(cfg, "", 0, -10012345) {
		t.Fatal("chat id match failed")
	}
	if telegram.IsAllowed(cfg, "evilbot", 9999, 9999) {
		t.Fatal("unknown sender should not match")
	}
}

// TestRelay_StopIsIdempotent ensures repeated stops don't panic.
func TestRelay_StopIsIdempotent(t *testing.T) {
	fb := newFakeBot(t)
	r := startRelay(t, fb, telegram.PersistedConfig{})
	r.Stop()
	r.Stop()
}

// TestRelay_BroadcastChunksLongOutput verifies that very long outbound
// messages are split into multiple sendMessage calls.
func TestRelay_BroadcastChunksLongOutput(t *testing.T) {
	fb := newFakeBot(t)
	r := startRelay(t, fb, telegram.PersistedConfig{
		Allowlist: []telegram.AllowEntry{{ChatID: 11}},
	})

	// Bind by issuing one inbound message.
	fb.pushUpdate(telegram.Update{
		UpdateID: 1,
		Message: &telegram.Message{
			Text: "bind",
			From: &telegram.User{ID: 11},
			Chat: &telegram.Chat{ID: 11, Type: "private"},
		},
	})
	select {
	case req := <-r.Submissions():
		req.Reply <- telegram.SubmitResponse{Accepted: true}
	case <-time.After(2 * time.Second):
		t.Fatal("no bind submission")
	}

	long := strings.Repeat("xyz ", 2000)
	before := len(fb.sentMessages())
	r.Broadcast(long)

	if got := len(fb.sentMessages()) - before; got < 2 {
		t.Fatalf("expected >=2 sends, got %d", got)
	}
}

// TestRelay_GetUpdatesErrorBackoff ensures the relay does not get stuck
// when getUpdates fails. After the failure clears, polling resumes.
func TestRelay_GetUpdatesErrorBackoff(t *testing.T) {
	fb := newFakeBot(t)
	fb.setError("getUpdates", "temporary failure")
	r := startRelay(t, fb, telegram.PersistedConfig{
		Allowlist: []telegram.AllowEntry{{ChatID: 1}},
	})
	// Wait until the error surfaces.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if r.LastError() != nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if r.LastError() == nil {
		t.Fatalf("expected LastError to be set after upstream failure")
	}
	// Clear the failure and push a real update; expect a submission.
	fb.setError("getUpdates", "")
	fb.pushUpdate(telegram.Update{
		UpdateID: 1,
		Message: &telegram.Message{
			Text: "after recovery",
			From: &telegram.User{ID: 1},
			Chat: &telegram.Chat{ID: 1, Type: "private"},
		},
	})
	select {
	case req := <-r.Submissions():
		req.Reply <- telegram.SubmitResponse{Accepted: true}
	case <-time.After(10 * time.Second):
		t.Fatal("timeout waiting for recovery submission")
	}
}
