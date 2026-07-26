package agent

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"spettro/internal/config"
	"spettro/internal/platform"
)

// These tests deliberately carry no build tag. The no-exec surface is a
// product decision, not an iOS build artifact, so it has to be verifiable on
// the desktop host that actually runs CI — hence every gated function takes
// an explicit `canExec bool` and the tests pass false. The one place that
// reads the real platform.CanExec() is runToolLoop; TestExecCapabilityMatchesPlatform
// below pins that the two agree.

// assembleTools reproduces exactly what runToolLoop does to turn a manifest
// and an agent spec into the tool set offered to the model: resolve the
// policies, apply the platform filter, then build the native tool specs.
// Keeping it in one helper means a change to that chain breaks these tests
// rather than silently bypassing them.
func assembleTools(t *testing.T, agentID string, canExec bool) []string {
	t.Helper()
	manifest := config.DefaultAgentManifest()
	spec, ok := manifest.AgentByID(agentID)
	if !ok {
		t.Fatalf("agent %q missing from the default manifest", agentID)
	}
	allowed, _ := resolveToolPolicies(spec, &manifest)
	allowed = filterExecTools(allowed, canExec)
	specs := buildToolSpecs(allowed, canExec)
	names := make([]string, 0, len(specs))
	for _, s := range specs {
		names = append(names, s.Name)
	}
	return names
}

// execCapableAgents are the shipped agents that hold shell-exec on desktop —
// i.e. every agent whose tool set the gate has to actually change.
var execCapableAgents = []string{"coding", "code", "git", "test", "review"}

func TestNoShellOrTerminalToolWithoutExec(t *testing.T) {
	// Substrings rather than exact IDs: a future tool called "bash-run" or
	// "shell-v2" must fail this test loudly instead of shipping to a device
	// that cannot run it.
	forbidden := []string{"shell", "bash", "pty", "terminal", "worktree"}
	for _, id := range execCapableAgents {
		names := assembleTools(t, id, false)
		for _, name := range names {
			for _, bad := range forbidden {
				if strings.Contains(name, bad) {
					t.Errorf("agent %q: tool %q offered with exec unavailable (matched %q)", id, name, bad)
				}
			}
		}
		// Guard against the filter accidentally emptying the toolset.
		if len(names) < 5 {
			t.Errorf("agent %q: only %d tools left after filtering (%v) — the gate is too broad", id, len(names), names)
		}
	}
}

func TestFileToolsSurviveWithoutExec(t *testing.T) {
	// The whole premise of the iOS port is that pure-Go file work still
	// works. If any of these ever disappears the product has no reason to
	// exist on the device.
	required := []string{"file-read", "file-write", "file-edit", "multi-edit", "glob", "grep", "ls"}
	names := assembleTools(t, "coding", false)
	for _, want := range required {
		if !slices.Contains(names, want) {
			t.Errorf("coding agent: %q missing with exec unavailable; have %v", want, names)
		}
	}
}

func TestExecGateChangesNothingWhenExecIsAvailable(t *testing.T) {
	for _, id := range execCapableAgents {
		manifest := config.DefaultAgentManifest()
		spec, _ := manifest.AgentByID(id)
		allowed, _ := resolveToolPolicies(spec, &manifest)
		if got := filterExecTools(allowed, true); !slices.Equal(got, allowed) {
			t.Errorf("agent %q: filter altered the toolset with exec available:\n got %v\nwant %v", id, got, allowed)
		}
		// And the shell tool really is there to be removed, so the negative
		// tests above are testing something.
		if !slices.Contains(allowed, "shell-exec") {
			t.Errorf("agent %q is in execCapableAgents but holds no shell-exec: %v", id, allowed)
		}
	}
}

func TestJobOutputSurvivesForSpoolPaging(t *testing.T) {
	// spoolTruncate's footer names job-output by hand, so a truncated
	// file-read or grep is unreadable without it. Removing it would strand
	// large outputs on exactly the platform where they matter most.
	names := assembleTools(t, "coding", false)
	if !slices.Contains(names, "job-output") {
		t.Fatalf("job-output must survive the exec gate (it pages spooled results); have %v", names)
	}
	if slices.Contains(names, "job-kill") {
		t.Fatalf("job-kill must not survive the exec gate (nothing can start a job); have %v", names)
	}
}

