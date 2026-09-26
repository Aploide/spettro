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

func TestLoadPromptProjectOverrideWins(t *testing.T) {
	cwd := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cwd, "agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	override := "---\nname: coding\n---\n\nProject-specific coding prompt."
	if err := os.WriteFile(filepath.Join(cwd, "agents", "coding.md"), []byte(override), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := loadPromptOrFallback(cwd, "agents/coding.md", "fallback"); got != "Project-specific coding prompt." {
		t.Fatalf("project override must win, got %q", got)
	}
}

func TestLoadPromptBlankOverrideFallsThroughToEmbedded(t *testing.T) {
	cwd := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cwd, "agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cwd, "agents", "explore.md"), []byte("  \n"), 0o644); err != nil {
		t.Fatal(err)
	}
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
