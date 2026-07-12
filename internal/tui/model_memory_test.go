package tui

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"spettro/internal/memory"
)

func seedInbox(t *testing.T, cands []memory.Candidate) {
	t.Helper()
	if _, err := memory.DefaultInbox().Add(cands, ""); err != nil {
		t.Fatalf("seed inbox: %v", err)
	}
}

func TestMemoryReviewApproveSavesAndRemoves(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	m := NewModelForTesting()
	seedInbox(t, []memory.Candidate{
		{Fact: "prefers tabs", Scope: memory.ScopeUser},
		{Fact: "run make lint", Scope: memory.ScopeUser},
	})

	got, _ := m.handleMemoryCommand("/memory review")
	m = got.(Model)
	if m.activeModal() != modalMemoryReview {
		t.Fatal("review modal not opened")
	}
	if len(m.memoryReviewItems) != 2 {
		t.Fatalf("items = %d, want 2", len(m.memoryReviewItems))
	}

	// Approve the first candidate.
	got, _ = m.updateMemoryReview(tea.KeyPressMsg{Code: 'a', Text: "a"})
	m = got.(Model)
	data, err := os.ReadFile(filepath.Join(home, ".spettro", "memory.md"))
	if err != nil || !strings.Contains(string(data), "- prefers tabs") {
		t.Fatalf("approved fact not saved: %v %q", err, data)
	}
	left, _ := memory.DefaultInbox().Load()
	if len(left) != 1 || left[0].Fact != "run make lint" {
		t.Fatalf("inbox after approve: %+v", left)
	}
	if len(m.memoryReviewItems) != 1 {
		t.Fatalf("dialog items after approve = %d, want 1", len(m.memoryReviewItems))
	}

	// Discard the remaining candidate: nothing more saved, modal closes.
	got, _ = m.updateMemoryReview(tea.KeyPressMsg{Code: 'd', Text: "d"})
	m = got.(Model)
	left, _ = memory.DefaultInbox().Load()
	if len(left) != 0 {
		t.Fatalf("inbox after discard: %+v", left)
	}
	data, _ = os.ReadFile(filepath.Join(home, ".spettro", "memory.md"))
	if strings.Contains(string(data), "run make lint") {
		t.Fatal("discarded fact was saved")
	}
	if m.showMemoryReview {
		t.Fatal("modal should close when the inbox is drained")
	}
}

func TestMemoryReviewEscCloses(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	m := NewModelForTesting()
	seedInbox(t, []memory.Candidate{{Fact: "keep me", Scope: memory.ScopeUser}})
	got, _ := m.handleMemoryCommand("/memory review")
	m = got.(Model)
	got, _ = m.updateMemoryReview(tea.KeyPressMsg{Code: tea.KeyEscape})
	m = got.(Model)
	if m.showMemoryReview {
		t.Fatal("esc did not close the modal")
	}
	left, _ := memory.DefaultInbox().Load()
	if len(left) != 1 {
		t.Fatal("closing must not touch the inbox")
	}
}

// Long facts must never overflow the dialog: unselected rows are truncated
// inside the border and the selected fact is word-wrapped in full.
func TestMemoryReviewViewFitsAndShowsFullSelectedFact(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	m := NewModelForTesting()
	m.width, m.height = 100, 30
	longFact := "User replies in a fast, concise way when asked about repository structure and always wants the assistant to double check test results before claiming success"
	m.showMemoryReview = true
	m.memoryReviewItems = []memory.Candidate{
		{ID: "m1", Fact: longFact, Scope: memory.ScopeUser, Sources: []string{"session-2b18aee5-8c109974", "session-2b18aee5-a75abfcc", "session-2b18aee5-11112222"}},
		{ID: "m2", Fact: strings.Repeat("another very long unselected fact ", 8), Scope: memory.ScopeProject},
	}
	m.memoryReviewCursor = 0

	out := m.viewMemoryReview()
	lines := strings.Split(out, "\n")
	for i, line := range lines {
		if w := lipgloss.Width(line); w > m.width {
			t.Fatalf("line %d overflows: width %d > %d: %q", i, w, m.width, line)
		}
	}
	// The selected fact must be fully present once ANSI codes and wrapping
	// whitespace are stripped.
	plain := stripANSIForTest(out)
	plain = strings.NewReplacer("│", " ", "╭", " ", "╮", " ", "╰", " ", "╯", " ", "─", " ").Replace(plain)
	flat := strings.Join(strings.Fields(plain), " ")
	if !strings.Contains(flat, longFact) {
		t.Fatal("selected fact is not shown in full")
	}
}

var ansiRe = regexp.MustCompile(`\x1b\[[0-9;]*m`)

func stripANSIForTest(s string) string {
	return ansiRe.ReplaceAllString(s, "")
}

func TestMemoryReviewEmptyInboxShowsBanner(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	m := NewModelForTesting()
	got, _ := m.handleMemoryCommand("/memory review")
	m = got.(Model)
	if m.showMemoryReview {
		t.Fatal("modal opened with empty inbox")
	}
}
