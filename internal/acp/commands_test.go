package acp

import (
	"strings"
	"testing"

	"spettro/internal/config"
)

func testSession(t *testing.T) *acpSession {
	t.Helper()
	// handleSlashCommand persists config via config.Update, which writes to
	// $HOME/.spettro/config.json; sandbox HOME so tests never touch the
	// developer's real config.
	t.Setenv("HOME", t.TempDir())
	return &acpSession{
		agentID: "plan",
		manifest: config.AgentManifest{
			Agents: []config.AgentSpec{
				{ID: "plan", Enabled: true},
				{ID: "coding", Enabled: true},
			},
		},
	}
}

func TestHandleSlashCommand_Unrecognized(t *testing.T) {
	s := testSession(t)
	cfg := config.UserConfig{}
	_, _, handled := handleSlashCommand(s, &cfg, "/nope")
	if handled {
		t.Fatalf("expected /nope to be unhandled so it falls through to the LLM")
	}
}

func TestHandleSlashCommand_Help(t *testing.T) {
	s := testSession(t)
	cfg := config.UserConfig{}
	reply, modeChanged, handled := handleSlashCommand(s, &cfg, "/help")
	if !handled || modeChanged || !strings.Contains(reply, "/permission") {
		t.Fatalf("unexpected /help result: reply=%q modeChanged=%v handled=%v", reply, modeChanged, handled)
	}
}

func TestHandleSlashCommand_Mode(t *testing.T) {
	s := testSession(t)
	cfg := config.UserConfig{}

	reply, modeChanged, handled := handleSlashCommand(s, &cfg, "/mode coding")
	if !handled || !modeChanged {
		t.Fatalf("expected /mode coding to be handled and change mode, got reply=%q modeChanged=%v handled=%v", reply, modeChanged, handled)
	}
	if s.agentID != "coding" {
		t.Fatalf("expected agentID coding, got %q", s.agentID)
	}

	reply, modeChanged, handled = handleSlashCommand(s, &cfg, "/mode bogus")
	if !handled || modeChanged || !strings.Contains(reply, "unknown mode") {
		t.Fatalf("expected /mode bogus to be rejected, got reply=%q modeChanged=%v handled=%v", reply, modeChanged, handled)
	}
	if s.agentID != "coding" {
		t.Fatalf("agentID should be unchanged after invalid /mode, got %q", s.agentID)
	}
}

func TestHandleSlashCommand_Permission(t *testing.T) {
	s := testSession(t)
	cfg := config.UserConfig{}

	reply, _, handled := handleSlashCommand(s, &cfg, "/permission yolo")
	if !handled || !strings.Contains(reply, "yolo") {
		t.Fatalf("expected /permission yolo to succeed, got reply=%q handled=%v", reply, handled)
	}
	if cfg.Permission != config.PermissionYOLO {
		t.Fatalf("expected cfg.Permission=yolo, got %q", cfg.Permission)
	}

	reply, _, handled = handleSlashCommand(s, &cfg, "/permission bogus")
	if !handled || !strings.Contains(reply, "invalid permission") {
		t.Fatalf("expected /permission bogus to be rejected, got reply=%q handled=%v", reply, handled)
	}
}

func TestHandleSlashCommand_Clear(t *testing.T) {
	s := testSession(t)
	s.history = []string{"user: hi", "assistant: hello"}
	cfg := config.UserConfig{}

	reply, _, handled := handleSlashCommand(s, &cfg, "/clear")
	if !handled || reply == "" {
		t.Fatalf("expected /clear to be handled, got reply=%q handled=%v", reply, handled)
	}
	if s.history != nil {
		t.Fatalf("expected history to be cleared, got %v", s.history)
	}
}
