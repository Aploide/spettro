package skills_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"spettro/internal/skills"
)

const minimalSKILL = `---
name: pdf-processing
description: Extract text from PDFs and fill PDF forms. Use when working with PDF documents.
---

# PDF Processing

Use pdfplumber for text extraction.
`

const richSKILL = `---
name: code-review
description: |
  Run a structured code review with checklist, security checks,
  and final summary. Use when the user asks for a review.
license: Apache-2.0
compatibility: Designed for Spettro
metadata:
  author: spettro-team
  version: "1.0"
allowed-tools: "Bash(git:*) Read"
---

# Code Review

## Steps
1. Read the diff
2. Suggest improvements
`

func writeSkill(t *testing.T, dir, name, content string) string {
	t.Helper()
	skillDir := filepath.Join(dir, name)
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", skillDir, err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(content), 0o644); err != nil {
		t.Fatalf("write SKILL.md: %v", err)
	}
	return skillDir
}

func TestRead_MinimalSkill(t *testing.T) {
	tmp := t.TempDir()
	dir := writeSkill(t, tmp, "pdf-processing", minimalSKILL)

	skill, err := skills.Read(dir)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if skill.Name != "pdf-processing" {
		t.Errorf("name = %q, want pdf-processing", skill.Name)
	}
	if !strings.Contains(skill.Description, "Extract text from PDFs") {
		t.Errorf("description missing required text: %q", skill.Description)
	}
	if skill.Location != filepath.Join(dir, "SKILL.md") {
		t.Errorf("location = %q, want %q", skill.Location, filepath.Join(dir, "SKILL.md"))
	}
	if skill.Directory != dir {
		t.Errorf("directory = %q, want %q", skill.Directory, dir)
	}
}

func TestRead_RichSkill(t *testing.T) {
	tmp := t.TempDir()
	dir := writeSkill(t, tmp, "code-review", richSKILL)
	scriptDir := filepath.Join(dir, "scripts")
	if err := os.MkdirAll(scriptDir, 0o755); err != nil {
		t.Fatalf("mkdir scripts: %v", err)
	}
	if err := os.WriteFile(filepath.Join(scriptDir, "review.sh"), []byte("#!/bin/bash\necho ok\n"), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}

	skill, err := skills.Read(dir)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if skill.Name != "code-review" {
		t.Errorf("name = %q, want code-review", skill.Name)
	}
	if skill.License != "Apache-2.0" {
		t.Errorf("license = %q, want Apache-2.0", skill.License)
	}
	if skill.Compatibility != "Designed for Spettro" {
		t.Errorf("compatibility = %q", skill.Compatibility)
	}
	if !strings.Contains(skill.AllowedTools, "Bash(git:*)") {
		t.Errorf("allowed_tools = %q", skill.AllowedTools)
	}
	if skill.Metadata["author"] != "spettro-team" {
		t.Errorf("metadata author = %q", skill.Metadata["author"])
	}
	if skill.Metadata["version"] != "1.0" {
		t.Errorf("metadata version = %q", skill.Metadata["version"])
	}
	if !strings.Contains(strings.Join(skill.Resources, ","), "scripts/review.sh") {
		t.Errorf("resources missing scripts/review.sh: %v", skill.Resources)
	}
}

func TestRead_MissingFrontmatter(t *testing.T) {
	tmp := t.TempDir()
	dir := writeSkill(t, tmp, "broken", "# just markdown without frontmatter\n")

	if _, err := skills.Read(dir); err == nil {
		t.Fatal("expected error for missing frontmatter")
	}
}

func TestRead_MissingDescription(t *testing.T) {
	tmp := t.TempDir()
	dir := writeSkill(t, tmp, "no-desc", "---\nname: no-desc\n---\nbody\n")

	if _, err := skills.Read(dir); err == nil {
		t.Fatal("expected error for missing description")
	}
}

