package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"spettro/internal/hooks"
)

const sessionStartMarker = ".hooks_session_started"

func (r *toolRuntime) runSessionStartHooks(ctx context.Context) error {
	if len(r.hooksConfig.Rules) == 0 {
		return nil
	}
	if strings.TrimSpace(r.sessionDir) != "" {
		marker := filepath.Join(r.sessionDir, sessionStartMarker)
		if _, err := os.Stat(marker); err == nil {
			return nil
		}
		if err := os.MkdirAll(r.sessionDir, 0o700); err == nil {
			_ = os.WriteFile(marker, []byte("started"), 0o644)
		}
	}
	for _, rule := range r.hooksConfig.Rules {
		if !rule.Enabled || rule.Event != hooks.EventSessionStart {
			continue
		}
		if _, err := hooks.Run(ctx, rule, hooks.RunInput{Event: hooks.EventSessionStart}); err != nil {
			return err
		}
	}
	return nil
}

func (r *toolRuntime) runPreToolHooks(ctx context.Context, toolID string, args json.RawMessage) (json.RawMessage, string, error) {
	updated := args
	for _, rule := range r.hooksConfig.Rules {
		if !rule.Enabled || rule.Event != hooks.EventPreToolUse || !hooks.MatchAny(rule, hookToolNames(toolID)...) {
			continue
		}
		res, err := hooks.Run(ctx, rule, hooks.RunInput{Event: hooks.EventPreToolUse, ToolID: toolID, ToolArgs: updated})
		if err != nil {
			return nil, "", err
		}
		switch res.Decision {
		case "deny", "block":
			reason := strings.TrimSpace(res.Reason)
			if reason == "" {
				reason = strings.TrimSpace(res.Message)
			}
			if reason == "" {
				reason = fmt.Sprintf("hook %s denied request", rule.ID)
			}
			r.emitApprovalTrace("denied", "hook", toolID, "", reason)
			return nil, reason, nil
		case "allow":
			if len(res.UpdatedArgs) > 0 && toolID == "bash" {
				updated = res.UpdatedArgs
			}
		}
	}
	return updated, "", nil
}

func (r *toolRuntime) runPostToolHooks(ctx context.Context, toolID string, args json.RawMessage, output string) error {
	for _, rule := range r.hooksConfig.Rules {
		if !rule.Enabled || rule.Event != hooks.EventPostToolUse || !hooks.MatchAny(rule, hookToolNames(toolID)...) {
			continue
		}
		_, err := hooks.Run(ctx, rule, hooks.RunInput{Event: hooks.EventPostToolUse, ToolID: toolID, ToolArgs: args, ToolOutput: truncate(output, 2000)})
		if err != nil {
			return err
		}
	}
	return nil
}

// hasPostToolHooks reports whether any enabled PostToolUse hook matches toolID.
func (r *toolRuntime) hasPostToolHooks(toolID string) bool {
	for _, rule := range r.hooksConfig.Rules {
		if rule.Enabled && rule.Event == hooks.EventPostToolUse && hooks.MatchAny(rule, hookToolNames(toolID)...) {
			return true
		}
	}
	return false
}

// hookToolNames is what a hook matcher is tested against for a tool: its
// canonical name plus the retired names that route to it, so a hook written
// for "shell-exec" still fires on bash. The tool_id a hook script receives is
// the canonical name.
func hookToolNames(toolID string) []string {
	return append([]string{toolID}, LegacyToolNames(toolID)...)
}

// finishToolCall runs the PostToolUse hooks for a finished call and returns
// the output to hand the model. After a successful file-edit or file-write, a hook that rewrites the file (gofmt -w, prettier --write) is
// part of the agent's own write: the file is re-stamped so the stale-read
// guard doesn't refuse the next edit, and the model is told the file changed.
func (r *toolRuntime) finishToolCall(ctx context.Context, call toolCall, out string, err error) string {
	abs, rel, ok := "", "", false
	if err == nil && r.hasPostToolHooks(call.Tool) {
		abs, rel, ok = r.writtenFile(call)
	}
	if !ok {
		_ = r.runPostToolHooks(ctx, call.Tool, call.Args, out)
		return out
	}
	// Held across the hooks, so a concurrent edit of the file waits for the
	// formatter instead of racing it.
	defer r.lockFile(abs)()
	before, _ := os.ReadFile(abs)
	_ = r.runPostToolHooks(ctx, call.Tool, call.Args, out)
	after, readErr := os.ReadFile(abs)
	// Re-stamp only a change the hooks made on top of the agent's own
	// content; if the file already differed before they ran, something else
	// changed it and the guard must still fire.
	if readErr != nil || bytes.Equal(before, after) || !r.stampMatches(rel, before) {
		return out
	}
	r.recordFileStamp(rel, after)
	return out + fmt.Sprintf("\nnote: a PostToolUse hook rewrote %s after this change (a formatter?), so it no longer matches what you wrote above; file-read it before quoting it in old_string", rel)
}

// writtenFile returns the file a successful write tool call changed.
func (r *toolRuntime) writtenFile(call toolCall) (abs, rel string, ok bool) {
	switch call.Tool {
	case "file-edit", "file-write":
	default:
		return "", "", false
	}
	var args struct {
		Path     string `json:"path"`
		FilePath string `json:"file_path"` // alias the write tools accept
	}
	if json.Unmarshal(call.Args, &args) != nil {
		return "", "", false
	}
	p := firstNonEmpty(args.Path, args.FilePath)
	if strings.TrimSpace(p) == "" {
		return "", "", false
	}
	abs, rel, err := r.resolvePath(p)
	return abs, rel, err == nil
}

func (r *toolRuntime) runPermissionRequestHooks(ctx context.Context, toolID, command string) (string, string, error) {
	for _, rule := range r.hooksConfig.Rules {
		if !rule.Enabled || rule.Event != hooks.EventPermissionRequest || !hooks.MatchAny(rule, hookToolNames(toolID)...) {
			continue
		}
		res, err := hooks.Run(ctx, rule, hooks.RunInput{Event: hooks.EventPermissionRequest, ToolID: toolID, Command: command})
		if err != nil {
			return "", "", err
		}
		switch res.Decision {
		case "deny", "block":
			reason := strings.TrimSpace(res.Reason)
			if reason == "" {
				reason = strings.TrimSpace(res.Message)
			}
			if reason == "" {
				reason = fmt.Sprintf("hook %s denied request", rule.ID)
			}
			return "deny", reason, nil
		case "allow":
			return "allow", strings.TrimSpace(res.Reason), nil
		}
	}
	return "", "", nil
}
