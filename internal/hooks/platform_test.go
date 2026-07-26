package hooks

import (
	"strings"
	"testing"

	"spettro/internal/platform"
)

func TestAvailableTracksExecCapability(t *testing.T) {
	if Available() != platform.CanExec() {
		t.Fatalf("Available() = %v, platform.CanExec() = %v — the gate has drifted", Available(), platform.CanExec())
	}
}

// The disabled response is what `/hooks` renders and what the tool loop
// receives. It must carry zero rules (a rule that cannot run must never reach
// runPreToolHooks, which aborts the turn on error) and must say why, so the
// user sees "disabled because ..." instead of an unexplained empty list.
func TestUnavailableConfigHasNoRulesAndExplainsItself(t *testing.T) {
	cfg := EffectiveConfig{Issues: []ValidationIssue{unavailableIssue()}}
	if len(cfg.Rules) != 0 {
		t.Fatalf("disabled config carries %d rules, want 0", len(cfg.Rules))
	}
	if len(cfg.Issues) != 1 {
		t.Fatalf("disabled config carries %d issues, want 1", len(cfg.Issues))
	}
	if got := cfg.Issues[0].Source; got != "platform" {
		t.Errorf("issue source = %q, want %q", got, "platform")
	}
	if !strings.HasPrefix(cfg.Issues[0].Message, "hooks are disabled") {
		t.Errorf("issue message %q does not lead with the fact that hooks are off", cfg.Issues[0].Message)
	}
}
