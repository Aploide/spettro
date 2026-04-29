package tui_test

import (
	"strings"
	"testing"

	"spettro/internal/tui"
)

func TestParseRemotePort(t *testing.T) {
	t.Parallel()

	cases := []struct {
		in   string
		want int
		err  bool
	}{
		{":8080", 8080, false},
		{"8080", 8080, false},
		{"  :7878  ", 7878, false},
		{":0", 0, true},
		{":99999", 0, true},
		{"abc", 0, true},
		{"", 0, true},
		{":", 0, true},
	}
	for _, tc := range cases {
		got, err := tui.ParseRemotePortForTesting(tc.in)
		if tc.err {
			if err == nil {
				t.Errorf("parseRemotePort(%q) expected error, got %d", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseRemotePort(%q) unexpected error: %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("parseRemotePort(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestRemoteCommand_StartStopLifecycle(t *testing.T) {
	m := tui.NewModelForTesting()

	// /remote with no port → should bind successfully and tell us the URL
	newModel, _ := m.HandleCommandForTesting("/remote")
	mm := newModel.(tui.Model)
	if !mm.HasRemoteServerForTesting() {
		t.Fatalf("expected remote server to be running after /remote")
	}
	addr := mm.RemoteAddressForTesting()
	if !strings.HasPrefix(addr, "http://127.0.0.1:") {
		t.Fatalf("expected loopback address, got %q", addr)
	}
	token := mm.RemoteTokenForTesting()
	if token == "" {
		t.Fatalf("expected non-empty bearer token")
	}

	// The TUI must actually push the URL + token to the chat. Without this,
	// the user has no way to discover the auth token after `/remote` runs.
	msgs := mm.MessagesForTesting()
	if len(msgs) == 0 {
		t.Fatalf("expected at least one chat message after /remote")
	}
	last := msgs[len(msgs)-1]
	if !strings.Contains(last.Content, token) {
		t.Fatalf("expected last message to contain bearer token %q, got %q", token, last.Content)
	}
	if !strings.Contains(last.Content, addr) {
		t.Fatalf("expected last message to contain address %q, got %q", addr, last.Content)
	}

	// /remote (again) → should warn that it's already running, leave server up
	newModel2, _ := mm.HandleCommandForTesting("/remote")
	mm2 := newModel2.(tui.Model)
	if !mm2.HasRemoteServerForTesting() {
		t.Fatalf("expected remote server to still be running")
	}
	if banner := mm2.BannerForTesting(); !strings.Contains(banner, "already") {
		t.Fatalf("expected 'already' banner, got %q", banner)
	}

	// /remote status → should re-print the URL and token to the chat
	newModelStatus, _ := mm2.HandleCommandForTesting("/remote status")
	mmStatus := newModelStatus.(tui.Model)
	statusMsgs := mmStatus.MessagesForTesting()
	statusLast := statusMsgs[len(statusMsgs)-1]
	if !strings.Contains(statusLast.Content, token) || !strings.Contains(statusLast.Content, "running") {
		t.Fatalf("expected /remote status to print token and 'running', got %q", statusLast.Content)
	}

	// /remote stop → server should go away
	newModel3, _ := mmStatus.HandleCommandForTesting("/remote stop")
	mm3 := newModel3.(tui.Model)
	if mm3.HasRemoteServerForTesting() {
		t.Fatalf("expected remote server to be stopped")
	}
}

// TestRemoteCommand_TokenIsVisibleInViewport regression-tests the bug where
// /remote pushed a system message containing the bearer token but never
// refreshed the viewport, so the user only saw the banner and never the
// token. The fix is to call m.refreshViewport() inside handleRemoteCommand.
func TestRemoteCommand_TokenIsVisibleInViewport(t *testing.T) {
	m := tui.NewModelForTesting()
	// Bypass the first-run trust dialog so View() shows the chat viewport.
	m.MarkReadyAndTrustedForTesting()
	m.SetDimensionsForTesting(140, 40)
	m = m.RecalcLayoutForTesting()

	updated, _ := m.HandleCommandForTesting("/remote")
	mm := updated.(tui.Model)
	if !mm.HasRemoteServerForTesting() {
		t.Fatalf("expected remote server to be running")
	}
	token := mm.RemoteTokenForTesting()
	if token == "" {
		t.Fatalf("expected non-empty bearer token")
	}

	view := mm.ViewForTesting()
	if !strings.Contains(view, token) {
		t.Fatalf("bearer token %q is not visible in the rendered TUI view\n--- view ---\n%s\n---", token, view)
	}
}

func TestRemoteCommand_StopWhenNotRunning(t *testing.T) {
	m := tui.NewModelForTesting()
	newModel, _ := m.HandleCommandForTesting("/remote stop")
	mm := newModel.(tui.Model)
	if mm.HasRemoteServerForTesting() {
		t.Fatalf("expected no remote server")
	}
	if banner := mm.BannerForTesting(); !strings.Contains(banner, "not running") {
		t.Fatalf("expected 'not running' banner, got %q", banner)
	}
}

func TestRemoteCommand_InvalidPort(t *testing.T) {
	m := tui.NewModelForTesting()
	newModel, _ := m.HandleCommandForTesting("/remote :nonsense")
	mm := newModel.(tui.Model)
	if mm.HasRemoteServerForTesting() {
		t.Fatalf("expected no remote server when port is invalid")
	}
	if banner := mm.BannerForTesting(); !strings.Contains(banner, "remote:") {
		t.Fatalf("expected 'remote:' error banner, got %q", banner)
	}
}
