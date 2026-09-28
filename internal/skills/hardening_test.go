package skills

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// YAML folds a plain scalar that continues on more-indented lines into one
// line; the parser must keep the continuation instead of dropping it (the
// "Use when ..." half of a description is what the model triggers on).
func TestParseMultiLineScalars(t *testing.T) {
	s, err := parse(`---
name: pdf
description: Extract text from PDFs and fill forms.
  Use when the user mentions PDFs.
when_to_use: "when the file
  ends in .pdf"
license:
  MIT
compatibility: any # trailing comment
---
Body`, "pdf")
	if err != nil {
		t.Fatal(err)
	}
	if s.Description != "Extract text from PDFs and fill forms. Use when the user mentions PDFs." {
		t.Errorf("description = %q", s.Description)
	}
	if s.WhenToUse != "when the file ends in .pdf" {
		t.Errorf("when_to_use = %q", s.WhenToUse)
	}
	if s.License != "MIT" {
		t.Errorf("license on the next line = %q", s.License)
	}
	if s.Compatibility != "any" {
		t.Errorf("compatibility = %q", s.Compatibility)
	}
}

// Frontmatter comes from untrusted folders (a cloned repository's
// .claude/skills), and the name, description and argument hint are drawn
// straight onto the terminal by the / menu and the $ palette. Escape
// sequences and control characters must not survive parsing.
func TestParseStripsTerminalControls(t *testing.T) {
	s, err := parse("---\nname: \"x\x1b[31my\"\ndescription: \"a\x1b]0;pwn\x07b\x1b]52;c;ZXZpbA==\x07c\"\nargument-hint: \"[\x00file\x7f]\"\nfoo\x1b[2J: bar\u009b\n---\nBody\n", "xy")
	if err != nil {
		t.Fatal(err)
	}
	for field, got := range map[string]string{"name": s.Name, "description": s.Description, "argument-hint": s.ArgumentHint} {
		if strings.ContainsFunc(got, func(r rune) bool { return r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) }) {
			t.Errorf("%s kept a control character: %q", field, got)
		}
	}
	if s.Name != "xy" || s.Description != "abc" || s.ArgumentHint != "[file]" {
		t.Errorf("name=%q description=%q hint=%q", s.Name, s.Description, s.ArgumentHint)
	}
	for k, v := range s.Metadata {
		if strings.ContainsAny(k+v, "\x1b\u009b") {
			t.Errorf("metadata kept an escape: %q=%q", k, v)
		}
	}
}

// A skill runs as "/<name> args", so a name with whitespace could never be
// run: the first word would be taken as the name. The directory name stands
// in, or the name with its whitespace turned into hyphens.
func TestParseNameWithWhitespace(t *testing.T) {
	s, err := parse("---\nname: PDF Tools\ndescription: d\n---\nBody\n", "pdf-tools")
	if err != nil {
		t.Fatal(err)
	}
	if s.Name != "pdf-tools" || len(s.Issues) == 0 {
		t.Errorf("name = %q issues = %v, want the directory name and a warning", s.Name, s.Issues)
	}
	s, err = parse("---\ndescription: d\n---\nBody\n", "My  Skill")
	if err != nil {
		t.Fatal(err)
	}
	if s.Name != "My-Skill" {
		t.Errorf("name = %q, want whitespace turned into a hyphen", s.Name)
	}
}

// Codex finds skills grouped in subfolders of its skill roots
// (~/.codex/skills/<group>/<skill>/SKILL.md); .agents/skills is Codex's too.
// Claude Code's roots stay one level deep.
func TestDiscoverNestedCodexSkills(t *testing.T) {
	home := isolateHome(t)
	cwd := t.TempDir()
	writeRaw(t, filepath.Join(home, ".codex", "skills", "tools"), "lint", "---\nname: lint\ndescription: d\n---\nb\n")
	writeRaw(t, filepath.Join(cwd, ".agents", "skills", "a", "b"), "deep", "---\nname: deep\ndescription: d\n---\nb\n")
	writeRaw(t, filepath.Join(cwd, ".claude", "skills", "group"), "hidden-in-claude", "---\nname: hidden-in-claude\ndescription: d\n---\nb\n")
	writeRaw(t, filepath.Join(cwd, ".agents", "skills", ".cache"), "dot", "---\nname: dot\ndescription: d\n---\nb\n")
	// A skill's own folders are not searched for more skills.
	outer := writeRaw(t, filepath.Join(home, ".codex", "skills"), "outer", "---\nname: outer\ndescription: d\n---\nb\n")
	writeRaw(t, filepath.Join(outer, "scripts"), "inner", "---\nname: inner\ndescription: d\n---\nb\n")

	cat, err := Discover(cwd, DefaultLookupOptions())
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, s := range cat.Skills {
		names = append(names, s.Name)
	}
	if !slices.Equal(names, []string{"deep", "lint", "outer"}) {
		t.Errorf("skills = %v, want deep, lint and outer", names)
	}
}

