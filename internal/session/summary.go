package session

import (
	"os"
	"sort"
)

func List(globalDir, cwd string) ([]Summary, error) {
	var out []Summary
	projectHash := ProjectHash(cwd)
	entries, err := os.ReadDir(SessionsDir(globalDir))
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		// Read metadata only — the heavy message/event files are not needed to
		// list sessions.
		meta, err := LoadMetadata(globalDir, entry.Name())
		if err != nil {
			continue
		}
		if meta.ProjectHash != projectHash && meta.ProjectPath != cwd {
			continue
		}
		preview := meta.Preview
		if preview == "" {
			// Sessions written before previews were persisted: fall back to
			// reading their messages so they still show a snippet.
			if state, err := Load(globalDir, entry.Name()); err == nil {
				preview = firstUserPreview(state.Messages)
			}
		}
		out = append(out, Summary{
			ID:        meta.ID,
			StartedAt: meta.StartedAt,
			UpdatedAt: meta.UpdatedAt,
			Path:      SessionDir(globalDir, meta.ID),
			Preview:   preview,
		})
	}

	sort.Slice(out, func(i, j int) bool {
		return out[i].UpdatedAt.After(out[j].UpdatedAt)
	})
	return out, nil
}

func firstUserPreview(messages []Message) string {
	for _, msg := range messages {
		if msg.Role != "user" {
			continue
		}
		preview := msg.Content
		if len(preview) > 60 {
			return preview[:60] + "…"
		}
		return preview
	}
	return ""
}
