package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"spettro/internal/agent"
	"spettro/internal/config"
	"spettro/internal/skills"
)

func TestResolveHeadlessPrompt(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CODEX_HOME", "")
	cwd := t.TempDir()
	for _, name := range []string{"greet", "mode"} {
		dir := filepath.Join(cwd, ".spettro", "skills", name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		content := "---\nname: " + name + "\ndescription: d\n---\nSay hello to $ARGUMENTS.\n"
		if err := os.WriteFile(filepath.Join(dir, skills.SkillFilename), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	agent.ReloadSkills()
	t.Cleanup(agent.ReloadSkills)
	cfg := config.UserConfig{}

	run, isSkill, err := resolveHeadlessPrompt(cwd, cfg, "/greet Ada")
	if err != nil || !isSkill || !strings.Contains(run, "Say hello to Ada.") {
		t.Errorf("/greet: run=%q isSkill=%v err=%v", run, isSkill, err)
	}
	// A headless command wins over a skill of the same name.
	if run, isSkill, _ := resolveHeadlessPrompt(cwd, cfg, "/mode"); isSkill || run != "/mode" {
		t.Errorf("/mode must stay a command: run=%q isSkill=%v", run, isSkill)
	}
	if run, isSkill, _ := resolveHeadlessPrompt(cwd, cfg, "/unknown x"); isSkill || run != "/unknown x" {
		t.Errorf("unknown command: run=%q isSkill=%v", run, isSkill)
	}
	run, isSkill, _ = resolveHeadlessPrompt(cwd, cfg, "try $greet")
	if isSkill || !strings.HasPrefix(run, "try $greet") || !strings.Contains(run, "<skill_content") {
		t.Errorf("$mention: run=%q isSkill=%v", run, isSkill)
	}
}