func TestLoadBody_ReturnsTrimmedBody(t *testing.T) {
	tmp := t.TempDir()
	dir := writeSkill(t, tmp, "pdf-processing", minimalSKILL)

	skill, err := skills.Read(dir)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	body, err := skills.LoadBody(skill)
	if err != nil {
		t.Fatalf("LoadBody: %v", err)
	}
	if !strings.Contains(body, "# PDF Processing") {
		t.Errorf("body missing markdown heading: %q", body)
	}
	if strings.Contains(body, "---") {
		t.Errorf("body should not contain frontmatter delimiter: %q", body)
	}
}

func TestDiscover_FindsSkillInProjectDir(t *testing.T) {
	cwd := t.TempDir()
	root := filepath.Join(cwd, ".spettro", "skills")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeSkill(t, root, "pdf-processing", minimalSKILL)

	t.Setenv("HOME", t.TempDir())

	cat, err := skills.Discover(cwd, skills.LookupOptions{IncludeProject: true})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(cat.Skills) != 1 {
		t.Fatalf("expected 1 skill, got %d", len(cat.Skills))
	}
	if cat.Skills[0].Name != "pdf-processing" {
		t.Errorf("name = %q", cat.Skills[0].Name)
	}
	if cat.Skills[0].Source != skills.SourceSpettro {
		t.Errorf("source = %q, want spettro", cat.Skills[0].Source)
	}
	if cat.Skills[0].Scope != skills.ScopeProject {
		t.Errorf("scope = %q, want project", cat.Skills[0].Scope)
	}
}

func TestDiscover_ProjectShadowsUser(t *testing.T) {
	cwd := t.TempDir()
	home := t.TempDir()
	t.Setenv("HOME", home)

	projectRoot := filepath.Join(cwd, ".spettro", "skills")
	userRoot := filepath.Join(home, ".spettro", "skills")
	for _, p := range []string{projectRoot, userRoot} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", p, err)
		}
	}
	writeSkill(t, projectRoot, "pdf-processing", strings.Replace(minimalSKILL,
		"Extract text from PDFs", "Project version", 1))
	writeSkill(t, userRoot, "pdf-processing", strings.Replace(minimalSKILL,
		"Extract text from PDFs", "User version", 1))

	cat, err := skills.Discover(cwd, skills.DefaultLookupOptions())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(cat.Skills) != 1 {
		t.Fatalf("expected 1 active skill (project shadows user), got %d", len(cat.Skills))
	}
	if !strings.Contains(cat.Skills[0].Description, "Project version") {
		t.Errorf("expected project description to win, got %q", cat.Skills[0].Description)
	}
	if len(cat.Shadowed) != 1 {
		t.Fatalf("expected 1 shadowed entry, got %d", len(cat.Shadowed))
	}
}

func TestDiscover_FindsSkillInClaudeDir(t *testing.T) {
	cwd := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	root := filepath.Join(cwd, ".claude", "skills")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeSkill(t, root, "pdf-processing", minimalSKILL)

	cat, err := skills.Discover(cwd, skills.DefaultLookupOptions())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(cat.Skills) != 1 {
		t.Fatalf("expected 1 skill, got %d", len(cat.Skills))
	}
	if cat.Skills[0].Source != skills.SourceClaude {
		t.Errorf("source = %q, want claude", cat.Skills[0].Source)
	}
}

func TestDiscover_FindsSkillInOpenAIDir(t *testing.T) {
	cwd := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	root := filepath.Join(cwd, ".openai", "skills")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeSkill(t, root, "pdf-processing", minimalSKILL)

	cat, err := skills.Discover(cwd, skills.DefaultLookupOptions())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(cat.Skills) != 1 || cat.Skills[0].Source != skills.SourceOpenAI {
		t.Fatalf("unexpected catalog: %+v", cat.Skills)
	}
}

func TestDiscover_FindsSkillInUserAgentsDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := filepath.Join(home, ".agents", "skills")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeSkill(t, root, "pdf-processing", minimalSKILL)

	cat, err := skills.Discover(t.TempDir(), skills.DefaultLookupOptions())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(cat.Skills) != 1 || cat.Skills[0].Source != skills.SourceAgents || cat.Skills[0].Scope != skills.ScopeUser {
		t.Fatalf("unexpected catalog: %+v", cat.Skills)
	}
}

func TestInstall_FromLocalDirectory(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	src := t.TempDir()
	writeSkill(t, src, "pdf-processing", minimalSKILL)

	res, err := skills.Install(context.Background(), skills.InstallOptions{
		Source: filepath.Join(src, "pdf-processing"),
		Scope:  skills.ScopeUser,
	})
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	expected := filepath.Join(home, ".spettro", "skills", "pdf-processing")
	if res.Destination != expected {
		t.Errorf("destination = %q, want %q", res.Destination, expected)
	}
	if _, err := os.Stat(filepath.Join(expected, "SKILL.md")); err != nil {
		t.Errorf("installed SKILL.md missing: %v", err)
	}
}

func TestInstall_FromParentWithOneSkill(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	src := t.TempDir()
	writeSkill(t, src, "pdf-processing", minimalSKILL)

	res, err := skills.Install(context.Background(), skills.InstallOptions{
		Source: src,
		Scope:  skills.ScopeUser,
	})
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if res.Skill.Name != "pdf-processing" {
		t.Errorf("name = %q", res.Skill.Name)
	}
}

func TestInstall_RejectsExistingWithoutForce(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	src := t.TempDir()
	writeSkill(t, src, "pdf-processing", minimalSKILL)

	_, err := skills.Install(context.Background(), skills.InstallOptions{
		Source: filepath.Join(src, "pdf-processing"),
		Scope:  skills.ScopeUser,
	})
	if err != nil {
		t.Fatalf("first install: %v", err)
	}
	_, err = skills.Install(context.Background(), skills.InstallOptions{
		Source: filepath.Join(src, "pdf-processing"),
		Scope:  skills.ScopeUser,
	})
	if err == nil {
		t.Fatal("expected duplicate install to fail without --force")
	}
	res, err := skills.Install(context.Background(), skills.InstallOptions{
		Source: filepath.Join(src, "pdf-processing"),
		Scope:  skills.ScopeUser,
		Force:  true,
	})
	if err != nil {
		t.Fatalf("force install: %v", err)
	}
	if !res.Replaced {
		t.Errorf("expected Replaced=true on force install")
	}
}

func TestInstall_ProjectScopeUsesCWD(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cwd := t.TempDir()
	src := t.TempDir()
	writeSkill(t, src, "pdf-processing", minimalSKILL)

	res, err := skills.Install(context.Background(), skills.InstallOptions{
		Source: filepath.Join(src, "pdf-processing"),
		Scope:  skills.ScopeProject,
		CWD:    cwd,
	})
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	expected := filepath.Join(cwd, ".spettro", "skills", "pdf-processing")
	if res.Destination != expected {
		t.Errorf("destination = %q, want %q", res.Destination, expected)
	}
}

func TestUninstall_RemovesInstalledSkill(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	src := t.TempDir()
	writeSkill(t, src, "pdf-processing", minimalSKILL)
	_, err := skills.Install(context.Background(), skills.InstallOptions{
		Source: filepath.Join(src, "pdf-processing"),
		Scope:  skills.ScopeUser,
	})
	if err != nil {
		t.Fatalf("Install: %v", err)
	}

	if err := skills.Uninstall("pdf-processing", skills.ScopeUser, ""); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".spettro", "skills", "pdf-processing")); !os.IsNotExist(err) {
		t.Errorf("expected skill directory removed, got err=%v", err)
	}
}

