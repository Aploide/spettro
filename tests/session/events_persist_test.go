package session_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"spettro/internal/session"
)

// List should render the resume picker from metadata alone; removing the heavy
// messages file must not drop the session or its preview.
func TestListReadsPreviewFromMetadataOnly(t *testing.T) {
	t.Parallel()
	globalDir := t.TempDir()
	cwd := t.TempDir()

	if err := session.Save(globalDir, session.State{
		Metadata: session.Metadata{ID: "s-preview", ProjectPath: cwd, ProjectHash: session.ProjectHash(cwd)},
		Messages: []session.Message{{Role: "user", Content: "remember this preview", At: time.Now()}},
	}); err != nil {
		t.Fatalf("save: %v", err)
	}
	// Delete messages.json to prove List does not depend on it.
	if err := os.Remove(filepath.Join(session.SessionDir(globalDir, "s-preview"), "messages.json")); err != nil {
		t.Fatalf("remove messages: %v", err)
	}

	items, err := session.List(globalDir, cwd)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 session, got %d", len(items))
	}
	if items[0].Preview != "remember this preview" {
		t.Fatalf("expected preview from metadata, got %q", items[0].Preview)
	}
}

// A routine Save carries no events (the TUI autoSave path never populates
// State.Events). It must not truncate the append-only agents event log that
// AppendEvent writes during a run, otherwise /resume rebuilds an empty
// activity feed.
func TestSaveDoesNotClobberAppendedEvents(t *testing.T) {
	t.Parallel()
	globalDir := t.TempDir()
	sessionID := "s-events"

	for i := 0; i < 3; i++ {
		if err := session.AppendEvent(globalDir, sessionID, session.AgentEvent{
			At:      time.Now(),
			Kind:    "tool",
			AgentID: "coding",
			Status:  "ok",
		}); err != nil {
			t.Fatalf("append event %d: %v", i, err)
		}
	}

	// Eventless Save, exactly like the autoSave path.
	if err := session.Save(globalDir, session.State{
		Metadata: session.Metadata{ID: sessionID, ProjectPath: t.TempDir()},
		Messages: []session.Message{{Role: "user", Content: "hi", At: time.Now()}},
	}); err != nil {
		t.Fatalf("save: %v", err)
	}

	loaded, err := session.Load(globalDir, sessionID)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(loaded.Events) != 3 {
		t.Fatalf("expected 3 events to survive eventless Save, got %d", len(loaded.Events))
	}
}

// When the caller does supply events, Save still persists them.
func TestSavePersistsSuppliedEvents(t *testing.T) {
	t.Parallel()
	globalDir := t.TempDir()
	sessionID := "s-events-supplied"

	if err := session.Save(globalDir, session.State{
		Metadata: session.Metadata{ID: sessionID, ProjectPath: t.TempDir()},
		Messages: []session.Message{{Role: "user", Content: "hi", At: time.Now()}},
		Events: []session.AgentEvent{
			{At: time.Now(), Kind: "agent", AgentID: "plan", Status: "done"},
		},
	}); err != nil {
		t.Fatalf("save: %v", err)
	}

	loaded, err := session.Load(globalDir, sessionID)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(loaded.Events) != 1 {
		t.Fatalf("expected 1 supplied event to persist, got %d", len(loaded.Events))
	}
}
