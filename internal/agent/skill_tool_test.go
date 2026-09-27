package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"spettro/internal/config"
	"spettro/internal/skills"
)

// writeSkillDir writes root/<name>/SKILL.md with the given frontmatter lines
// and body.
func writeSkillDir(t *testing.T, root, name, front, body string) {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	content := "---\nname: " + name + "\n" + front + "---\n" + body
	if err := os.WriteFile(filepath.Join(dir, skills.SkillFilename), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// skillToolRuntime returns a runtime whose catalog holds three skills in the
// project's .spettro/skills: a normal one, one only the user may run, and
// one disabled.
func skillToolRuntime(t *testing.T) *toolRuntime {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	r, dir := newAliasTestRuntime(t)
	root := filepath.Join(dir, ".spettro", "skills")
	writeSkillDir(t, root, "greet", "description: Greets someone\n", "Say hello to $ARGUMENTS.\n")
	writeSkillDir(t, root, "deploy", "description: Deploys\ndisable-model-invocation: true\n", "Deploy.\n")
	writeSkillDir(t, root, "off", "description: Off\ndisabled: true\n", "Off.\n")
	cat, err := skills.Discover(dir, skills.DefaultLookupOptions())
	if err != nil {
		t.Fatal(err)
	}
	r.skillsCatalog = cat
	return r
}

func runSkillCall(t *testing.T, r *toolRuntime, tool string, args any) (string, error) {
	t.Helper()
	return r.execute(context.Background(), aliasCall(t, tool, args), map[string]struct{}{"skill": {}})
}

func TestSkillToolLoadsBodyWithArgs(t *testing.T) {
	r := skillToolRuntime(t)
	out, err := runSkillCall(t, r, "skill", map[string]any{"name": "greet", "args": "Ada"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `<skill_content name="greet">`) || !strings.Contains(out, "Say hello to Ada.") {
		t.Errorf("activation output:\n%s", out)
	}
	if !strings.Contains(out, "Skill directory: ") {
		t.Error("the model must be told where the skill's bundled files are")
	}
}

// Every retired name reaches the same tool; skill-list lists.
func TestSkillToolAliases(t *testing.T) {
	r := skillToolRuntime(t)
	for _, tool := range []string{"skill-read", "activate-skill", "skill-activate"} {
		out, err := runSkillCall(t, r, tool, map[string]any{"name": "greet"})
		if err != nil || !strings.Contains(out, "Say hello to .") {
			t.Errorf("%s: out=%q err=%v", tool, out, err)
		}
	}
	// Claude Code's Skill tool spells the name "skill".
	if out, err := runSkillCall(t, r, "skill", map[string]any{"skill": "greet"}); err != nil || !strings.Contains(out, "greet") {
		t.Errorf("skill field: out=%q err=%v", out, err)
	}
	out, err := runSkillCall(t, r, "skill-list", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	var rows []skillListRow
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("listing is not JSON: %v\n%s", err, out)
	}
	if len(rows) != 1 || rows[0].Name != "greet" {
		t.Errorf("listing = %+v, want only the model-invocable skill", rows)
	}
}

func TestSkillToolRefusals(t *testing.T) {
	r := skillToolRuntime(t)
	cases := map[string]string{
		"deploy":  "can only be run by the user",
		"off":     "disabled",
		"missing": "available: greet",
	}
	for name, want := range cases {
		_, err := runSkillCall(t, r, "skill", map[string]any{"name": name})
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("skill %q: err = %v, want it to mention %q", name, err, want)
		}
	}
	if _, err := runSkillCall(t, r, "skill", map[string]any{"location": "/etc/passwd"}); err == nil {
		t.Error("a location outside the catalog must be refused")
	}
}

func TestSkillToolListFilter(t *testing.T) {
	r := skillToolRuntime(t)
	out, err := runSkillCall(t, r, "skill", map[string]any{"query": "nothing-matches"})
	if err != nil || out != "[]" {
		t.Errorf("filtered listing = %q, %v", out, err)
	}
}

// The system prompt lists only model-invocable skills and is advertised
// with the skill tool.
func TestSkillCatalogInSystemPrompt(t *testing.T) {
	r := skillToolRuntime(t)
	prompt := buildSystemStringWith(toolLoopConfig{SystemPrompt: "base", SkillsCatalog: r.skillsCatalog}, "")
	if !strings.Contains(prompt, "- greet: Greets someone") {
		t.Errorf("system prompt lacks the greet line:\n%s", prompt)
	}
	if strings.Contains(prompt, "deploy") || strings.Contains(prompt, "- off") {
		t.Errorf("user-only and disabled skills leaked into the prompt:\n%s", prompt)
	}
	surface := r.buildToolSurface([]string{"skill"}, "")
	if len(surface.deferredNames()) != 0 {
		t.Errorf("with skills installed the skill tool must be advertised, deferred = %v", surface.deferredNames())
	}
}

// SkillCatalogFor honours the user config: compat discovery switch and the
// disabled_skills list.
func TestSkillCatalogForHonoursConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", "")
	cwd := t.TempDir()
	writeSkillDir(t, filepath.Join(cwd, ".spettro", "skills"), "native", "description: d\n", "b\n")
	writeSkillDir(t, filepath.Join(home, ".claude", "skills"), "claude-one", "description: d\n", "b\n")
	ReloadSkills()
	t.Cleanup(ReloadSkills)

	names := func(c skills.Catalog) []string {
		var out []string
		for _, s := range c.Active() {
			out = append(out, s.Name)
		}
		return out
	}
	if got := names(SkillCatalogFor(cwd, config.UserConfig{})); strings.Join(got, ",") != "claude-one,native" {
		t.Errorf("default catalog = %v", got)
	}
	if got := names(SkillCatalogFor(cwd, config.UserConfig{SkillsCompatDisabled: true})); strings.Join(got, ",") != "native" {
		t.Errorf("compat off = %v", got)
	}
	if got := names(SkillCatalogFor(cwd, config.UserConfig{DisabledSkills: []string{"Native"}})); strings.Join(got, ",") != "claude-one" {
		t.Errorf("disabled native = %v", got)
	}
}
