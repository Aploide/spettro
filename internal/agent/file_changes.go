package agent

import (
	"context"
	"sync"
)

// File changes reported to hosts.
//
// The model learns what an edit did from the tool's text result (a short
// summary and a compact diff). Hosts that can render a real diff (ACP
// editors show one inline in the tool-call card) need more: the file's whole
// text before and after the call. The file tools already hold both when they
// write, so they record them on a per-call sink carried by the call context,
// the same way images reach hosts (see imageSink in view_image.go); the tool
// loop copies them onto the completion ToolTrace.FileChanges. Nothing here
// reaches the model or the conversation history.

// maxFileChangeText caps each side (old and new) of a recorded change. A
// change to a larger file is still reported, with its texts dropped and
// TextOmitted set: a diff of a multi-megabyte file is not something a host
// can usefully render, and holding both copies in every trace would cost
// memory for no benefit.
const maxFileChangeText = 256 << 10

// FileChange is one file a tool call changed.
type FileChange struct {
	// Path is the absolute path of the file.
	Path string
	// OldText is the file's content before the call; empty when Created.
	OldText string
	// NewText is the file's content after the call.
	NewText string
	// Created is set when the file did not exist before the call.
	Created bool
	// TextOmitted is set when either side exceeded maxFileChangeText; OldText
	// and NewText are then empty and only Path and Created are meaningful.
	TextOmitted bool
}

// fileChangeSink collects the changes one tool call made. Tools in a batch
// run in parallel goroutines, so each call gets its own sink through its
// context rather than sharing a runtime field.
type fileChangeSink struct {
	mu      sync.Mutex
	changes []FileChange
}

type fileChangeSinkKey struct{}

// withFileChangeSink derives a context carrying a fresh sink for one call.
func withFileChangeSink(ctx context.Context) (context.Context, *fileChangeSink) {
	s := &fileChangeSink{}
	return context.WithValue(ctx, fileChangeSinkKey{}, s), s
}

// recordFileChange reports that the current call changed path (absolute)
// from oldText to newText. It is a no-op when the context carries no sink
// (direct runtime calls in tests, hosts that never asked).
func recordFileChange(ctx context.Context, path, oldText, newText string, created bool) {
	s, ok := ctx.Value(fileChangeSinkKey{}).(*fileChangeSink)
	if !ok || s == nil {
		return
	}
	s.mu.Lock()
	s.changes = append(s.changes, newFileChange(path, oldText, newText, created))
	s.mu.Unlock()
}

// newFileChange builds a FileChange, dropping texts over the size cap.
func newFileChange(path, oldText, newText string, created bool) FileChange {
	if len(oldText) > maxFileChangeText || len(newText) > maxFileChangeText {
		return FileChange{Path: path, Created: created, TextOmitted: true}
	}
	return FileChange{Path: path, OldText: oldText, NewText: newText, Created: created}
}

// list returns a copy of the recorded changes (nil for a nil sink).
func (s *fileChangeSink) list() []FileChange {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.changes) == 0 {
		return nil
	}
	return append([]FileChange(nil), s.changes...)
}
