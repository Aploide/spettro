package tui

import (
	"testing"

	"spettro/internal/agent"
)

// TestActiveModalSingle verifies each flag maps to its modal when it is the
// only one set (the common single-modal case must be unambiguous).
func TestActiveModalSingle(t *testing.T) {
	cases := []struct {
		name string
		set  func(*Model)
		want modal
	}{
		{"none", func(*Model) {}, modalNone},
		{"trust", func(m *Model) { m.showTrust = true }, modalTrust},
		{"login", func(m *Model) { m.showLogin = true }, modalLogin},
		{"onboarding", func(m *Model) { m.showOnboarding = true }, modalOnboarding},
		{"resume", func(m *Model) { m.showResume = true }, modalResume},
		{"memory-review", func(m *Model) { m.showMemoryReview = true }, modalMemoryReview},
		{"connect", func(m *Model) { m.showConnect = true }, modalConnect},
		{"question", func(m *Model) {
			m.pendingQuestion = newQuestionForm(askUserRequestMsg{
				form:     agent.AskUserForm{Questions: []agent.AskUserQuestion{{Header: "H", Question: "Q?"}}},
				response: make(chan askUserResponse, 1),
			})
		}, modalQuestion},
		{"selector", func(m *Model) { m.showSelector = true }, modalSelector},
		{"setup", func(m *Model) { m.showSetup = true }, modalSetup},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := NewModelForTesting()
			tc.set(&m)
			if got := m.activeModal(); got != tc.want {
				t.Fatalf("activeModal() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestActiveModalPrecedence verifies trust wins over a co-occurring overlay
// (the realistic fresh-user-on-untrusted-folder case), and that precedence is
// deterministic when several flags are set.
func TestActiveModalPrecedence(t *testing.T) {
	m := NewModelForTesting()
	m.showTrust = true
	m.showOnboarding = true
	m.showLogin = true
	if got := m.activeModal(); got != modalTrust {
		t.Fatalf("trust should take precedence, got %v", got)
	}

	m = NewModelForTesting()
	m.showOnboarding = true
	m.showResume = true
	if got := m.activeModal(); got != modalOnboarding {
		t.Fatalf("onboarding should take precedence over resume, got %v", got)
	}

	// A question blocks a tool call, so it outranks any picker the user left
	// open — but not the startup gates, which decide whether the run happens.
	m = NewModelForTesting()
	m.pendingQuestion = newQuestionForm(askUserRequestMsg{
		form:     agent.AskUserForm{Questions: []agent.AskUserQuestion{{Header: "H", Question: "Q?"}}},
		response: make(chan askUserResponse, 1),
	})
	m.showSelector = true
	if got := m.activeModal(); got != modalQuestion {
		t.Fatalf("a pending question should outrank the selector, got %v", got)
	}
	m.showTrust = true
	if got := m.activeModal(); got != modalTrust {
		t.Fatalf("trust should still gate a pending question, got %v", got)
	}
}

// TestActiveModalEveryRoutedModalHasConsistentView guards against the three
// dispatch sites drifting: every modal that update() routes keys to must also
// be renderable by View (or, for modalSetup, intentionally fall through). We
// assert View does not panic and returns non-empty output for each.
func TestActiveModalViewDoesNotPanic(t *testing.T) {
	for _, mod := range []modal{modalTrust, modalLogin, modalOnboarding, modalQuestion, modalResume, modalMemoryReview, modalConnect, modalSelector, modalSetup, modalNone} {
		m := NewModelForTesting()
		m.ready = true
		m.width, m.height = 80, 24
		m = m.recalcLayout()
		switch mod {
		case modalTrust:
			m.showTrust = true
		case modalLogin:
			m.showLogin = true
		case modalOnboarding:
			m.showOnboarding = true
		case modalQuestion:
			m.pendingQuestion = newQuestionForm(askUserRequestMsg{
				form:     agent.AskUserForm{Questions: []agent.AskUserQuestion{{Header: "H", Question: "Q?"}}},
				response: make(chan askUserResponse, 1),
			})
		case modalResume:
			m.showResume = true
		case modalMemoryReview:
			m.showMemoryReview = true
		case modalConnect:
			m.showConnect = true
		case modalSelector:
			m.showSelector = true
		case modalSetup:
			m.showSetup = true
		}
		if out := m.View().Content; out == "" {
			t.Fatalf("View() returned empty for modal %v", mod)
		}
	}
}
