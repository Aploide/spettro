package tui_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"spettro/internal/tui"
)

const minimalSKILL = `---
name: pdf-processing
description: Extract text from PDFs and fill PDF forms. Use when working with PDF documents.
---

# PDF Processing
`

func TestHandleCommand_SkillListEmpty(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	m := tui.NewModelForTesting()
	next, _ := m.HandleCommandForTesting("/skill list")
	got := next.(tui.Model)
	msgs := got.MessagesForTesting()
	if len(msgs) == 0 {
		t.Fatal("expected system message after /skill list")
	}
	last := msgs[len(msgs)-1].Content
	if !strings.Contains(strings.ToLower(last), "no skills discovered") {
		t.Errorf("expected empty hint, got %q", last)
	}
	if !strings.Contains(last, "search roots") {
		t.Errorf("expected search-roots listing in empty output, got %q", last)
	}
}

func TestHandleCommand_SkillInstallFromLocalPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	src := t.TempDir()
	skillDir := filepath.Join(src, "pdf-processing")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(minimalSKILL), 0o644); err != nil {
		t.Fatalf("write SKILL.md: %v", err)
	}

	m := tui.NewModelForTesting()
	next, _ := m.HandleCommandForTesting("/skill install " + skillDir)
	got := next.(tui.Model)

	if !strings.Contains(strings.ToLower(got.BannerForTesting()), "installed") {
		t.Errorf("expected install banner, got %q", got.BannerForTesting())
	}

	dest := filepath.Join(home, ".spettro", "skills", "pdf-processing", "SKILL.md")
	if _, err := os.Stat(dest); err != nil {
		t.Errorf("expected installed SKILL.md at %s, err=%v", dest, err)
	}

	listed, _ := got.HandleCommandForTesting("/skill list")
	listedM := listed.(tui.Model)
	msgs := listedM.MessagesForTesting()
	last := msgs[len(msgs)-1].Content
	if !strings.Contains(last, "pdf-processing") {
		t.Errorf("expected /skill list to mention installed skill, got %q", last)
	}
}

func TestHandleCommand_SkillUninstallRemovesSkill(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	src := t.TempDir()
	skillDir := filepath.Join(src, "pdf-processing")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(minimalSKILL), 0o644); err != nil {
		t.Fatalf("write SKILL.md: %v", err)
	}

	m := tui.NewModelForTesting()
	next, _ := m.HandleCommandForTesting("/skill install " + skillDir)
	got := next.(tui.Model)

	next2, _ := got.HandleCommandForTesting("/skill uninstall pdf-processing")
	got2 := next2.(tui.Model)
	if !strings.Contains(strings.ToLower(got2.BannerForTesting()), "uninstalled") {
		t.Errorf("expected uninstall banner, got %q", got2.BannerForTesting())
	}
	if _, err := os.Stat(filepath.Join(home, ".spettro", "skills", "pdf-processing")); !os.IsNotExist(err) {
		t.Errorf("expected skill directory removed, err=%v", err)
	}
}

func TestHandleCommand_SkillsAliasWorks(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	m := tui.NewModelForTesting()
	next, _ := m.HandleCommandForTesting("/skills")
	got := next.(tui.Model)
	if len(got.MessagesForTesting()) == 0 {
		t.Fatal("expected /skills to produce a system message")
	}
}

func TestHandleCommand_SkillInfoShowsExcerpt(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := filepath.Join(home, ".spettro", "skills", "pdf-processing")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "SKILL.md"), []byte(minimalSKILL), 0o644); err != nil {
		t.Fatalf("write SKILL.md: %v", err)
	}

	m := tui.NewModelForTesting()
	next, _ := m.HandleCommandForTesting("/skill info pdf-processing")
	got := next.(tui.Model)
	msgs := got.MessagesForTesting()
	if len(msgs) == 0 {
		t.Fatal("expected /skill info to produce a system message")
	}
	last := msgs[len(msgs)-1].Content
	if !strings.Contains(last, "pdf-processing") {
		t.Errorf("expected info to mention skill name, got %q", last)
	}
	if !strings.Contains(last, "PDF Processing") {
		t.Errorf("expected info to include body excerpt, got %q", last)
	}
}

func TestHandleCommand_SkillDisableEnable(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := filepath.Join(home, ".spettro", "skills", "pdf-processing")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "SKILL.md"), []byte(minimalSKILL), 0o644); err != nil {
		t.Fatalf("write SKILL.md: %v", err)
	}

	m := tui.NewModelForTesting()
	next, _ := m.HandleCommandForTesting("/skill disable pdf-processing")
	got := next.(tui.Model)
	if !strings.Contains(strings.ToLower(got.BannerForTesting()), "disabled") {
		t.Errorf("expected disabled banner, got %q", got.BannerForTesting())
	}
	if _, err := os.Stat(filepath.Join(root, ".spettro-disabled")); err != nil {
		t.Errorf("expected disabled marker file, err=%v", err)
	}

	next2, _ := got.HandleCommandForTesting("/skill enable pdf-processing")
	got2 := next2.(tui.Model)
	if !strings.Contains(strings.ToLower(got2.BannerForTesting()), "enabled") {
		t.Errorf("expected enabled banner, got %q", got2.BannerForTesting())
	}
	if _, err := os.Stat(filepath.Join(root, ".spettro-disabled")); !os.IsNotExist(err) {
		t.Errorf("expected disabled marker removed, err=%v", err)
	}
}

func TestHandleCommand_SkillWhereLists8Roots(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	m := tui.NewModelForTesting()
	next, _ := m.HandleCommandForTesting("/skill where")
	got := next.(tui.Model)
	msgs := got.MessagesForTesting()
	if len(msgs) == 0 {
		t.Fatal("expected /skill where to produce a system message")
	}
	last := msgs[len(msgs)-1].Content
	for _, fragment := range []string{".spettro/skills", ".agents/skills", ".claude/skills", ".openai/skills"} {
		if !strings.Contains(last, fragment) {
			t.Errorf("expected /skill where output to contain %q, got %q", fragment, last)
		}
	}
}

func TestHandleCommand_SkillInstallRejectsUnknownFlag(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	m := tui.NewModelForTesting()
	next, _ := m.HandleCommandForTesting("/skill install /tmp/whatever --bogus")
	got := next.(tui.Model)
	if !strings.Contains(strings.ToLower(got.BannerForTesting()), "unknown flag") {
		t.Errorf("expected unknown-flag banner, got %q", got.BannerForTesting())
	}
}
