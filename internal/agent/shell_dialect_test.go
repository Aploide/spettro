package agent

import (
	"strings"
	"testing"

	"spettro/internal/shell"
)

// On a PowerShell host the shell tool's description and the hints for
// reading truncated instruction files must not teach POSIX-only syntax:
// `&&` is a parse error in Windows PowerShell 5.1 and there is no sed.
func TestShellGuidanceFollowsTheDialect(t *testing.T) {
	ps := shellToolDescFor(shell.KindPowerShell)
	if strings.Contains(ps, "cd web && npm test") || !strings.Contains(ps, "Set-Location web; npm test") {
		t.Fatalf("PowerShell tool description teaches &&: %q", ps)
	}
	if !strings.Contains(shellToolDescFor(shell.KindPOSIX), "cd web && npm test") {
		t.Fatal("POSIX description lost its chaining example")
	}
	if h := shellReadCommand(shell.KindPowerShell); strings.Contains(h, "sed") || !strings.Contains(h, "Get-Content") {
		t.Fatalf("PowerShell read hint = %q", h)
	}
	if h := shellReadCommand(shell.KindPOSIX); !strings.Contains(h, "sed -n") {
		t.Fatalf("POSIX read hint = %q", h)
	}
}
