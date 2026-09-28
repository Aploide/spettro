package tui

import (
	"path/filepath"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"spettro/internal/session"
	"spettro/internal/storage"
)

// Resuming a short conversation after a long one must not keep the long
// one's context gauge: the first prompt would be refused with "context
// limit reached; run /compact".
func TestResumeResetsTheContextGauge(t *testing.T) {
	m := NewModelForTesting()
	global := t.TempDir()
	m.store = &storage.Store{GlobalDir: global, ProjectDir: filepath.Join(global, "p")}
	window := m.contextWindow()
	if window == 0 {
		window = contextWindowDefault(m.cfg.ActiveProvider)
	}
	m.contextTokens = window * 99 / 100
	m.compactWarningLevel = 2
	if !m.evaluateCompact().IsBlocking {
		t.Fatal("precondition: the gauge should block at 99%")
	}
	state := session.State{
		Metadata: session.Metadata{ID: "s-small", ProjectPath: m.cwd, StartedAt: time.Now()},
		Messages: []session.Message{{Role: "user", Content: "hi"}, {Role: "assistant", Content: "hello"}},
	}
	if err := session.Save(global, state); err != nil {
		t.Fatal(err)
	}
	m.showResume = true
	m.resumeItems = []session.Summary{{ID: "s-small"}}
	nm, _ := m.updateResume(tea.KeyPressMsg{Code: tea.KeyEnter})
	got := nm.(Model)
	if got.contextTokens != 0 || got.compactWarningLevel != 0 || got.evaluateCompact().IsBlocking {
		t.Fatalf("after /resume: contextTokens=%d warning=%d blocking=%v", got.contextTokens, got.compactWarningLevel, got.evaluateCompact().IsBlocking)
	}
}

// "Don't execute" keeps the plan for /approve, as its banner says, and
// neither it nor Esc turns the next ordinary prompt into an edit of the
// plan; only the "Edit" choice does. /clear drops the plan.
func TestPlanApprovalChoicesLeaveNoStaleState(t *testing.T) {
	const plan = "1. do X"
	for _, c := range []struct {
		name string
		key  tea.KeyPressMsg
	}{
		{"don't execute", tea.KeyPressMsg{Code: tea.KeyEnter}},
		{"esc", tea.KeyPressMsg{Code: tea.KeyEscape}},
	} {
		t.Run(c.name, func(t *testing.T) {
			m := NewModelForTesting()
			m.pendingPlan = plan
			m.showPlanApproval = true
			m.planApprovalCursor = 1 // Don't execute (ignored by esc)
			nm, _ := m.updatePlanApproval(c.key)
			m = nm.(Model)
			if m.pendingPlan != plan {
				t.Fatalf("the plan was dropped; /approve would have nothing to run")
			}
			// An ordinary prompt while a run is active: it must reach the
			// steer-or-queue picker, not the plan agent as an edit.
			m.thinking = true
			m.ta.SetValue("something else entirely")
			nm, _ = m.updateMain(tea.KeyPressMsg{Code: tea.KeyEnter})
			m = nm.(Model)
			if !m.showSteerChoice || m.pendingPlan != plan {
				t.Fatalf("the prompt was taken as a plan edit (steer=%v plan=%q)", m.showSteerChoice, m.pendingPlan)
			}
		})
	}
	t.Run("edit", func(t *testing.T) {
		m := NewModelForTesting()
		m.pendingPlan = plan
		m.showPlanApproval = true
		m.planApprovalCursor = 2
		nm, _ := m.updatePlanApproval(tea.KeyPressMsg{Code: tea.KeyEnter})
		m = nm.(Model)
		if !m.planEditing {
			t.Fatal("choosing Edit must route the next input to the plan agent")
		}
	})
	t.Run("clear", func(t *testing.T) {
		m := NewModelForTesting()
		m.pendingPlan = plan
		m.planEditing = true
		nm, _ := m.handleCommand("/clear")
		m = nm.(Model)
		if m.pendingPlan != "" || m.planEditing {
			t.Fatalf("/clear kept the plan: %q editing=%v", m.pendingPlan, m.planEditing)
		}
	})
}
