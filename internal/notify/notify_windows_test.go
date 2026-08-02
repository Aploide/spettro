//go:build windows

package notify

import (
	"strings"
	"testing"
)

// The toast script is PowerShell embedded in a Go string, so nothing at
// compile time catches a typo in a WinRT type name or a template change. Run
// it for real and require a clean exit.
func TestToastCommandSucceeds(t *testing.T) {
	out, err := toastCommand("spettro test", "notification self-check").CombinedOutput()
	if err != nil {
		t.Fatalf("toast failed: %v\n%s", err, out)
	}
	if s := strings.TrimSpace(string(out)); s != "" {
		t.Errorf("toast wrote output, want none: %q", s)
	}
}

// Title and body reach PowerShell through the environment precisely so that
// hostile text cannot become code. Text that would break a naive interpolation
// must still deliver cleanly.
func TestToastHandlesHostileText(t *testing.T) {
	for _, body := range []string{
		`'; Write-Output pwned; '`,
		`quotes " and ' and backtick $(1+1)`,
		`xml <tags> & entities`,
		"caffè — 日本語 — ✅",
	} {
		out, err := toastCommand("spettro test", body).CombinedOutput()
		if err != nil {
			t.Errorf("body %q: %v\n%s", body, err, out)
			continue
		}
		if strings.Contains(string(out), "pwned") {
			t.Errorf("body %q: interpolated into the script", body)
		}
	}
}
