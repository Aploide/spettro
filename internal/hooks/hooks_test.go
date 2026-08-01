package hooks

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestMatch(t *testing.T) {
	cases := []struct {
		matcher string
		toolID  string
		want    bool
	}{
		{"", "shell-exec", true},
		{"*", "shell-exec", true},
		{"  *  ", "shell-exec", true},
		{"shell-exec", "shell-exec", true},
		{"shell-exec", "read-file", false},
		{"shell-*", "shell-exec", true},
		{"shell-*", "read-file", false},
		{"re:^shell-", "shell-exec", true},
		{"re:^shell-", "read-file", false},
		{"re:(", "shell-exec", false},
		{"[", "shell-exec", false},
	}
	for _, c := range cases {
		rule := EffectiveRule{Rule: Rule{Matcher: c.matcher}}
		if got := Match(rule, c.toolID); got != c.want {
			t.Errorf("Match(%q, %q) = %v, want %v", c.matcher, c.toolID, got, c.want)
		}
	}
}

func writeHooksFile(t *testing.T, path string, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadEffectiveMergesAndValidates(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cwd := t.TempDir()

	writeHooksFile(t, filepath.Join(home, ".spettro", "hooks.json"), `{"hooks":[
		{"id":"shared","event":"PreToolUse","matcher":"*","command":"echo global"},
		{"id":"global-only","event":"PostToolUse","command":"echo post"},
		{"id":"bad-event","event":"Nope","command":"echo x"},
		{"id":"no-cmd","event":"PreToolUse","command":"  "}
	]}`)
	writeHooksFile(t, filepath.Join(cwd, ".spettro", "hooks.json"), `[
		{"id":"shared","event":"PreToolUse","matcher":"*","command":"echo project"},
		{"event":"SessionStart","command":"echo start","enabled":false}
	]`)

	cfg, err := LoadEffective(cwd)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Rules) != 3 {
		t.Fatalf("got %d rules, want 3: %+v", len(cfg.Rules), cfg.Rules)
	}
	byID := map[string]EffectiveRule{}
	for _, r := range cfg.Rules {
		byID[r.ID] = r
	}
	// project rule with same merge key overrides the global one
	if got := byID["shared"]; got.Command != "echo project" || got.Source != "project" {
		t.Errorf("shared rule not overridden by project: %+v", got)
	}
	if got := byID["global-only"]; got.Source != "global" || !got.Enabled {
		t.Errorf("global-only rule wrong: %+v", got)
	}
	// auto-generated ID for the project rule without one, enabled:false honored
	if got := byID["project-2"]; got.Enabled || got.Event != EventSessionStart {
		t.Errorf("project-2 rule wrong: %+v", got)
	}
	if len(cfg.Issues) != 2 {
		t.Fatalf("got %d issues, want 2: %+v", len(cfg.Issues), cfg.Issues)
	}
}

func TestLoadEffectiveMissingFiles(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfg, err := LoadEffective(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Rules) != 0 || len(cfg.Issues) != 0 {
		t.Errorf("expected empty config, got %+v", cfg)
	}
}

func TestLoadEffectiveInvalidJSON(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cwd := t.TempDir()
	writeHooksFile(t, filepath.Join(cwd, ".spettro", "hooks.json"), `{not json`)
	if _, err := LoadEffective(cwd); err == nil {
		t.Fatal("expected error for invalid JSON config")
	}
}

func TestRunPassesInputAndEnv(t *testing.T) {
	rule := EffectiveRule{Rule: Rule{
		ID:      "env",
		Event:   EventPreToolUse,
		Command: `cat > /dev/null; echo "$SPETTRO_HOOK_EVENT/$SPETTRO_HOOK_TOOL_ID"`,
	}}
	res, err := Run(context.Background(), rule, RunInput{Event: EventPreToolUse, ToolID: "shell-exec"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Stdout != "PreToolUse/shell-exec" {
		t.Errorf("stdout = %q", res.Stdout)
	}
}

func TestRunReadsInputFromStdin(t *testing.T) {
	rule := EffectiveRule{Rule: Rule{ID: "stdin", Event: EventPreToolUse, Command: "cat"}}
	args := json.RawMessage(`{"command":"ls"}`)
	res, err := Run(context.Background(), rule, RunInput{Event: EventPreToolUse, ToolID: "shell-exec", ToolArgs: args})
	if err != nil {
		t.Fatal(err)
	}
	var in RunInput
	if err := json.Unmarshal([]byte(res.Stdout), &in); err != nil {
		t.Fatalf("stdin was not echoed as JSON: %v (%q)", err, res.Stdout)
	}
	if in.ToolID != "shell-exec" || string(in.ToolArgs) != `{"command":"ls"}` {
		t.Errorf("input round-trip wrong: %+v", in)
	}
}

func TestRunParsesDecisionEnvelope(t *testing.T) {
	rule := EffectiveRule{Rule: Rule{
		ID:    "decide",
		Event: EventPreToolUse,
		Command: `echo "some log line"
echo '{"decision":"Deny","reason":" nope ","updated_args":{"command":"ls -la"}}'`,
	}}
	res, err := Run(context.Background(), rule, RunInput{Event: EventPreToolUse})
	if err != nil {
		t.Fatal(err)
	}
	if res.Decision != "deny" {
		t.Errorf("decision = %q, want deny", res.Decision)
	}
	if res.Reason != "nope" {
		t.Errorf("reason = %q", res.Reason)
	}
	if string(res.UpdatedArgs) != `{"command":"ls -la"}` {
		t.Errorf("updated_args = %q", res.UpdatedArgs)
	}
}

func TestRunNonJSONStdoutIsNotADecision(t *testing.T) {
	rule := EffectiveRule{Rule: Rule{ID: "plain", Event: EventPostToolUse, Command: "echo just text"}}
	res, err := Run(context.Background(), rule, RunInput{Event: EventPostToolUse})
	if err != nil {
		t.Fatal(err)
	}
	if res.Decision != "" {
		t.Errorf("decision = %q, want empty", res.Decision)
	}
	if res.Stdout != "just text" {
		t.Errorf("stdout = %q", res.Stdout)
	}
}

func TestRunFailureReturnsError(t *testing.T) {
	rule := EffectiveRule{Rule: Rule{ID: "boom", Event: EventPreToolUse, Command: "echo oops >&2; exit 3"}}
	res, err := Run(context.Background(), rule, RunInput{Event: EventPreToolUse})
	if err == nil {
		t.Fatal("expected error for non-zero exit")
	}
	if res.Stderr != "oops" {
		t.Errorf("stderr = %q", res.Stderr)
	}
}

func TestRunTimeout(t *testing.T) {
	rule := EffectiveRule{Rule: Rule{ID: "slow", Event: EventPreToolUse, Command: "sleep 10", TimeoutSec: 1}}
	start := time.Now()
	_, err := Run(context.Background(), rule, RunInput{Event: EventPreToolUse})
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("timeout took too long: %v", elapsed)
	}
}

func TestLastNonEmptyLine(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{"one", "one"},
		{"one\ntwo\n", "two"},
		{"one\n\n  \n", "one"},
		{"one\n two \n\n", "two"},
	}
	for _, c := range cases {
		if got := lastNonEmptyLine(c.in); got != c.want {
			t.Errorf("lastNonEmptyLine(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
