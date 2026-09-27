package tui

import (
	"fmt"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// Git runs only in background commands: no Update path starts a git
// process on the UI goroutine (commands returned by Update are not run
// here).
func TestNoGitOnTheUpdateGoroutine(t *testing.T) {
	calls := 0
	orig := runGit
	runGit = func(dir string, args ...string) ([]byte, error) {
		calls++
		return nil, fmt.Errorf("git disabled in this test")
	}
	defer func() { runGit = orig }()

	m := footerModel(140, 40)
	m.messages = append(m.messages, ChatMessage{Role: RoleUser, Content: "hi"})
	// Paths that refresh the side panel's git state. (ctrl+b does too, but
	// it also saves the user's config file, which a test must not touch.)
	for _, msg := range []tea.Msg{agentDoneMsg{content: "done"}, commitDoneMsg{commitMsg: "x"}} {
		m.thinking = true
		nm, _ := m.Update(msg)
		m = nm.(Model)
	}
	nm, _ := m.handleDiffCommand("/diff")
	m = nm.(Model)
	if calls != 0 {
		t.Fatalf("Update started %d git processes on the UI goroutine", calls)
	}
	if !m.gitRefreshPending && m.lastModifiedRefreshAt.IsZero() {
		t.Fatal("no background git refresh was requested")
	}
}
