package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	agentprompts "spettro/agents"
	"spettro/internal/config"
)

// TestLoadPromptUsesEmbeddedDefaultOutsideRepo pins the fix for the biggest
// context gap: in a foreign repo (no agents/ dir) the coding agent must get the
// built-in prompt, not its one-line manifest description.
func TestLoadPromptUsesEmbeddedDefaultOutsideRepo(t *testing.T) {
	cwd := t.TempDir()
	got := loadPromptOrFallback(cwd, "agents/coding.md", "Coding orchestrator")
	if got == "Coding orchestrator" {
		t.Fatal("expected the embedded coding prompt, got the description fallback")
	}
	if strings.HasPrefix(got, "---") {
		t.Fatalf("frontmatter must be stripped from the embedded prompt:\n%s", got[:min(len(got), 200)])
	}
	raw, ok := agentprompts.Prompt("agents/coding.md")
	if !ok || stripFrontmatter(raw) != got {
		t.Fatal("loader result must equal the embedded agents/coding.md body")
	}
}

func TestLoadPromptProjectManifestFileWins(t *testing.T) {
	cwd := t.TempDir()
	writeFileAt(t, filepath.Join(cwd, config.AgentManifestFilename), "# project manifest\n")
	writeFileAt(t, filepath.Join(cwd, "agents", "coding.md"), "---\nname: coding\n---\n\nProject-specific coding prompt.")
	if got := loadPromptOrFallback(cwd, "agents/coding.md", "fallback"); got != "Project-specific coding prompt." {
		t.Fatalf("with a project manifest, its prompt file must win, got %q", truncate(got, 80))
	}
}

// TestLoadPromptIgnoresUnrelatedAgentsDir: a repo that happens to have an
// agents/ folder (its own AI product's prompts) must not replace Spettro's
// built-in prompts unless the project ships a Spettro manifest.
func TestLoadPromptIgnoresUnrelatedAgentsDir(t *testing.T) {
	cwd := t.TempDir()
	writeFileAt(t, filepath.Join(cwd, "agents", "reviewer.md"), "You review customer support tickets.")
	got := loadPromptOrFallback(cwd, "agents/reviewer.md", "fallback")
	raw, _ := agentprompts.Prompt("agents/reviewer.md")
	if got != stripFrontmatter(raw) {
		t.Fatalf("expected the embedded reviewer prompt, got %q", truncate(got, 80))
	}
}

func TestLoadPromptDotSpettroOverride(t *testing.T) {
	cwd := t.TempDir()
	writeFileAt(t, filepath.Join(cwd, ".spettro", "agents", "coding.md"), "Override from .spettro.")
	writeFileAt(t, filepath.Join(cwd, config.AgentManifestFilename), "# project manifest\n")
	writeFileAt(t, filepath.Join(cwd, "agents", "coding.md"), "Manifest-relative prompt.")
	if got := loadPromptOrFallback(cwd, "agents/coding.md", "fallback"); got != "Override from .spettro." {
		t.Fatalf(".spettro/agents override must win, got %q", truncate(got, 80))
	}
}

func TestLoadPromptCustomPromptFileWithoutManifest(t *testing.T) {
	cwd := t.TempDir()
	writeFileAt(t, filepath.Join(cwd, "prompts", "triage.md"), "Custom triage prompt.")
	if got := loadPromptOrFallback(cwd, "prompts/triage.md", "fallback"); got != "Custom triage prompt." {
		t.Fatalf("a non-built-in prompt_file is read from the project, got %q", truncate(got, 80))
	}
}

func TestLoadPromptBlankOverrideFallsThroughToEmbedded(t *testing.T) {
	cwd := t.TempDir()
	writeFileAt(t, filepath.Join(cwd, ".spettro", "agents", "explore.md"), "  \n")
	got := loadPromptOrFallback(cwd, "agents/explore.md", "fallback")
	if got == "fallback" || !strings.Contains(got, "explore") {
		t.Fatalf("blank override should fall through to the embedded prompt, got %q", got)
	}
}

func TestLoadPromptUnknownFileUsesFallback(t *testing.T) {
	cwd := t.TempDir()
	for _, rel := range []string{"agents/does-not-exist.md", "prompts/coding.md", "", "../agents/coding.md"} {
		if got := loadPromptOrFallback(cwd, rel, "desc"); got != "desc" {
			t.Errorf("prompt_file %q: expected fallback, got %q", rel, truncate(got, 80))
		}
	}
}

