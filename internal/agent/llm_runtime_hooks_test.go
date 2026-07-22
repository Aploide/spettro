package agent

import (
	"context"
	"encoding/json"
	"testing"

	"spettro/internal/hooks"
)

func hookRule(id string, event hooks.Event, matcher, command string) hooks.EffectiveRule {
	return hooks.EffectiveRule{
		Rule:    hooks.Rule{ID: id, Event: event, Matcher: matcher, Command: command},
		Source:  "test",
		Enabled: true,
	}
}

func TestRunPreToolHooksDenyBlocks(t *testing.T) {
	r := &toolRuntime{hooksConfig: hooks.EffectiveConfig{Rules: []hooks.EffectiveRule{
		hookRule("deny-all", hooks.EventPreToolUse, "*", `echo '{"decision":"deny","reason":"not allowed"}'`),
	}}}
	updated, denyReason, err := r.runPreToolHooks(context.Background(), "shell-exec", json.RawMessage(`{"command":"ls"}`))
	if err != nil {
		t.Fatal(err)
	}
	if denyReason != "not allowed" {
		t.Errorf("deny reason = %q, want %q", denyReason, "not allowed")
	}
	if updated != nil {
		t.Errorf("args should be nil on deny, got %s", updated)
	}
}

func TestRunPreToolHooksDenyFallbackReason(t *testing.T) {
	r := &toolRuntime{hooksConfig: hooks.EffectiveConfig{Rules: []hooks.EffectiveRule{
		hookRule("quiet-deny", hooks.EventPreToolUse, "*", `echo '{"decision":"block"}'`),
	}}}
	_, denyReason, err := r.runPreToolHooks(context.Background(), "shell-exec", nil)
	if err != nil {
		t.Fatal(err)
	}
	if denyReason != "hook quiet-deny denied request" {
		t.Errorf("deny reason = %q", denyReason)
	}
}

func TestRunPreToolHooksAllowRewritesShellArgs(t *testing.T) {
	r := &toolRuntime{hooksConfig: hooks.EffectiveConfig{Rules: []hooks.EffectiveRule{
		hookRule("rewrite", hooks.EventPreToolUse, "*", `echo '{"decision":"allow","updated_args":{"command":"ls -la"}}'`),
	}}}
	updated, denyReason, err := r.runPreToolHooks(context.Background(), "shell-exec", json.RawMessage(`{"command":"ls"}`))
	if err != nil {
		t.Fatal(err)
	}
	if denyReason != "" {
		t.Fatalf("unexpected deny: %q", denyReason)
	}
	if string(updated) != `{"command":"ls -la"}` {
		t.Errorf("updated args = %s", updated)
	}
}

func TestRunPreToolHooksNoRewriteForNonShellTools(t *testing.T) {
	r := &toolRuntime{hooksConfig: hooks.EffectiveConfig{Rules: []hooks.EffectiveRule{
		hookRule("rewrite", hooks.EventPreToolUse, "*", `echo '{"decision":"allow","updated_args":{"path":"/etc/passwd"}}'`),
	}}}
	orig := json.RawMessage(`{"path":"a.txt"}`)
	updated, _, err := r.runPreToolHooks(context.Background(), "read-file", orig)
	if err != nil {
		t.Fatal(err)
	}
	if string(updated) != string(orig) {
		t.Errorf("args rewritten for non-shell tool: %s", updated)
	}
}

func TestRunPreToolHooksSkipsDisabledAndNonMatching(t *testing.T) {
	disabled := hookRule("disabled", hooks.EventPreToolUse, "*", `echo '{"decision":"deny"}'`)
	disabled.Enabled = false
	r := &toolRuntime{hooksConfig: hooks.EffectiveConfig{Rules: []hooks.EffectiveRule{
		disabled,
		hookRule("other-tool", hooks.EventPreToolUse, "read-*", `echo '{"decision":"deny"}'`),
		hookRule("other-event", hooks.EventPostToolUse, "*", `echo '{"decision":"deny"}'`),
	}}}
	updated, denyReason, err := r.runPreToolHooks(context.Background(), "shell-exec", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if denyReason != "" {
		t.Errorf("unexpected deny: %q", denyReason)
	}
	if string(updated) != `{}` {
		t.Errorf("args changed: %s", updated)
	}
}

func TestRunPreToolHooksFailingHookPropagatesError(t *testing.T) {
	r := &toolRuntime{hooksConfig: hooks.EffectiveConfig{Rules: []hooks.EffectiveRule{
		hookRule("boom", hooks.EventPreToolUse, "*", "exit 1"),
	}}}
	if _, _, err := r.runPreToolHooks(context.Background(), "shell-exec", nil); err == nil {
		t.Fatal("expected error from failing hook")
	}
}

func TestRunPermissionRequestHooks(t *testing.T) {
	r := &toolRuntime{hooksConfig: hooks.EffectiveConfig{Rules: []hooks.EffectiveRule{
		hookRule("auto-allow", hooks.EventPermissionRequest, "shell-*", `echo '{"decision":"allow","reason":"trusted"}'`),
	}}}
	decision, reason, err := r.runPermissionRequestHooks(context.Background(), "shell-exec", "ls")
	if err != nil {
		t.Fatal(err)
	}
	if decision != "allow" || reason != "trusted" {
		t.Errorf("got (%q, %q), want (allow, trusted)", decision, reason)
	}

	decision, _, err = r.runPermissionRequestHooks(context.Background(), "read-file", "")
	if err != nil {
		t.Fatal(err)
	}
	if decision != "" {
		t.Errorf("non-matching tool should yield no decision, got %q", decision)
	}
}

func TestRunPostToolHooksError(t *testing.T) {
	r := &toolRuntime{hooksConfig: hooks.EffectiveConfig{Rules: []hooks.EffectiveRule{
		hookRule("post-ok", hooks.EventPostToolUse, "*", "cat > /dev/null"),
	}}}
	if err := r.runPostToolHooks(context.Background(), "shell-exec", nil, "output"); err != nil {
		t.Fatal(err)
	}

	r = &toolRuntime{hooksConfig: hooks.EffectiveConfig{Rules: []hooks.EffectiveRule{
		hookRule("post-fail", hooks.EventPostToolUse, "*", "exit 2"),
	}}}
	if err := r.runPostToolHooks(context.Background(), "shell-exec", nil, "output"); err == nil {
		t.Fatal("expected error from failing post hook")
	}
}
