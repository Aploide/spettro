package agent

import (
	"fmt"

	"spettro/internal/memory"
)

// runSaveMemory implements the save-memory builtin: append one short fact to
// the persistent memory file (user scope by default, project scope on
// request). Saved facts are loaded into the system context of future
// sessions, not the current one — the running session's prompt prefix must
// stay stable for provider prompt caching.
func (r *toolRuntime) runSaveMemory(args []byte) (string, error) {
	var payload struct {
		Fact  string `json:"fact"`
		Scope string `json:"scope"`
	}
	if err := decodeJSONStrict(args, &payload); err != nil {
		return "", fmt.Errorf("save-memory args: %w", err)
	}
	scope := memory.ScopeUser
	switch payload.Scope {
	case "", "user":
	case "project":
		scope = memory.ScopeProject
	default:
		return "", fmt.Errorf("save-memory: invalid scope %q (use \"user\" or \"project\")", payload.Scope)
	}
	res, err := memory.DefaultStore(r.cwd).Save(scope, payload.Fact)
	if err != nil {
		return "", err
	}
	switch res.Outcome {
	case memory.SavedDuplicate:
		return fmt.Sprintf("already in %s memory (%s) — refreshed its last-used date instead of duplicating it", scope, res.Path), nil
	case memory.SavedToInbox:
		return fmt.Sprintf("a similar %s memory already exists (%q); the new fact was routed to the review inbox as a replacement candidate — the user can resolve it with /memory review", scope, res.Near), nil
	default:
		return fmt.Sprintf("saved to %s memory (%s); it will be loaded into context starting from the next session", scope, res.Path), nil
	}
}
