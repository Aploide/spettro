package tui

import "testing"

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
		{"connect", func(m *Model) { m.showConnect = true }, modalConnect},
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
}

// TestActiveModalEveryRoutedModalHasConsistentView guards against the three
// dispatch sites drifting: every modal that update() routes keys to must also
// be renderable by View (or, for modalSetup, intentionally fall through). We
// assert View does not panic and returns non-empty output for each.
func TestActiveModalViewDoesNotPanic(t *testing.T) {
	for _, mod := range []modal{modalTrust, modalLogin, modalOnboarding, modalResume, modalConnect, modalSelector, modalSetup, modalNone} {
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
		case modalResume:
			m.showResume = true
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
