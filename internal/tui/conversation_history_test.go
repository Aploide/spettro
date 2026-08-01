package tui

import (
	"fmt"
	"strings"
	"testing"
)

// TestBuildConversationHistory_EmptyOnFirstTurn verifies that a fresh model (or
// one whose only message is the current user turn) yields no history.
func TestBuildConversationHistory_EmptyOnFirstTurn(t *testing.T) {
	m := NewModelForTesting()
	if got := m.buildConversationHistory(); got != "" {
		t.Fatalf("no messages should yield empty history, got %q", got)
	}

	// Only the current user turn present → still empty (it is excluded).
	m.messages = []ChatMessage{{Role: RoleUser, Content: "first prompt"}}
	if got := m.buildConversationHistory(); got != "" {
		t.Fatalf("lone trailing user turn should be excluded, got %q", got)
	}
}

// TestBuildConversationHistory_ExcludesTrailingUserTurn verifies prior turns are
// included while the current (trailing) user message is omitted.
func TestBuildConversationHistory_ExcludesTrailingUserTurn(t *testing.T) {
	m := NewModelForTesting()
	m.messages = []ChatMessage{
		{Role: RoleUser, Content: "add a feature"},
		{Role: RoleAssistant, Content: "done, added it"},
		{Role: RoleUser, Content: "now write tests"}, // current turn
	}
	got := m.buildConversationHistory()
	if !strings.Contains(got, "user: add a feature") {
		t.Fatalf("prior user turn missing: %q", got)
	}
	if !strings.Contains(got, "assistant: done, added it") {
		t.Fatalf("prior assistant turn missing: %q", got)
	}
	if strings.Contains(got, "now write tests") {
		t.Fatalf("current (trailing) user turn must be excluded: %q", got)
	}
}

// TestBuildConversationHistory_IncludesCompactSummary verifies a /compact
// summary is carried forward, while routine system notices are not.
func TestBuildConversationHistory_IncludesCompactSummary(t *testing.T) {
	m := NewModelForTesting()
	m.messages = []ChatMessage{
		{Role: RoleSystem, Content: compactSummaryPrefix + "\n\nWe decided to use Postgres."},
		{Role: RoleSystem, Content: "spettro ready — /help"},
		{Role: RoleAssistant, Content: "ok"},
		{Role: RoleUser, Content: "current"}, // trailing, excluded
	}
	got := m.buildConversationHistory()
	if !strings.Contains(got, "We decided to use Postgres") {
		t.Fatalf("compact summary must be carried forward: %q", got)
	}
	if strings.Contains(got, "spettro ready") {
		t.Fatalf("routine system notices must not be included: %q", got)
	}
}

// TestBuildConversationHistory_BoundsOldestFirst verifies the byte cap keeps the
// most recent turns and drops the oldest, preserving oldest-first ordering.
func TestBuildConversationHistory_BoundsOldestFirst(t *testing.T) {
	m := NewModelForTesting()
	// Build many large turns so the cap is exceeded. Each ~5KB.
	big := strings.Repeat("x", 5000)
	for i := range 20 {
		role := RoleUser
		if i%2 == 1 {
			role = RoleAssistant
		}
		// Tag each turn so we can identify which survived.
		m.messages = append(m.messages, ChatMessage{Role: role, Content: tag(i) + big})
	}
	// Append a trailing assistant turn so nothing is excluded as "current".
	m.messages = append(m.messages, ChatMessage{Role: RoleAssistant, Content: tag(999) + "recent"})

	got := m.buildConversationHistory()
	if len(got) > maxConversationHistoryBytes+singleLineTurnSlack {
		t.Fatalf("history exceeded cap: %d bytes", len(got))
	}
	// The most recent turn must survive; the oldest must be dropped.
	if !strings.Contains(got, tag(999)) {
		t.Fatalf("most recent turn should survive the cap: %q", got[:80])
	}
	if strings.Contains(got, tag(0)) {
		t.Fatalf("oldest turn should be dropped by the cap")
	}
	// Ordering must remain oldest-first: an earlier surviving turn must appear
	// before a later one.
	iEarly := strings.Index(got, tag(16))
	iLate := strings.Index(got, tag(18))
	if iEarly >= 0 && iLate >= 0 && iEarly >= iLate {
		t.Fatalf("history must be oldest-first (tag16=%d tag18=%d)", iEarly, iLate)
	}
}

// singleLineTurnSlack allows for per-turn truncation markers + newlines when
// asserting the overall cap.
const singleLineTurnSlack = 4096

func tag(i int) string {
	return fmt.Sprintf("[[T%d]]", i)
}
