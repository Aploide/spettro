package tui

import (
	"errors"
	"strings"
	"testing"

	"spettro/internal/update"
	"spettro/internal/version"
)

// setTestVersion makes the running version v for one test.
func setTestVersion(t *testing.T, v string) {
	t.Helper()
	orig := version.App
	version.App = v
	t.Cleanup(func() { version.App = orig })
}

// With no update pending (the startup check may be a day old), /update runs
// a live check instead of answering "already up to date".
func TestUpdateCommandChecksLiveWhenNothingPending(t *testing.T) {
	m := NewModelForTesting()
	out, cmd := m.runUpdateCommand()
	nm := out.(Model)
	if cmd == nil || !nm.updateBusy {
		t.Fatalf("no live check started (busy=%v)", nm.updateBusy)
	}
	if strings.Contains(nm.banner, "up to date") {
		t.Fatalf("banner %q claims up to date before checking", nm.banner)
	}
}

func TestExplicitUpdateCheckOutcomes(t *testing.T) {
	setTestVersion(t, "v1.2.0")

	t.Run("up to date", func(t *testing.T) {
		m := NewModelForTesting()
		m.updateBusy = true
		out, cmd := m.handleUpdateCheck(updateCheckMsg{rel: &update.Release{Version: "v1.2.0"}, explicit: true})
		nm := out.(Model)
		if cmd != nil || nm.updateBusy || !strings.Contains(nm.banner, "up to date") {
			t.Fatalf("cmd=%v busy=%v banner=%q", cmd != nil, nm.updateBusy, nm.banner)
		}
	})
	t.Run("check failed", func(t *testing.T) {
		m := NewModelForTesting()
		m.updateBusy = true
		out, cmd := m.handleUpdateCheck(updateCheckMsg{err: errors.New("offline"), explicit: true})
		nm := out.(Model)
		if cmd != nil || nm.updateBusy || !strings.Contains(nm.banner, "offline") {
			t.Fatalf("cmd=%v busy=%v banner=%q", cmd != nil, nm.updateBusy, nm.banner)
		}
	})
	t.Run("newer release installs", func(t *testing.T) {
		m := NewModelForTesting()
		m.updateBusy = true
		out, cmd := m.handleUpdateCheck(updateCheckMsg{rel: &update.Release{Version: "v1.3.0"}, explicit: true})
		nm := out.(Model)
		if cmd == nil || !nm.updateBusy || nm.updateAvailable == nil || nm.updateAvailable.Version != "v1.3.0" {
			t.Fatalf("install not started: cmd=%v busy=%v pending=%+v", cmd != nil, nm.updateBusy, nm.updateAvailable)
		}
		if !strings.Contains(nm.banner, "downloading v1.3.0") {
			t.Fatalf("banner = %q", nm.banner)
		}
	})
}
