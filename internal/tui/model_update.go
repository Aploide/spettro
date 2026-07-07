package tui

import (
	"context"
	"fmt"
	"time"

	tea "charm.land/bubbletea/v2"

	"spettro/internal/update"
	"spettro/internal/version"
)

// updateCheckMsg carries the result of the background startup version check.
type updateCheckMsg struct {
	rel *update.Release
	err error
}

// updateAppliedMsg carries the result of downloading and installing an
// update requested via /update.
type updateAppliedMsg struct {
	version string
	path    string
	err     error
}

// checkUpdateCmd asks GitHub for the latest release in the background so
// startup never blocks on the network; a short timeout keeps a slow or
// offline check from lingering.
func checkUpdateCmd() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		rel, err := update.LatestRelease(ctx)
		return updateCheckMsg{rel: rel, err: err}
	}
}

// applyUpdateCmd downloads and installs rel, replacing the running binary.
func applyUpdateCmd(rel *update.Release) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		path, err := update.Apply(ctx, rel)
		return updateAppliedMsg{version: rel.Version, path: path, err: err}
	}
}

func (m Model) handleUpdateCheck(msg updateCheckMsg) (tea.Model, tea.Cmd) {
	// Silent on failure (offline, rate-limited, etc.) — this is a passive
	// background check, not something worth interrupting the user for.
	if msg.err != nil || msg.rel == nil || !update.IsNewer(version.App, msg.rel.Version) {
		return m, nil
	}
	m.updateAvailable = msg.rel
	m.pushSystemMsg(fmt.Sprintf("update available: %s → %s — type /update to install", version.App, msg.rel.Version))
	m.refreshViewport()
	return m, nil
}

func (m Model) handleUpdateApplied(msg updateAppliedMsg) (tea.Model, tea.Cmd) {
	m.updateBusy = false
	if msg.err != nil {
		m.showBanner("update failed: "+msg.err.Error(), "error")
		m.refreshViewport()
		return m, nil
	}
	m.updateAvailable = nil
	m.relaunchBinary = msg.path
	m.pushSystemMsg(fmt.Sprintf("updated to %s — restarting…", msg.version))
	m.refreshViewport()
	return m, tea.Quit
}

// runUpdateCommand kicks off the download/install for a pending update.
// Invoked from /update.
func (m Model) runUpdateCommand() (tea.Model, tea.Cmd) {
	if m.updateBusy {
		m.showBanner("update already in progress…", "info")
		return m, nil
	}
	if m.updateAvailable == nil {
		m.showBanner("spettro "+version.App+" is already up to date", "info")
		return m, nil
	}
	m.updateBusy = true
	m.showBanner("downloading "+m.updateAvailable.Version+"…", "info")
	return m, applyUpdateCmd(m.updateAvailable)
}

// RelaunchPath returns the path to a newly installed executable if /update
// just replaced the running binary, or "" otherwise. main() execs into it
// after the TUI exits cleanly so the restart is seamless.
func RelaunchPath(final tea.Model) string {
	if m, ok := final.(Model); ok {
		return m.relaunchBinary
	}
	return ""
}