func TestNoExecToolDescriptionsReplaceExecAdvice(t *testing.T) {
	specs := buildToolSpecs([]string{"job-output", "view-image", "file-read"}, false)
	byName := map[string]string{}
	for _, s := range specs {
		byName[s.Name] = s.Description
	}
	if got := byName["job-output"]; got == builtinNativeToolDescs["job-output"] {
		t.Error("job-output kept its desktop description, which advertises background jobs")
	}
	if got, want := byName["view-image"], builtinNativeToolDescs["view-image"]; got == want {
		t.Error("view-image kept its desktop description, which tells the model to take screenshots with a shell")
	}
	// Untouched tools must keep their exact shipped wording.
	if got, want := byName["file-read"], builtinNativeToolDescs["file-read"]; got != want {
		t.Errorf("file-read description changed: got %q want %q", got, want)
	}
	// And with exec available nothing is substituted at all.
	for _, s := range buildToolSpecs([]string{"job-output", "view-image"}, true) {
		if s.Description != builtinNativeToolDescs[s.Name] {
			t.Errorf("%s description substituted with exec available", s.Name)
		}
	}
}

func TestSystemPromptExplainsTheMissingShell(t *testing.T) {
	base := toolLoopConfig{SystemPrompt: "You are a coding agent.", CWD: "/tmp/x"}

	withExec := buildSystemString(base)
	if strings.Contains(withExec, "no command execution") {
		t.Fatal("the no-exec section leaked into the normal desktop prompt")
	}

	noExec := base
	noExec.NoExec = true
	got := buildSystemString(noExec)
	if !strings.HasPrefix(got, withExec) {
		t.Fatal("the no-exec section must be appended, not substituted for the agent's own prompt")
	}
	// The specific things a shell-trained model would otherwise assume.
	for _, want := range []string{"no shell", "git", "test", "file-edit", "unverified"} {
		if !strings.Contains(got, want) {
			t.Errorf("no-exec system prompt never mentions %q", want)
		}
	}
	// Byte-stability: the system prompt is the head of the provider cache
	// prefix, so two builds of the same config must be identical.
	if buildSystemString(noExec) != got {
		t.Error("no-exec system prompt is not byte-stable across calls")
	}
}

func TestExecCapabilityMatchesPlatform(t *testing.T) {
	// runToolLoop gates on platform.CanExec(); on every platform that builds
	// this test binary that must be true, and the reason string must be
	// empty. This is what catches someone flipping the constant by accident.
	if !platform.CanExec() {
		t.Fatalf("platform.CanExec() is false on a test host; reason: %q", platform.ExecUnavailableReason())
	}
	if platform.ExecUnavailableReason() != "" {
		t.Errorf("ExecUnavailableReason must be empty when CanExec is true, got %q", platform.ExecUnavailableReason())
	}
}

func TestGatedToolsExistInTheShippedManifest(t *testing.T) {
	// A gate on a tool ID that no longer exists is a gate on nothing. If a
	// tool is renamed, this fails and the rename gets a matching entry.
	manifest := config.DefaultAgentManifest()
	known := map[string]struct{}{}
	for _, tool := range manifest.Tools {
		known[tool.ID] = struct{}{}
		for _, alias := range tool.Aliases {
			known[alias] = struct{}{}
		}
	}
	for _, id := range execDependentTools {
		if _, ok := known[id]; !ok {
			t.Errorf("gated tool %q is not defined in the default manifest — stale gate?", id)
		}
	}
}

func TestWalkSignatureTracksNestedEdits(t *testing.T) {
	// The exec-free progress fingerprint used by goal mode. The cwd-mtime
	// fallback it replaces cannot see this edit, which is the whole point.
	dir := t.TempDir()
	write := func(rel, content string) {
		t.Helper()
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("pkg/sub/a.go", "package sub\n")
	before := walkSignature(dir)
	if before == "" {
		t.Fatal("walkSignature returned empty for a populated tree")
	}
	if walkSignature(dir) != before {
		t.Fatal("walkSignature is not stable for an unchanged tree")
	}
	write("pkg/sub/a.go", "package sub\n\nfunc F() {}\n")
	if walkSignature(dir) == before {
		t.Fatal("walkSignature did not change after a nested file edit")
	}
	// .git churn must not read as agent progress.
	afterEdit := walkSignature(dir)
	write(".git/objects/ab/cdef", "junk")
	if walkSignature(dir) != afterEdit {
		t.Error("walkSignature changed for a .git-only change")
	}
}