func TestCatalogPrompt_ContainsSkillsSection(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := filepath.Join(home, ".spettro", "skills")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeSkill(t, root, "pdf-processing", minimalSKILL)

	cat, err := skills.Discover(t.TempDir(), skills.DefaultLookupOptions())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	prompt := skills.CatalogPrompt(cat)
	if !strings.Contains(prompt, "<available_skills>") {
		t.Errorf("prompt missing <available_skills>: %q", prompt)
	}
	if !strings.Contains(prompt, "pdf-processing") {
		t.Errorf("prompt missing skill name: %q", prompt)
	}
	if !strings.Contains(prompt, "skill-read tool") {
		t.Errorf("prompt missing activation guidance: %q", prompt)
	}
}

func TestCatalogPrompt_EmptyCatalogReturnsEmpty(t *testing.T) {
	if got := skills.CatalogPrompt(skills.Catalog{}); got != "" {
		t.Errorf("expected empty string for empty catalog, got %q", got)
	}
}

func TestActivationContent_WrapsBodyWithTags(t *testing.T) {
	skill := skills.Skill{
		Name:      "pdf-processing",
		Directory: "/tmp/skills/pdf-processing",
		Resources: []string{"scripts/extract.py"},
	}
	out := skills.ActivationContent(skill, "# PDF Processing\nUse pdfplumber.")
	if !strings.Contains(out, `<skill_content name="pdf-processing">`) {
		t.Errorf("missing opening tag: %q", out)
	}
	if !strings.Contains(out, "</skill_content>") {
		t.Errorf("missing closing tag: %q", out)
	}
	if !strings.Contains(out, "scripts/extract.py") {
		t.Errorf("missing resource listing: %q", out)
	}
}

func TestSearchRoots_IncludesAllStandardLocations(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cwd := t.TempDir()
	roots := skills.SearchRoots(cwd, skills.DefaultLookupOptions())
	if len(roots) < 8 {
		t.Errorf("expected >= 8 roots, got %d", len(roots))
	}
	expectations := map[string]bool{
		filepath.Join(cwd, ".spettro", "skills"):  false,
		filepath.Join(cwd, ".agents", "skills"):   false,
		filepath.Join(cwd, ".claude", "skills"):   false,
		filepath.Join(cwd, ".openai", "skills"):   false,
		filepath.Join(home, ".spettro", "skills"): false,
		filepath.Join(home, ".agents", "skills"):  false,
		filepath.Join(home, ".claude", "skills"):  false,
		filepath.Join(home, ".openai", "skills"):  false,
	}
	for _, r := range roots {
		if _, ok := expectations[r.Path]; ok {
			expectations[r.Path] = true
		}
	}
	for path, found := range expectations {
		if !found {
			t.Errorf("expected discovery root %s in SearchRoots()", path)
		}
	}
}

func TestSearchRoots_ProjectRootsBeforeUserRoots(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cwd := t.TempDir()
	roots := skills.SearchRoots(cwd, skills.DefaultLookupOptions())
	var firstUser, lastProject int = -1, -1
	for i, r := range roots {
		if r.Scope == skills.ScopeProject {
			lastProject = i
		}
		if r.Scope == skills.ScopeUser && firstUser == -1 {
			firstUser = i
		}
	}
	if firstUser == -1 || lastProject == -1 {
		t.Fatalf("expected both project and user roots, got %v", roots)
	}
	if lastProject >= firstUser {
		t.Errorf("project roots should come before user roots; lastProject=%d firstUser=%d", lastProject, firstUser)
	}
}

func TestSkillCatalog_FindIsCaseInsensitive(t *testing.T) {
	cat := skills.Catalog{
		Skills: []skills.Skill{
			{Name: "code-review"},
			{Name: "data-analysis"},
		},
	}
	if _, ok := cat.Find("CODE-REVIEW"); !ok {
		t.Errorf("expected case-insensitive match")
	}
	if _, ok := cat.Find("does-not-exist"); ok {
		t.Errorf("did not expect match for missing skill")
	}
}
