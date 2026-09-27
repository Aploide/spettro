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
	// The install runs as a command off the UI goroutine (a git clone can
	// take a while): nothing is installed until its result comes back.
	next, cmd := m.HandleCommandForTesting("/skill install " + skillDir)
	got := next.(tui.Model)
	if !strings.Contains(got.BannerForTesting(), "installing") || cmd == nil {
		t.Fatalf("install must start in the background: banner %q, cmd %v", got.BannerForTesting(), cmd != nil)
	}
	if _, err := os.Stat(filepath.Join(home, ".spettro", "skills", "pdf-processing")); !os.IsNotExist(err) {
		t.Fatalf("install ran inside the command handler (err=%v)", err)
	}
	next, _ = got.UpdateForTesting(cmd())
	got = next.(tui.Model)

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
	next, cmd := m.HandleCommandForTesting("/skill install " + skillDir)
	next, _ = next.(tui.Model).UpdateForTesting(cmd())
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

	// lastMessage runs a command and returns the system message it printed.
	lastMessage := func(m tui.Model, cmd string) (tui.Model, string) {
		t.Helper()
		next, _ := m.HandleCommandForTesting(cmd)
		got := next.(tui.Model)
		msgs := got.MessagesForTesting()
		if len(msgs) == 0 {
			t.Fatalf("%s printed nothing", cmd)
		}
		return got, msgs[len(msgs)-1].Content
	}

	m := tui.NewModelForTesting()
	next, _ := m.HandleCommandForTesting("/skill disable pdf-processing")
	got := next.(tui.Model)
	if !strings.Contains(strings.ToLower(got.BannerForTesting()), "disabled") {
		t.Errorf("expected disabled banner, got %q", got.BannerForTesting())
	}
	// The choice lives in the user config; the skill folder is not touched
	// (it may belong to Claude Code or Codex).
	if _, err := os.Stat(filepath.Join(root, ".spettro-disabled")); !os.IsNotExist(err) {
		t.Errorf("disable must not write a marker into the skill folder, err=%v", err)
	}
	got, list := lastMessage(got, "/skills")
	if !strings.Contains(list, "pdf-processing  [disabled]") {
		t.Errorf("expected the skill listed as disabled, got %q", list)
	}

	next2, _ := got.HandleCommandForTesting("/skill enable pdf-processing")
	got2 := next2.(tui.Model)
	if !strings.Contains(strings.ToLower(got2.BannerForTesting()), "enabled") {
		t.Errorf("expected enabled banner, got %q", got2.BannerForTesting())
	}
	_, list = lastMessage(got2, "/skills")
	if !strings.Contains(list, "pdf-processing  [/pdf-processing]") {
		t.Errorf("expected the skill runnable again, got %q", list)
	}
}

// A marker written by an older Spettro still disables the skill, and
// /skill enable removes it.
func TestHandleCommand_SkillEnableRemovesLegacyMarker(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := filepath.Join(home, ".spettro", "skills", "pdf-processing")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "SKILL.md"), []byte(minimalSKILL), 0o644); err != nil {
		t.Fatalf("write SKILL.md: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, ".spettro-disabled"), nil, 0o644); err != nil {
		t.Fatalf("write marker: %v", err)
	}
	m := tui.NewModelForTesting()
	next, _ := m.HandleCommandForTesting("/skill enable pdf-processing")
	if !strings.Contains(strings.ToLower(next.(tui.Model).BannerForTesting()), "enabled") {
		t.Errorf("expected enabled banner, got %q", next.(tui.Model).BannerForTesting())
	}
	if _, err := os.Stat(filepath.Join(root, ".spettro-disabled")); !os.IsNotExist(err) {
		t.Errorf("expected legacy marker removed, err=%v", err)
	}
}

// Older versions of /skill disable wrote the marker into any skill folder,
// Claude Code's and Codex's included. It is Spettro's own file, so
// /skill enable removes it there too, and the skill is runnable again.
func TestHandleCommand_SkillEnableRemovesLegacyMarkerInCompatRoot(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", "")
	root := filepath.Join(home, ".claude", "skills", "pdf-processing")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "SKILL.md"), []byte(minimalSKILL), 0o644); err != nil {
		t.Fatalf("write SKILL.md: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, ".spettro-disabled"), nil, 0o644); err != nil {
		t.Fatalf("write marker: %v", err)
	}
	m := tui.NewModelForTesting()
	next, _ := m.HandleCommandForTesting("/skill enable pdf-processing")
	got := next.(tui.Model)
	if banner := got.BannerForTesting(); !strings.Contains(banner, "enabled") || got.BannerKindForTesting() != "success" {
		t.Errorf("expected a success banner, got %q (%s)", banner, got.BannerKindForTesting())
	}
	if _, err := os.Stat(filepath.Join(root, ".spettro-disabled")); !os.IsNotExist(err) {
		t.Errorf("expected legacy marker removed from the Claude Code folder, err=%v", err)
	}
	next, _ = got.HandleCommandForTesting("/skills")
	msgs := next.(tui.Model).MessagesForTesting()
	if list := msgs[len(msgs)-1].Content; !strings.Contains(list, "pdf-processing  [/pdf-processing]") {
		t.Errorf("expected the skill runnable again, got %q", list)
	}
}

// A skill its own SKILL.md disables (disabled: true) cannot be enabled from
// Spettro; /skill enable must say so instead of reporting success.
func TestHandleCommand_SkillEnableReportsFrontmatterDisable(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := filepath.Join(home, ".spettro", "skills", "pdf-processing")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	content := strings.Replace(minimalSKILL, "name: pdf-processing\n", "name: pdf-processing\ndisabled: true\n", 1)
	if err := os.WriteFile(filepath.Join(root, "SKILL.md"), []byte(content), 0o644); err != nil {
		t.Fatalf("write SKILL.md: %v", err)
	}
	m := tui.NewModelForTesting()
	next, _ := m.HandleCommandForTesting("/skill enable pdf-processing")
	got := next.(tui.Model)
	if got.BannerKindForTesting() == "success" || !strings.Contains(got.BannerForTesting(), "disabled: true") {
		t.Errorf("expected the frontmatter disable reported, got %q (%s)", got.BannerForTesting(), got.BannerKindForTesting())
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
	// The listing shows real filesystem paths, so the separator is the
	// platform's; compare against the joined spelling rather than a literal.
	for _, dir := range []string{".spettro", ".agents", ".claude", ".codex", ".openai"} {
		fragment := filepath.Join(dir, "skills")
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
