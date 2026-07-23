package config_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"spettro/internal/config"
)

// projectRoot resolves the repository root from the test's CWD. Tests run
// from `tests/config/`, so the repo lives two directories up.
func projectRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	root := filepath.Clean(filepath.Join(wd, "..", ".."))
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("expected go.mod at repo root %s: %v", root, err)
	}
	return root
}

func TestDefaultAgentManifestIsValid(t *testing.T) {
	m := config.DefaultAgentManifest()
	if err := m.Validate(); err != nil {
		t.Fatalf("default manifest should validate: %v", err)
	}
	if m.DefaultAgent != "plan" {
		t.Fatalf("expected plan as default agent, got %q", m.DefaultAgent)
	}
	coding, ok := m.AgentByID("coding")
	if !ok {
		t.Fatal("expected default manifest to include coding agent")
	}
	for _, toolID := range []string{"file-write", "file-edit", "shell-exec", "bash", "ls"} {
		if !slices.Contains(coding.AllowedTools, toolID) {
			t.Fatalf("coding agent should allow %q", toolID)
		}
	}
	for _, action := range []string{"write", "execute", "git"} {
		if !slices.Contains(coding.PermittedActions, action) {
			t.Fatalf("coding agent should permit %q actions", action)
		}
	}
}

// TestPlanAgent_DelegatesAllDiscovery locks in the orchestration contract:
// the plan orchestrator owns NO direct read tools. Every fact it needs
// about the repository has to come back through an `agent` delegation to
// the `explore` worker (or similar). If a future refactor accidentally
// re-grants glob/grep/file-read to plan, this test fails loudly.
func TestPlanAgent_DelegatesAllDiscovery(t *testing.T) {
	m := config.DefaultAgentManifest()
	plan, ok := m.AgentByID("plan")
	if !ok {
		t.Fatal("expected default manifest to include plan agent")
	}
	for _, denied := range []string{"glob", "grep", "file-read", "ls", "shell-exec", "bash", "file-write", "file-edit"} {
		if slices.Contains(plan.AllowedTools, denied) {
			t.Fatalf("plan orchestrator must NOT carry %q; delegate discovery to explore instead", denied)
		}
	}
	if !slices.Contains(plan.AllowedTools, "agent") {
		t.Fatal("plan orchestrator MUST keep the `agent` tool — without it, delegation is impossible")
	}
	if !slices.Contains(plan.Handoffs, "explore") {
		t.Fatal("plan orchestrator MUST have explore in its handoff list")
	}
}

// TestCodeWorker_UsesDedicatedPromptFile guards against the previous setup
// where the `code` worker and `coding` orchestrator shared a prompt file.
// They now have distinct prompts so the worker can't accidentally inherit
// orchestrator-style "delegate everything" guidance.
func TestCodeWorker_UsesDedicatedPromptFile(t *testing.T) {
	m := config.DefaultAgentManifest()
	code, ok := m.AgentByID("code")
	if !ok {
		t.Fatal("expected default manifest to include code worker")
	}
	if code.PromptFile != "agents/code.md" {
		t.Fatalf("code worker should use agents/code.md, got %q", code.PromptFile)
	}
	coding, _ := m.AgentByID("coding")
	if coding.PromptFile == code.PromptFile {
		t.Fatal("coding orchestrator and code worker must not share a prompt file")
	}
}

// TestCodingOrchestrator_CanDelegateToAllWorkers makes the handoff list a
// hard contract: the coding orchestrator MUST be able to spawn every
// downstream specialist (code, git, test, review, docs, explore) so the
// "delegate first" prompt is actually executable.
func TestCodingOrchestrator_CanDelegateToAllWorkers(t *testing.T) {
	m := config.DefaultAgentManifest()
	coding, ok := m.AgentByID("coding")
	if !ok {
		t.Fatal("expected default manifest to include coding orchestrator")
	}
	if !slices.Contains(coding.AllowedTools, "agent") {
		t.Fatal("coding orchestrator MUST keep the `agent` tool")
	}
	required := []string{"code", "git", "test", "review", "docs", "explore"}
	for _, target := range required {
		if !slices.Contains(coding.Handoffs, target) {
			t.Fatalf("coding orchestrator must declare %q as a handoff target", target)
		}
		if _, ok := m.AgentByID(target); !ok {
			t.Fatalf("handoff target %q must exist in the manifest", target)
		}
	}
}

