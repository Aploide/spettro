package tui_test

import (
	"strings"
	"testing"

	"spettro/internal/tui"
)

// TestDevinCommand_SetsOrgID verifies the /devin slash command persists a
// well-formed org id and rejects malformed input.
func TestDevinCommand_SetsOrgID(t *testing.T) {
	m := tui.NewModelForTesting()
	if got := m.DevinOrgIDForTesting(); got != "" {
		t.Fatalf("default devin org id should be empty, got %q", got)
	}
	out, _ := m.HandleCommandForTesting("/devin org-abc123")
	m = out.(tui.Model)
	if got := m.DevinOrgIDForTesting(); got != "org-abc123" {
		t.Fatalf("after /devin org-abc123, org = %q, want \"org-abc123\"", got)
	}

	// Malformed values should be rejected without overwriting the
	// previous valid setting.
	out, _ = m.HandleCommandForTesting("/devin not_an_org")
	m = out.(tui.Model)
	if got := m.DevinOrgIDForTesting(); got != "org-abc123" {
		t.Fatalf("malformed /devin overwrote a valid org id: now %q", got)
	}
	if !strings.Contains(m.BannerForTesting(), "should start with org-") {
		t.Fatalf("expected validation error banner, got %q", m.BannerForTesting())
	}
}
