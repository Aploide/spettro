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