// TestGitPromptKeepsKeyContracts pins the non-negotiable parts of the git
// agent prompt so future rewrites can't accidentally drop them. The
// contracts we care about:
//   - mandatory Co-Authored-By: Spettro trailer (cross-checked with the
//     runtime auto-injection in internal/agent/commit_policy.go);
//   - Conventional Commits format with imperative subjects;
//   - mandatory inspection pipeline (`git status`, `git log`, `git diff`)
//     before any mutation, so the agent never writes a message before
//     reading what changed.
func TestGitPromptKeepsKeyContracts(t *testing.T) {
	root := projectRoot(t)
	raw, err := os.ReadFile(filepath.Join(root, "agents", "git.md"))
	if err != nil {
		t.Fatalf("read agents/git.md: %v", err)
	}
	body := string(raw)
	mustContain := []struct {
		needle string
		why    string
	}{
		{"Co-Authored-By: Spettro <spettro@eyed.to>", "mandatory commit co-author trailer"},
		{"Conventional Commits", "Conventional Commits format must be enforced"},
		{"imperative", "subjects must be in the imperative mood"},
		{"git status", "inspection pipeline must include git status"},
		{"git log", "inspection pipeline must include git log to learn project style"},
		{"git diff", "inspection pipeline must include git diff to read the change"},
		{"≤72", "subject-line length cap must be specified"},
		{"Never push", "push-without-permission must be forbidden"},
	}
	for _, mc := range mustContain {
		if !strings.Contains(body, mc.needle) {
			t.Errorf("agents/git.md missing %q (%s)", mc.needle, mc.why)
		}
	}
}

// TestProjectManifest_MatchesOrchestrationContract loads the actual
// `spettro.agents.toml` shipped with the repo and asserts the same
// invariants we enforce on DefaultAgentManifest. This keeps the on-disk
// manifest from quietly drifting away from the fallback when someone
// edits one without the other.
func TestProjectManifest_MatchesOrchestrationContract(t *testing.T) {
	root := projectRoot(t)
	m, err := config.LoadAgentManifestForProject(root)
	if err != nil {
		t.Fatalf("LoadAgentManifestForProject(%q): %v", root, err)
	}
	plan, ok := m.AgentByID("plan")
	if !ok {
		t.Fatal("spettro.agents.toml must define a plan agent")
	}
	for _, denied := range []string{"glob", "grep", "file-read", "ls", "shell-exec", "bash", "file-write", "file-edit"} {
		if slices.Contains(plan.AllowedTools, denied) {
			t.Errorf("project manifest: plan must NOT carry %q (delegate to explore instead)", denied)
		}
	}
	if !slices.Contains(plan.AllowedTools, "agent") {
		t.Error("project manifest: plan MUST keep the `agent` tool")
	}
	if !slices.Contains(plan.Handoffs, "explore") {
		t.Error("project manifest: plan MUST list explore as a handoff")
	}

	code, ok := m.AgentByID("code")
	if !ok {
		t.Fatal("spettro.agents.toml must define a code worker")
	}
	if code.PromptFile != "agents/code.md" {
		t.Errorf("project manifest: code worker must use agents/code.md, got %q", code.PromptFile)
	}

	coding, ok := m.AgentByID("coding")
	if !ok {
		t.Fatal("spettro.agents.toml must define a coding orchestrator")
	}
	for _, target := range []string{"code", "git", "test", "review", "docs", "explore"} {
		if !slices.Contains(coding.Handoffs, target) {
			t.Errorf("project manifest: coding orchestrator must list %q in handoffs", target)
		}
	}
}

