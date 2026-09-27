package tui

import (
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"spettro/internal/config"
	"spettro/internal/provider"
	"spettro/internal/storage"
)

// countGit replaces runGit for the test with a stub that counts calls and
// answers with reply (nil: git fails).
func countGit(t *testing.T, reply func(args []string) ([]byte, error)) *int {
	t.Helper()
	calls := 0
	orig := runGit
	runGit = func(dir string, args ...string) ([]byte, error) {
		calls++
		if reply == nil {
			return nil, fmt.Errorf("git disabled in this test")
		}
		return reply(args)
	}
	t.Cleanup(func() { runGit = orig })
	return &calls
}

// Git runs only in background commands: no Update path, and not New, starts
// a git process on the UI goroutine (commands returned by Update are not run
// here). The test runs in a temporary HOME (testhome), so ctrl+b and
// shift+tab may save the config.
func TestNoGitOnTheUpdateGoroutine(t *testing.T) {
	calls := countGit(t, nil)

	cwd := t.TempDir()
	store := &storage.Store{ProjectDir: cwd + "/.spettro", GlobalDir: t.TempDir()}
	_ = New(cwd, config.Default(), store, provider.NewManager(), nil)
	if *calls != 0 {
		t.Fatalf("New started %d git processes", *calls)
	}

	m := footerModel(140, 40)
	m.messages = append(m.messages, ChatMessage{Role: RoleUser, Content: "hi"})
	// Paths that refresh the side panel's git state.
	for _, msg := range []tea.Msg{agentDoneMsg{content: "done"}, commitDoneMsg{commitMsg: "x"}} {
		m.thinking = true
		nm, _ := m.Update(msg)
		m = nm.(Model)
	}
	m.gitRefreshPending = false
	for _, key := range []tea.KeyPressMsg{
		{Code: 'b', Mod: tea.ModCtrl},
		{Code: tea.KeyTab, Mod: tea.ModShift},
	} {
		nm, _ := m.Update(key)
		m = nm.(Model)
	}
	nm, _ := m.handleDiffCommand("/diff")
	m = nm.(Model)
	// A goal iteration ends: the no-progress guard compares fingerprints the
	// run's command took, and dispatching the next iteration runs nothing.
	m.activeGoal = &goalState{Objective: "x", NoProgressLimit: 3}
	m.thinking = true
	nm, _ = m.Update(agentDoneMsg{content: "iteration", goalSigBefore: "git:1", goalSigAfter: "git:1"})
	m = nm.(Model)
	if *calls != 0 {
		t.Fatalf("Update started %d git processes on the UI goroutine", *calls)
	}
	if m.activeGoal == nil || m.activeGoal.NoProgress != 1 {
		t.Fatalf("an unchanged workspace did not count as no progress: %+v", m.activeGoal)
	}
}

// ctrl+b asks for a background git refresh, and the answer lands in the
// side panel.
func TestSidePanelToggleRefreshesGitInTheBackground(t *testing.T) {
	countGit(t, nil)
	m := footerModel(140, 40)
	nm, cmd := m.Update(tea.KeyPressMsg{Code: 'b', Mod: tea.ModCtrl})
	m = nm.(Model)
	if !m.showSidePanel || cmd == nil || m.lastModifiedRefreshAt.IsZero() {
		t.Fatalf("ctrl+b requested no git refresh (panel=%v cmd=%v)", m.showSidePanel, cmd != nil)
	}
	nm, _ = m.Update(modifiedFilesMsg{branch: "main", files: []modifiedFileEntry{{Path: "a.go", Unstaged: true}}})
	if got := nm.(Model).gitBranch; got != "main" {
		t.Fatalf("the refresh did not reach the side panel: branch %q", got)
	}
}

// /diff without paths lists the modified files, and hands what it read to
// the side panel as well, as it did when it ran on the UI goroutine.
func TestDiffCommandRefreshesSidePanelGitState(t *testing.T) {
	countGit(t, func(args []string) ([]byte, error) {
		switch strings.Join(args, " ") {
		case "rev-parse --is-inside-work-tree":
			return []byte("true\n"), nil
		case "branch --show-current":
			return []byte("feature\n"), nil
		case "status --porcelain":
			return []byte(" M a.go\n"), nil
		case "diff HEAD -- a.go":
			return []byte("diff --git a/a.go b/a.go\n+x\n"), nil
		}
		return nil, nil
	})
	m := footerModel(140, 40)
	_, cmd := m.handleDiffCommand("/diff")
	if cmd == nil {
		t.Fatal("/diff returned no command")
	}
	nm, _ := m.Update(cmd())
	m = nm.(Model)
	if m.gitBranch != "feature" || len(m.modifiedFiles) != 1 || m.modifiedFiles[0].Path != "a.go" {
		t.Fatalf("/diff did not update the side panel's git state: branch %q files %+v", m.gitBranch, m.modifiedFiles)
	}
	if last := m.messages[len(m.messages)-1]; last.Kind != "diff" {
		t.Fatalf("/diff showed no diff: %+v", last)
	}
}

// A goal iteration that changed the workspace resets the no-progress count;
// one without fingerprints (not a goal run's message) counts as progress.
func TestGoalProgressUsesTheRunsFingerprints(t *testing.T) {
	countGit(t, nil)
	for _, tc := range []struct {
		name          string
		before, after string
		want          int
	}{
		{"changed", "git:1", "git:2", 0},
		{"unchanged", "git:1", "git:1", 2},
		{"no fingerprints", "", "", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := footerModel(140, 40)
			m.activeGoal = &goalState{Objective: "x", NoProgress: 1, NoProgressLimit: 5}
			m.thinking = true
			nm, _ := m.Update(agentDoneMsg{content: "i", goalSigBefore: tc.before, goalSigAfter: tc.after})
			g := nm.(Model).activeGoal
			if g == nil || g.NoProgress != tc.want {
				t.Fatalf("no-progress count %+v, want %d", g, tc.want)
			}
		})
	}
}