func TestLoadPromptAbsolutePath(t *testing.T) {
	p := filepath.Join(t.TempDir(), "custom.md")
	if err := os.WriteFile(p, []byte("Absolute prompt."), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := loadPromptOrFallback(t.TempDir(), p, "desc"); got != "Absolute prompt." {
		t.Fatalf("absolute prompt_file must be read directly, got %q", got)
	}
}

// TestEveryManifestPromptFileIsEmbedded guards against adding an agent whose
// prompt_file isn't shipped in the binary.
func TestEveryManifestPromptFileIsEmbedded(t *testing.T) {
	for _, spec := range config.DefaultAgentManifest().Agents {
		if spec.PromptFile == "" {
			continue
		}
		if _, ok := agentprompts.Prompt(spec.PromptFile); !ok {
			t.Errorf("agent %q: prompt_file %q is not embedded", spec.ID, spec.PromptFile)
		}
	}
}

// TestCodingPromptContracts pins the practices the built-in coding prompt must
// teach, and keeps the old per-answer ceremony from creeping back.
func TestCodingPromptContracts(t *testing.T) {
	raw, ok := agentprompts.Prompt("agents/coding.md")
	if !ok {
		t.Fatal("agents/coding.md is not embedded")
	}
	body := stripFrontmatter(raw)
	for _, needle := range []string{
		"Understand before editing",
		"file-edit",
		"Verify",
		"Read the full error output",
		"non-interactively",
		"state the assumption",
		"Don't create files the task doesn't need",
		"shared test namespaces",
	} {
		if !strings.Contains(body, needle) {
			t.Errorf("coding prompt missing %q", needle)
		}
	}
	for _, banned := range []string{"Restate the request", "## Plan", "## Remaining Risks", "Keep parallel batches to 2"} {
		if strings.Contains(body, banned) {
			t.Errorf("coding prompt still carries rigid ceremony %q", banned)
		}
	}
}

// TestCodingPromptEvidenceContracts pins the habits a benchmark showed the
// coding agent lacking: long thinking before running anything, one test for
// several symptoms, assertions weakened until they passed, and a reference
// library trusted as the spec (then fuzzed well outside the task).
func TestCodingPromptEvidenceContracts(t *testing.T) {
	raw, ok := agentprompts.Prompt("agents/coding.md")
	if !ok {
		t.Fatal("agents/coding.md is not embedded")
	}
	body := stripFrontmatter(raw)
	for _, needle := range []string{
		"Get evidence early",
		"run the failing test",
		"instead of simulating it at length in your head",
		"short steps between tool calls",
		"each reported symptom and each stated requirement its own check",
		"at the strength the task states",
		"Never weaken or delete an assertion",
		"an aid, not the spec",
		"boundary cases",
		"Stay in scope",
	} {
		if !strings.Contains(body, needle) {
			t.Errorf("coding prompt missing %q", needle)
		}
	}
}

// TestCodingPromptReadsNarrowly: a benchmark replay showed broad up-front
// reads (callers, callees, whole files, several at once) and per-task ceremony
// calls (a separate build-system lookup, deleting scratch scripts) inflating
// both reasoning and call counts. The prompt must start from what the task
// points to and keep scratch files out of the repo.
func TestCodingPromptReadsNarrowly(t *testing.T) {
	raw, ok := agentprompts.Prompt("agents/coding.md")
	if !ok {
		t.Fatal("agents/coding.md is not embedded")
	}
	body := stripFrontmatter(raw)
	for _, needle := range []string{"Start from what the task points to", "Widen only when", "An existing failing test is the reproduction", "$TMPDIR"} {
		if !strings.Contains(body, needle) {
			t.Errorf("coding prompt missing %q", needle)
		}
	}
	for _, banned := range []string{"code they call or are called by", "read several files at once", "Read whole files", "Find out how the project builds", "delete throwaway scripts", "Remove temporary files"} {
		if strings.Contains(body, banned) {
			t.Errorf("coding prompt still carries %q", banned)
		}
	}
}

// TestCodingPromptFinalAnswerIsShort keeps the report guidance brief: a few
// lines covering change, verification and caveats.
func TestCodingPromptFinalAnswerIsShort(t *testing.T) {
	raw, ok := agentprompts.Prompt("agents/coding.md")
	if !ok {
		t.Fatal("agents/coding.md is not embedded")
	}
	_, section, found := strings.Cut(stripFrontmatter(raw), "# Final answer")
	if !found {
		t.Fatal("coding prompt has no Final answer section")
	}
	for _, needle := range []string{"A few lines", "what you changed", "how you verified", "caveats"} {
		if !strings.Contains(section, needle) {
			t.Errorf("Final answer section missing %q", needle)
		}
	}
}

// TestCodingPromptLSPIsOptional: the lsp tool is absent (or has no server)
// in many projects, so the coding prompt may only mention it conditionally;
// an unconditional nudge wastes calls on "no lsp server configured" errors.
func TestCodingPromptLSPIsOptional(t *testing.T) {
	raw, ok := agentprompts.Prompt("agents/coding.md")
	if !ok {
		t.Fatal("agents/coding.md is not embedded")
	}
	for _, line := range strings.Split(stripFrontmatter(raw), "\n") {
		for _, sentence := range strings.Split(line, ". ") {
			lower := strings.ToLower(sentence)
			if !strings.Contains(lower, "`lsp`") && !strings.Contains(lower, "language-server") && !strings.Contains(lower, "language server") {
				continue
			}
			if !strings.Contains(lower, "if ") {
				t.Errorf("coding prompt mentions the language server unconditionally: %q", sentence)
			}
		}
	}
}

// TestPromptsDoNotMandateCommentNarration guards against prompts that make the
// model spend steps on the comment tool.
func TestPromptsDoNotMandateCommentNarration(t *testing.T) {
	for _, name := range []string{"coding", "code", "explore", "general-purpose", "tester", "reviewer", "git", "docs-writer", "chat"} {
		raw, ok := agentprompts.Prompt("agents/" + name + ".md")
		if !ok {
			t.Fatalf("agents/%s.md is not embedded", name)
		}
		for _, banned := range []string{"before each write/exec op", "before each test command", "before each major git operation"} {
			if strings.Contains(raw, banned) {
				t.Errorf("agents/%s.md mandates comment narration (%q)", name, banned)
			}
		}
	}
}

// TestCodingPromptNamesOnlyGrantedTools keeps the coding prompt from steering
// the model to a tool its agent doesn't have (a wasted, failing step): every
// built-in tool it names in backticks must be allowed for the coding agent in
// both the built-in manifest and the repo's own spettro.agents.toml.
func TestCodingPromptNamesOnlyGrantedTools(t *testing.T) {
	raw, ok := agentprompts.Prompt("agents/coding.md")
	if !ok {
		t.Fatal("agents/coding.md is not embedded")
	}
	body := stripFrontmatter(raw)
	repoManifest, err := config.LoadAgentManifest(filepath.Join("..", "..", config.AgentManifestFilename))
	if err != nil {
		t.Fatalf("load repo manifest: %v", err)
	}
	manifests := map[string]config.AgentManifest{"built-in": config.DefaultAgentManifest(), "spettro.agents.toml": repoManifest}
	for label, m := range manifests {
		spec, ok := m.AgentByID("coding")
		if !ok {
			t.Fatalf("%s: no coding agent", label)
		}
		allowed, _ := resolveToolPolicies(spec, &m)
		granted := map[string]bool{}
		for _, id := range allowed {
			granted[id] = true
		}
		for tool := range builtinNativeToolDescs {
			if strings.Contains(body, "`"+tool+"`") && !granted[tool] {
				t.Errorf("%s: coding prompt names `%s`, which the coding agent is not allowed to use", label, tool)
			}
		}
	}
}

// TestGitPromptFollowsTheRepoConventions: the git worker's prompt now ships in
// every project, so it must defer to the repository's own commit style and
// must not carry Spettro's scope table or issue refs.
func TestGitPromptFollowsTheRepoConventions(t *testing.T) {
	raw, ok := agentprompts.Prompt("agents/git.md")
	if !ok {
		t.Fatal("agents/git.md is not embedded")
	}
	if !strings.Contains(raw, "Match the repository's existing convention") {
		t.Error("git prompt must tell the worker to match the repository's commit style")
	}
	for _, banned := range []string{"Every commit Spettro produces follows", "project conventions for Spettro", "`internal/agent/*`", "aploide/spettro"} {
		if strings.Contains(raw, banned) {
			t.Errorf("git prompt still carries Spettro-specific convention %q", banned)
		}
	}
}