func TestDecodeAgentManifest(t *testing.T) {
	raw := `
version = 1
default_agent = "plan"

[metadata]
name = "Test agents"
description = "Manifest for tests"

[runtime]
default_permission = "ask-first"
default_timeout_sec = 90
allow_network_tools = false
log_tool_calls = true

[[tools]]
id = "repo-search"
name = "Repository Search"
description = "Searches files"
kind = "builtin"
enabled = true
timeout_sec = 30
requires_approval = false
permitted_actions = ["read", "search"]

[[tools]]
id = "provider-chat"
name = "Provider Chat"
description = "Calls active provider"
kind = "builtin"
enabled = true
timeout_sec = 60
requires_approval = false
permitted_actions = ["chat"]

[[agents]]
id = "plan"
name = "Planning"
description = "Plans work"
skill = "architecture"
mode = "orchestrator"
allowed_tools = ["repo-search"]
permitted_actions = ["read", "search", "plan"]
permission = "ask-first"
temperature = 0.2
max_tokens = 2048
max_steps = 10
handoffs = ["ask"]
enabled = true

[[agents]]
id = "ask"
name = "Ask"
description = "Chat mode"
skill = "conversation"
mode = "orchestrator"
allowed_tools = ["provider-chat", "repo-search"]
permitted_actions = ["chat", "read"]
permission = "restricted"
temperature = 0.5
max_tokens = 4096
max_steps = 8
handoffs = ["plan"]
enabled = true
`
	m, err := config.DecodeAgentManifest(strings.NewReader(raw))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(m.Agents) != 2 {
		t.Fatalf("expected 2 agents, got %d", len(m.Agents))
	}
	if len(m.EnabledToolsForAgent("ask")) != 2 {
		t.Fatalf("expected 2 enabled tools for ask")
	}
}

func TestDecodeAgentManifestUnknownToolRefFails(t *testing.T) {
	raw := `
version = 1
default_agent = "plan"

[runtime]
default_permission = "ask-first"
default_timeout_sec = 60

[[tools]]
id = "repo-search"
name = "Repository Search"
description = "Searches files"
kind = "builtin"
enabled = true
timeout_sec = 30
requires_approval = false
permitted_actions = ["read", "search"]

[[agents]]
id = "plan"
name = "Planning"
description = "Plans work"
skill = "architecture"
mode = "orchestrator"
allowed_tools = ["missing-tool"]
permitted_actions = ["plan"]
permission = "ask-first"
max_steps = 5
handoffs = []
enabled = true
`
	_, err := config.DecodeAgentManifest(strings.NewReader(raw))
	if err == nil {
		t.Fatal("expected validation error")
	}
	if !strings.Contains(err.Error(), "unknown tool") {
		t.Fatalf("expected unknown tool error, got %v", err)
	}
}

func TestDecodeAgentManifest_V1IsAutoNormalizedToV2(t *testing.T) {
	raw := `
version = 1
default_agent = "plan"

[runtime]
default_permission = "ask-first"
default_timeout_sec = 60
allow_network_tools = false
log_tool_calls = true

[[tools]]
id = "repo-search"
name = "Repository Search"
description = "Searches files"
kind = "builtin"
enabled = true
timeout_sec = 30
requires_approval = false
permitted_actions = ["read", "search"]

[[agents]]
id = "plan"
name = "Planning"
description = "Plans work"
skill = "architecture"
mode = "orchestrator"
allowed_tools = ["repo-search"]
permitted_actions = ["read", "search", "plan"]
permission = "ask-first"
max_steps = 10
handoffs = []
enabled = true
`
	m, original, changed, err := config.DecodeAgentManifestWithMigrationInfo(strings.NewReader(raw))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if original != 1 {
		t.Fatalf("expected original version 1, got %d", original)
	}
	if !changed {
		t.Fatal("expected migration change flag for v1 manifest")
	}
	if m.Version != 9 {
		t.Fatalf("expected normalized version 9, got %d", m.Version)
	}
}
