package acp

import (
	"strings"
	"testing"

	"spettro/internal/config"
	"spettro/internal/provider"
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
	_, _, handled := handleSlashCommand(s, &cfg, provider.NewManager(), "/nope")
	if handled {
		t.Fatalf("expected /nope to be unhandled so it falls through to the LLM")
	}
}

func TestHandleSlashCommand_Help(t *testing.T) {
	s := testSession(t)
	cfg := config.UserConfig{}
	reply, modeChanged, handled := handleSlashCommand(s, &cfg, provider.NewManager(), "/help")
	if !handled || modeChanged || !strings.Contains(reply, "/permission") {
		t.Fatalf("unexpected /help result: reply=%q modeChanged=%v handled=%v", reply, modeChanged, handled)
	}
}

func TestHandleSlashCommand_Mode(t *testing.T) {
	s := testSession(t)
	cfg := config.UserConfig{}

	reply, modeChanged, handled := handleSlashCommand(s, &cfg, provider.NewManager(), "/mode coding")
	if !handled || !modeChanged {
		t.Fatalf("expected /mode coding to be handled and change mode, got reply=%q modeChanged=%v handled=%v", reply, modeChanged, handled)
	}
	if s.agentID != "coding" {
		t.Fatalf("expected agentID coding, got %q", s.agentID)
	}

	reply, modeChanged, handled = handleSlashCommand(s, &cfg, provider.NewManager(), "/mode bogus")
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

	reply, _, handled := handleSlashCommand(s, &cfg, provider.NewManager(), "/permission yolo")
	if !handled || !strings.Contains(reply, "yolo") {
		t.Fatalf("expected /permission yolo to succeed, got reply=%q handled=%v", reply, handled)
	}
	if cfg.Permission != config.PermissionYOLO {
		t.Fatalf("expected cfg.Permission=yolo, got %q", cfg.Permission)
	}

	reply, _, handled = handleSlashCommand(s, &cfg, provider.NewManager(), "/permission bogus")
	if !handled || !strings.Contains(reply, "invalid permission") {
		t.Fatalf("expected /permission bogus to be rejected, got reply=%q handled=%v", reply, handled)
	}
}

func TestHandleSlashCommand_Models(t *testing.T) {
	s := testSession(t)
	cfg := config.UserConfig{ActiveProvider: "openai", ActiveModel: "gpt-4o"}
	pm := provider.NewManager()

	reply, _, handled := handleSlashCommand(s, &cfg, pm, "/models")
	if !handled || !strings.Contains(reply, "openai:gpt-4o") {
		t.Fatalf("expected /models to show the current model, got reply=%q handled=%v", reply, handled)
	}

	reply, _, handled = handleSlashCommand(s, &cfg, pm, "/models bogus:model")
	if !handled || !strings.Contains(reply, "unknown model") {
		t.Fatalf("expected unknown model to be rejected, got reply=%q handled=%v", reply, handled)
	}

	reply, _, handled = handleSlashCommand(s, &cfg, pm, "/models nocolon")
	if !handled || !strings.Contains(reply, "usage:") {
		t.Fatalf("expected malformed /models arg to show usage, got reply=%q handled=%v", reply, handled)
	}
}

func TestHandleSlashCommand_Clear(t *testing.T) {
	s := testSession(t)
	s.history = []provider.Message{
		{Role: provider.RoleUser, Content: "hi"},
		{Role: provider.RoleAssistant, Content: "hello"},
	}
	cfg := config.UserConfig{}

	reply, _, handled := handleSlashCommand(s, &cfg, provider.NewManager(), "/clear")
	if !handled || reply == "" {
		t.Fatalf("expected /clear to be handled, got reply=%q handled=%v", reply, handled)
	}
	if s.history != nil {
		t.Fatalf("expected history to be cleared, got %v", s.history)
	}
}

func TestHandleSlashCommand_Memory(t *testing.T) {
	s := testSession(t)
	s.cwd = t.TempDir()
	cfg := config.UserConfig{}
	pm := provider.NewManager()

	reply, _, handled := handleSlashCommand(s, &cfg, pm, "/memory")
	if !handled || !strings.Contains(reply, "user memory") || !strings.Contains(reply, "empty") {
		t.Fatalf("expected empty memory listing, got handled=%v reply=%q", handled, reply)
	}

	reply, _, handled = handleSlashCommand(s, &cfg, pm, "/memory add project prefers tabs")
	if !handled || !strings.Contains(reply, "saved to project memory") {
		t.Fatalf("expected project save confirmation, got handled=%v reply=%q", handled, reply)
	}
	reply, _, _ = handleSlashCommand(s, &cfg, pm, "/memory show")
	if !strings.Contains(reply, "prefers tabs") {
		t.Fatalf("expected saved fact in show output, got %q", reply)
	}

	reply, _, _ = handleSlashCommand(s, &cfg, pm, "/memory add")
	if !strings.Contains(reply, "usage: /memory add") {
		t.Fatalf("expected add usage, got %q", reply)
	}

	reply, _, _ = handleSlashCommand(s, &cfg, pm, "/memory clear all")
	if !strings.Contains(reply, "memory cleared") {
		t.Fatalf("expected clear confirmation, got %q", reply)
	}
	reply, _, _ = handleSlashCommand(s, &cfg, pm, "/memory")
	if strings.Contains(reply, "prefers tabs") {
		t.Fatalf("expected fact gone after clear, got %q", reply)
	}

	reply, _, _ = handleSlashCommand(s, &cfg, pm, "/memory bogus")
	if !strings.Contains(reply, "usage: /memory") {
		t.Fatalf("expected usage message, got %q", reply)
	}
}
