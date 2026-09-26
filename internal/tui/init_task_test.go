package tui

import (
	"strings"
	"testing"
)

// TestInitTaskIsProjectAgnostic: SPETTRO.md is now loaded into every
// session's context, so /init must describe the user's project, not Spettro's
// own internals.
func TestInitTaskIsProjectAgnostic(t *testing.T) {
	for _, banned := range []string{"spettro.agents.toml", "internal/agent", "bubbletea", "internal/tui"} {
		if strings.Contains(initTask, banned) {
			t.Errorf("/init task mentions Spettro-specific %q", banned)
		}
	}
	for _, want := range []string{`path "SPETTRO.md"`, "Build, test & lint", "single test", "Conventions"} {
		if !strings.Contains(initTask, want) {
			t.Errorf("/init task missing %q", want)
		}
	}
}
