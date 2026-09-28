package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
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
	for _, rule := range r.toolHookRules(ctx, hooks.EventPreToolUse, toolID) {
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
			// Only a shell command may be rewritten by a hook: a call the
			// built-in shell carries out, under bash or an unfolded
			// shell-exec, and never a tool of the operator's own called bash.
			if len(res.UpdatedArgs) > 0 && r.builtinFor(toolID) == "bash" {
				updated = res.UpdatedArgs
			}
		}
	}
	return updated, "", nil
}

func (r *toolRuntime) runPostToolHooks(ctx context.Context, toolID string, args json.RawMessage, output string) error {
	for _, rule := range r.toolHookRules(ctx, hooks.EventPostToolUse, toolID) {
		_, err := hooks.Run(ctx, rule, hooks.RunInput{Event: hooks.EventPostToolUse, ToolID: toolID, ToolArgs: args, ToolOutput: truncate(output, 2000)})
		if err != nil {
			return err
		}
	}
	return nil
}

// hasPostToolHooks reports whether any enabled PostToolUse hook matches toolID.
func (r *toolRuntime) hasPostToolHooks(ctx context.Context, toolID string) bool {
	return len(r.toolHookRules(ctx, hooks.EventPostToolUse, toolID)) > 0
}

// toolHookRules returns, in order, the enabled hooks of event that apply to
// a call of toolID. A matcher is tested against:
//
//   - the canonical name, always, so an alias is never a way around a hook
//     on the tool it runs;
//   - the retired name the model called the tool by, if any (withCalledAs),
//     so a hook written for "task-delete" fires on task-delete calls, as it
//     always did, but not on every todo-write; an lsp call counts as called
//     by the retired tool its op replaced (hookAlias), so a hook written for
//     "diagnostics" fires on lsp {op: "diagnostics"};
//   - the retired names that were the very same tool (sameTool: shell-exec
//     is bash), so a hook written for "shell-exec" keeps guarding the shell
//     now that the model only sees bash.
//
// Names are matched only while they belong to the tool the call runs (see
// tool_names.go): a call of the operator's own tool called bash is not the
// shell, so hooks written for shell-exec do not fire on it; and a retired
// name the operator's own tool answers to (a "shell-exec" script) is that
// tool's, so its hooks do not fire on the built-in bash.
//
// A hook that applies only through a retired name is skipped when a hook
// with the same command already applies, so a hook copied under both
// "shell-exec" and "bash" runs once per call, not twice. The tool_id a hook
// script receives is the call's own name: the canonical one for an alias.
func (r *toolRuntime) toolHookRules(ctx context.Context, event hooks.Event, toolID string) []hooks.EffectiveRule {
	var retired []string
	if alias := calledAs(ctx); alias != "" && alias != toolID {
		retired = append(retired, alias)
	}
	if r.builtinFor(toolID) == toolID {
		for _, name := range LegacyToolNames(toolID) {
			if legacyTools[name].sameTool && !r.userToolNamed(name) && !slices.Contains(retired, name) {
				retired = append(retired, name)
			}
		}
	}
	var out []hooks.EffectiveRule
	var viaRetired []bool
	for _, rule := range r.hooksConfig.Rules {
		if !rule.Enabled || rule.Event != event {
			continue
		}
		switch {
		case hooks.Match(rule, toolID):
			out, viaRetired = append(out, rule), append(viaRetired, false)
		case hooks.MatchAny(rule, retired...):
			out, viaRetired = append(out, rule), append(viaRetired, true)
		}
	}
	seen := map[string]bool{}
	for i, rule := range out {
		if !viaRetired[i] {
			seen[strings.TrimSpace(rule.Command)] = true
		}
	}
	kept := out[:0]
	for i, rule := range out {
		if cmd := strings.TrimSpace(rule.Command); viaRetired[i] {
			if seen[cmd] {
				continue
			}
			seen[cmd] = true
		}
		kept = append(kept, rule)
	}
	return kept
}

type calledAsKey struct{}

// withCalledAs records, for the hooks of one tool call, the retired name the
// call answers to (toolRuntime.hookAlias: the name the model called the tool
// by, "" when it used the canonical name, or an lsp op's former tool). It is
// set for every call, so a sub-agent's calls never inherit the parent's.
func withCalledAs(ctx context.Context, name string) context.Context {
	return context.WithValue(ctx, calledAsKey{}, name)
}

func calledAs(ctx context.Context) string {
	name, _ := ctx.Value(calledAsKey{}).(string)
	return name
}

// finishToolCall runs the PostToolUse hooks for a finished call and returns
// the output to hand the model. After a successful file-edit or file-write,
// a hook that rewrites the file (gofmt -w, prettier --write) is part of the
// agent's own write: the file is re-stamped so the stale-read
// guard doesn't refuse the next edit, and the model is told the file changed.
func (r *toolRuntime) finishToolCall(ctx context.Context, call toolCall, out string, err error) string {
	abs, rel, ok := "", "", false
	if err == nil && r.hasPostToolHooks(ctx, call.Tool) {
		// Which file was written depends on the built-in that carried the
		// call out (an unfolded multi-edit is file-edit's code).
		if run, runErr := r.builtinCall(call); runErr == nil {
			abs, rel, ok = r.writtenFile(run)
		}
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
	for _, rule := range r.toolHookRules(ctx, hooks.EventPermissionRequest, toolID) {
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
