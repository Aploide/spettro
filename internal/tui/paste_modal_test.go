package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// TestPasteReachesConnectKeyEntry verifies that bracketed-paste text lands in
// the textarea while the connect dialog is on the key-entry step. PasteMsg is
// not a KeyPressMsg, so it used to be dropped by the modal passthrough guard.
func TestPasteReachesConnectKeyEntry(t *testing.T) {
	m := NewModelForTesting()
	m.showConnect = true
	m.connectStep = 1
	m.connectProvider = "openai"
	m.ta.Focus()

	out, _ := m.update(tea.PasteMsg{Content: "sk-test-12345"})
	nm := out.(Model)
	if got := strings.TrimSpace(nm.ta.Value()); got != "sk-test-12345" {
		t.Fatalf("pasted key should reach the textarea, got %q", got)
	}
}

// TestPasteIgnoredOnConnectProviderList verifies paste does not leak into the
// provider-filter step (step 0) or the textarea while a list step is active.
func TestPasteIgnoredOnConnectProviderList(t *testing.T) {
	m := NewModelForTesting()
	m.showConnect = true
	m.connectStep = 0

	out, _ := m.update(tea.PasteMsg{Content: "sk-test-12345"})
	nm := out.(Model)
	if nm.connectFilter != "" {
		t.Fatalf("paste should not modify the provider filter, got %q", nm.connectFilter)
	}
	if got := strings.TrimSpace(nm.ta.Value()); got != "" {
		t.Fatalf("paste should not reach the textarea on the list step, got %q", got)
	}
}
