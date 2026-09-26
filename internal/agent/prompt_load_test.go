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
	writeTestFile(t, filepath.Join(cwd, config.AgentManifestFilename), "# project manifest\n")
	writeTestFile(t, filepath.Join(cwd, "agents", "coding.md"), "---\nname: coding\n---\n\nProject-specific coding prompt.")
	if got := loadPromptOrFallback(cwd, "agents/coding.md", "fallback"); got != "Project-specific coding prompt." {
		t.Fatalf("with a project manifest, its prompt file must win, got %q", truncate(got, 80))
	}
}

// TestLoadPromptIgnoresUnrelatedAgentsDir: a repo that happens to have an
// agents/ folder (its own AI product's prompts) must not replace Spettro's
// built-in prompts unless the project ships a Spettro manifest.
func TestLoadPromptIgnoresUnrelatedAgentsDir(t *testing.T) {
	cwd := t.TempDir()
	writeTestFile(t, filepath.Join(cwd, "agents", "reviewer.md"), "You review customer support tickets.")
	got := loadPromptOrFallback(cwd, "agents/reviewer.md", "fallback")
	raw, _ := agentprompts.Prompt("agents/reviewer.md")
	if got != stripFrontmatter(raw) {
		t.Fatalf("expected the embedded reviewer prompt, got %q", truncate(got, 80))
	}
}

func TestLoadPromptDotSpettroOverride(t *testing.T) {
	cwd := t.TempDir()
	writeTestFile(t, filepath.Join(cwd, ".spettro", "agents", "coding.md"), "Override from .spettro.")
	writeTestFile(t, filepath.Join(cwd, config.AgentManifestFilename), "# project manifest\n")
	writeTestFile(t, filepath.Join(cwd, "agents", "coding.md"), "Manifest-relative prompt.")
	if got := loadPromptOrFallback(cwd, "agents/coding.md", "fallback"); got != "Override from .spettro." {
		t.Fatalf(".spettro/agents override must win, got %q", truncate(got, 80))
	}
}

func TestLoadPromptCustomPromptFileWithoutManifest(t *testing.T) {
	cwd := t.TempDir()
	writeTestFile(t, filepath.Join(cwd, "prompts", "triage.md"), "Custom triage prompt.")
	if got := loadPromptOrFallback(cwd, "prompts/triage.md", "fallback"); got != "Custom triage prompt." {
		t.Fatalf("a non-built-in prompt_file is read from the project, got %q", truncate(got, 80))
	}
}

func TestLoadPromptBlankOverrideFallsThroughToEmbedded(t *testing.T) {
	cwd := t.TempDir()
	writeTestFile(t, filepath.Join(cwd, ".spettro", "agents", "explore.md"), "  \n")
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
