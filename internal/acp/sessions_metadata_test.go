package acp

import (
	"testing"
	"time"

	"spettro/internal/provider"
	"spettro/internal/session"
)

// Continuing a TUI session from an editor must not drop the metadata ACP
// does not manage: an unfinished /goal record (which the TUI's /resume
// offers to continue) and the /stats usage counters.
func TestPersistStateKeepsGoalAndStats(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	global := t.TempDir()
	cwd := t.TempDir()
	goal := &session.GoalRecord{Objective: "ship it", Iteration: 3, Active: true}
	stats := &provider.SessionUsage{Totals: provider.UsageTotals{Requests: 7}}
	if err := session.Save(global, session.State{
		Metadata: session.Metadata{ID: "tui-1", ProjectPath: cwd, StartedAt: time.Now(), Goal: goal, Stats: stats},
		Messages: []session.Message{{Role: "user", Content: "hi"}},
	}); err != nil {
		t.Fatal(err)
	}
	b := newBridge(Options{CWD: cwd, GlobalDir: global})
	s, _, err := b.restoreSession("tui-1", cwd)
	if err != nil {
		t.Fatal(err)
	}
	if err := session.Save(global, s.persistState()); err != nil {
		t.Fatal(err)
	}
	got, err := session.Load(global, "tui-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Metadata.Goal == nil || got.Metadata.Goal.Objective != "ship it" || !got.Metadata.Goal.Active {
		t.Errorf("goal record = %+v, want it kept", got.Metadata.Goal)
	}
	if got.Metadata.Stats == nil || got.Metadata.Stats.Totals.Requests != 7 {
		t.Errorf("stats = %+v, want them kept", got.Metadata.Stats)
	}
}
