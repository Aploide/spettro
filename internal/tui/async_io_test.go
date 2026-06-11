package tui

import (
	"strings"
	"testing"

	"spettro/internal/provider"
)

// TestHandleLocalProbeDoneSuccess verifies the async local-endpoint probe
// finishes the connect: models registered, endpoint persisted, dialog closed.
func TestHandleLocalProbeDoneSuccess(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // isolate config writes

	m := NewModelForTesting()
	m.showConnect = true
	m.connectStep = 1
	m.connectProvider = localConnectProviderID

	models := []provider.Model{
		{Provider: "http://localhost:1234", Name: "local-model", Local: true},
	}
	out, _ := m.handleLocalProbeDone(localProbeDoneMsg{endpoint: "http://localhost:1234", models: models})
	nm, ok := out.(Model)
	if !ok {
		t.Fatal("handler should return a Model")
	}
	if nm.showConnect {
		t.Fatal("connect dialog should close on success")
	}
	if !nm.hasLocalEndpoint("http://localhost:1234") {
		t.Fatalf("endpoint should be persisted, have %v", nm.cfg.LocalEndpoints)
	}
	var found bool
	for _, mod := range nm.providers.Models() {
		if mod.Provider == "http://localhost:1234" && mod.Name == "local-model" {
			found = true
		}
	}
	if !found {
		t.Fatal("local model should be registered in the manager")
	}
}

// TestHandleLocalProbeDoneError verifies a failed probe keeps the dialog open
// and surfaces an error banner (no models registered).
func TestHandleLocalProbeDoneError(t *testing.T) {
	m := NewModelForTesting()
	m.showConnect = true
	m.connectStep = 1
	m.connectProvider = localConnectProviderID

	out, _ := m.handleLocalProbeDone(localProbeDoneMsg{endpoint: "http://bad", err: errProbe})
	nm, _ := out.(Model)
	if !nm.showConnect {
		t.Fatal("connect dialog should stay open on failure")
	}
	if nm.banner == "" {
		t.Fatal("expected an error banner on probe failure")
	}
}

// TestHandleLocalProbeDoneEmpty verifies an endpoint returning no models is
// treated as an error and does not close the dialog.
func TestHandleLocalProbeDoneEmpty(t *testing.T) {
	m := NewModelForTesting()
	m.showConnect = true
	m.connectStep = 1
	m.connectProvider = localConnectProviderID

	out, _ := m.handleLocalProbeDone(localProbeDoneMsg{endpoint: "http://localhost:1", models: nil})
	nm, _ := out.(Model)
	if !nm.showConnect {
		t.Fatal("empty model list should keep the dialog open")
	}
}

// TestHandleTelegramAutostartDoneError verifies an autostart failure surfaces a
// system message and leaves no relay attached.
func TestHandleTelegramAutostartDoneError(t *testing.T) {
	m := NewModelForTesting()
	out, _ := m.handleTelegramAutostartDone(telegramAutostartDoneMsg{err: errProbe})
	nm, _ := out.(Model)
	if nm.telegramRelay != nil {
		t.Fatal("no relay should be attached after a failed autostart")
	}
	var sawFailure bool
	for _, msg := range nm.messages {
		if msg.Role == RoleSystem && strings.Contains(msg.Content, "autostart failed") {
			sawFailure = true
		}
	}
	if !sawFailure {
		t.Fatal("expected an autostart-failure system message")
	}
}

type probeErr struct{}

func (probeErr) Error() string { return "boom" }

var errProbe = probeErr{}
