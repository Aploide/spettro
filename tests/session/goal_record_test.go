package session_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"spettro/internal/session"
)

// TestGoalRecordRoundTrip verifies that a session with an active goal record
// can be serialized to disk and deserialized with all fields intact.
func TestGoalRecordRoundTrip(t *testing.T) {
	t.Parallel()
	globalDir := t.TempDir()
	sessionID := "test-goal-session"

	startedAt := time.Date(2026, 2, 11, 10, 0, 0, 0, time.UTC)
	goalStartedAt := time.Date(2026, 2, 11, 10, 5, 0, 0, time.UTC)

	original := session.State{
		Metadata: session.Metadata{
			ID:        sessionID,
			StartedAt: startedAt,
			Goal: &session.GoalRecord{
				Objective:       "implement persistence and resume",
				Iteration:       3,
				NoProgress:      1,
				StartedAt:       goalStartedAt,
				MaxIterations:   50,
				NoProgressLimit: 5,
				Active:          true,
			},
		},
		Messages: []session.Message{
			{
				Role:    "user",
				Content: "test message",
				At:      startedAt,
			},
		},
	}

	if err := session.Save(globalDir, original); err != nil {
		t.Fatalf("failed to save session: %v", err)
	}

	loaded, err := session.Load(globalDir, sessionID)
	if err != nil {
		t.Fatalf("failed to load session: %v", err)
	}

	if loaded.Metadata.Goal == nil {
		t.Fatal("loaded session has nil Goal record, expected non-nil")
	}

	og := original.Metadata.Goal
	lg := loaded.Metadata.Goal

	if lg.Objective != og.Objective {
		t.Errorf("goal Objective: got %q, want %q", lg.Objective, og.Objective)
	}
	if lg.Iteration != og.Iteration {
		t.Errorf("goal Iteration: got %d, want %d", lg.Iteration, og.Iteration)
	}
	if lg.NoProgress != og.NoProgress {
		t.Errorf("goal NoProgress: got %d, want %d", lg.NoProgress, og.NoProgress)
	}
	if lg.MaxIterations != og.MaxIterations {
		t.Errorf("goal MaxIterations: got %d, want %d", lg.MaxIterations, og.MaxIterations)
	}
	if lg.NoProgressLimit != og.NoProgressLimit {
		t.Errorf("goal NoProgressLimit: got %d, want %d", lg.NoProgressLimit, og.NoProgressLimit)
	}
	if lg.Active != og.Active {
		t.Errorf("goal Active: got %v, want %v", lg.Active, og.Active)
	}
	if !lg.StartedAt.Equal(og.StartedAt) {
		t.Errorf("goal StartedAt: got %v, want %v", lg.StartedAt, og.StartedAt)
	}
}

// TestGoalRecordInactiveNotRestored verifies that an inactive goal (Active=false)
// is not offered for resume on session load (pendingGoalResume check in TUI
// uses Active as the gate).
func TestGoalRecordInactiveNotRestored(t *testing.T) {
	t.Parallel()
	globalDir := t.TempDir()
	sessionID := "test-inactive-goal"

	state := session.State{
		Metadata: session.Metadata{
			ID:        sessionID,
			StartedAt: time.Now(),
			Goal: &session.GoalRecord{
				Objective:       "completed goal",
				Iteration:       10,
				StartedAt:       time.Now(),
				MaxIterations:   50,
				NoProgressLimit: 5,
				Active:          false,
			},
		},
		Messages: []session.Message{
			{Role: "user", Content: "test", At: time.Now()},
		},
	}

	if err := session.Save(globalDir, state); err != nil {
		t.Fatalf("failed to save: %v", err)
	}

	loaded, err := session.Load(globalDir, sessionID)
	if err != nil {
		t.Fatalf("failed to load: %v", err)
	}

	if loaded.Metadata.Goal == nil {
		t.Fatal("loaded session has nil Goal record")
	}
	if loaded.Metadata.Goal.Active {
		t.Error("inactive goal was marked as active after round-trip")
	}
}

// TestGoalRecordNilWhenNoGoal verifies that sessions without a goal
// do not gain a spurious goal record on load.
func TestGoalRecordNilWhenNoGoal(t *testing.T) {
	t.Parallel()
	globalDir := t.TempDir()
	sessionID := "test-no-goal"

	state := session.State{
		Metadata: session.Metadata{
			ID:        sessionID,
			StartedAt: time.Now(),
		},
		Messages: []session.Message{
			{Role: "user", Content: "test", At: time.Now()},
		},
	}

	if err := session.Save(globalDir, state); err != nil {
		t.Fatalf("failed to save: %v", err)
	}

	loaded, err := session.Load(globalDir, sessionID)
	if err != nil {
		t.Fatalf("failed to load: %v", err)
	}

	if loaded.Metadata.Goal != nil {
		t.Error("session without goal gained a non-nil Goal record after round-trip")
	}
}

// TestGoalRecordJSONFormat verifies that the goal record is written
// in the expected JSON format by checking the raw metadata file.
func TestGoalRecordJSONFormat(t *testing.T) {
	t.Parallel()
	globalDir := t.TempDir()
	sessionID := "test-format"

	state := session.State{
		Metadata: session.Metadata{
			ID:        sessionID,
			StartedAt: time.Date(2026, 2, 11, 10, 0, 0, 0, time.UTC),
			Goal: &session.GoalRecord{
				Objective:       "test goal",
				Iteration:       5,
				Active:          true,
				StartedAt:       time.Date(2026, 2, 11, 10, 5, 0, 0, time.UTC),
				MaxIterations:   100,
				NoProgressLimit: 10,
			},
		},
		Messages: []session.Message{
			{Role: "user", Content: "test", At: time.Date(2026, 2, 11, 10, 0, 0, 0, time.UTC)},
		},
	}

	if err := session.Save(globalDir, state); err != nil {
		t.Fatalf("failed to save: %v", err)
	}

	metaPath := filepath.Join(globalDir, "sessions", sessionID, "session.json")
	data, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatalf("failed to read metadata file: %v", err)
	}

	content := string(data)
	for _, want := range []string{
		`"goal"`,
		`"objective": "test goal"`,
		`"iteration": 5`,
		`"active": true`,
		`"max_iterations": 100`,
		`"no_progress_limit": 10`,
	} {
		if !strings.Contains(content, want) {
			t.Errorf("metadata file missing %q\ngot:\n%s", want, content)
		}
	}
}