// SKILL.md files come from cloned repositories. One over the size limit is
// reported and skipped rather than read into memory. (A FIFO is covered in
// hardening_unix_test.go.)
func TestDiscoverRefusesHugeManifests(t *testing.T) {
	isolateHome(t)
	cwd := t.TempDir()
	root := filepath.Join(cwd, ".claude", "skills")
	writeRaw(t, root, "huge", "---\nname: huge\ndescription: d\n---\n"+strings.Repeat("x", maxSkillFileBytes))
	writeRaw(t, root, "fine", "---\nname: fine\ndescription: d\n---\nb\n")
	cat, _ := Discover(cwd, DefaultLookupOptions())
	if len(cat.Skills) != 1 || cat.Skills[0].Name != "fine" {
		t.Errorf("skills = %+v, want fine alone", cat.Skills)
	}
	if len(cat.Issues) != 1 || !strings.Contains(cat.Issues[0], "limit") {
		t.Errorf("issues = %v, want the size limit reported", cat.Issues)
	}
}

// The cache notices skills added, edited or removed on disk, so the
// long-lived ACP and headless processes (which have no /skill reload) pick
// them up, and it does not rescan while nothing changed.
func TestCacheNoticesChangesOnDisk(t *testing.T) {
	isolateHome(t)
	cwd := t.TempDir()
	root := filepath.Join(cwd, ".claude", "skills")
	writeSkill(t, root, "first", "first", "d")
	var c Cache
	names := func() []string {
		var out []string
		for _, s := range c.Get(cwd, DefaultLookupOptions()).Skills {
			out = append(out, s.Name+":"+s.Description)
		}
		return out
	}
	if got := names(); !slices.Equal(got, []string{"first:d"}) {
		t.Fatalf("first Get = %v", got)
	}

	// Unchanged stamps (same size, same modification time): no rescan.
	manifest := filepath.Join(root, "first", SkillFilename)
	info, err := os.Stat(manifest)
	if err != nil {
		t.Fatal(err)
	}
	writeSkill(t, root, "first", "first", "e")
	if err := os.Chtimes(manifest, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	if got := names(); !slices.Equal(got, []string{"first:d"}) {
		t.Errorf("Get rescanned although nothing it stamps changed: %v", got)
	}

	// An edit, an added skill (the root did not exist before) and a
	// SKILL.md added to a folder that had none are all picked up.
	later := info.ModTime().Add(2 * time.Second)
	if err := os.Chtimes(manifest, later, later); err != nil {
		t.Fatal(err)
	}
	if got := names(); !slices.Equal(got, []string{"first:e"}) {
		t.Errorf("an edited SKILL.md was not picked up: %v", got)
	}
	writeSkill(t, filepath.Join(cwd, ".spettro", "skills"), "second", "second", "d")
	if got := names(); !slices.Equal(got, []string{"first:e", "second:d"}) {
		t.Errorf("a new root was not picked up: %v", got)
	}
	if err := os.MkdirAll(filepath.Join(root, "third"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := names(); len(got) != 2 {
		t.Fatalf("a folder without SKILL.md is not a skill: %v", got)
	}
	writeSkill(t, root, "third", "third", "d")
	if got := names(); !slices.Equal(got, []string{"first:e", "second:d", "third:d"}) {
		t.Errorf("a SKILL.md added to an existing folder was not picked up: %v", got)
	}
	if err := os.RemoveAll(filepath.Join(root, "first")); err != nil {
		t.Fatal(err)
	}
	if got := names(); !slices.Equal(got, []string{"second:d", "third:d"}) {
		t.Errorf("a removed skill is still listed: %v", got)
	}
}
