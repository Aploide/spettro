package skills

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseValidFrontmatter(t *testing.T) {
	s, err := parse(`---
name: my-skill
description: "Does a thing"
license: MIT
allowed-tools: shell-exec
metadata:
  author: carlo
  tag: test
---
# Body

Instructions here.
`)
	if err != nil {
		t.Fatal(err)
	}
	if s.Name != "my-skill" || s.Description != "Does a thing" || s.License != "MIT" {
		t.Errorf("parsed skill wrong: %+v", s)
	}
	if s.AllowedTools != "shell-exec" {
		t.Errorf("allowed tools = %q", s.AllowedTools)
	}
	if s.Metadata["author"] != "carlo" || s.Metadata["tag"] != "test" {
		t.Errorf("metadata = %+v", s.Metadata)
	}
	if len(s.Issues) != 0 {
		t.Errorf("unexpected issues: %v", s.Issues)
	}
}

func TestParseBlockScalarAndDisabled(t *testing.T) {
	s, err := parse(`---
name: block-skill
description: |
  line one
  line two
disabled: true
---
body`)
	if err != nil {
		t.Fatal(err)
	}
	if s.Description != "line one\nline two" {
		t.Errorf("block scalar description = %q", s.Description)
	}
	if !s.Disabled {
		t.Error("disabled: true not honored")
	}

	s, err = parse("---\nname: en\ndescription: d\nenabled: false\n---\n")
	if err != nil {
		t.Fatal(err)
	}
	if !s.Disabled {
		t.Error("enabled: false must disable the skill")
	}
}

func TestParseErrorsAndIssues(t *testing.T) {
	if _, err := parse("no frontmatter at all"); err == nil {
		t.Error("missing frontmatter must error")
	}
	if _, err := parse("---\ndescription: d\n---\n"); err == nil {
		t.Error("missing name must error")
	}
	if _, err := parse("---\nname: x\n---\n"); err == nil {
		t.Error("missing description must error")
	}
	s, err := parse("---\nname: Bad_Name\ndescription: d\n---\n")
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Issues) == 0 {
		t.Error("invalid name must produce a validation issue")
	}
}

func TestSplitFrontmatter(t *testing.T) {
	front, body := splitFrontmatter("---\na: b\n---\nBody text")
	if front != "a: b" || body != "Body text" {
		t.Errorf("split = (%q, %q)", front, body)
	}
	front, body = splitFrontmatter("plain content")
	if front != "" || body != "plain content" {
		t.Errorf("no-frontmatter split = (%q, %q)", front, body)
	}
	// CRLF normalization
	front, _ = splitFrontmatter("---\r\na: b\r\n---\r\nBody")
	if front != "a: b" {
		t.Errorf("crlf front = %q", front)
	}
}

func writeSkill(t *testing.T, root, dirName, name, desc string) {
	t.Helper()
	dir := filepath.Join(root, dirName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	content := "---\nname: " + name + "\ndescription: " + desc + "\n---\nBody of " + name + "\n"
	if err := os.WriteFile(filepath.Join(dir, SkillFilename), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDiscoverPrecedenceAndShadowing(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cwd := t.TempDir()

	writeSkill(t, filepath.Join(cwd, ".spettro", "skills"), "deploy", "deploy", "project version")
	writeSkill(t, filepath.Join(home, ".spettro", "skills"), "deploy", "deploy", "user version")
	writeSkill(t, filepath.Join(home, ".claude", "skills"), "review", "review", "user review")

	cat, err := Discover(cwd, DefaultLookupOptions())
	if err != nil {
		t.Fatal(err)
	}
	if len(cat.Skills) != 2 {
		t.Fatalf("got %d skills: %+v", len(cat.Skills), cat.Skills)
	}
	dep, ok := cat.Find("deploy")
	if !ok || dep.Scope != ScopeProject || dep.Description != "project version" {
		t.Errorf("project skill must win: %+v", dep)
	}
	if len(cat.Shadowed) != 1 || cat.Shadowed[0].Description != "user version" {
		t.Errorf("shadowed = %+v", cat.Shadowed)
	}
	rev, ok := cat.Find("REVIEW") // case-insensitive lookup
	if !ok || rev.Source != SourceClaude || rev.Scope != ScopeUser {
		t.Errorf("claude-dir skill wrong: %+v", rev)
	}
	if _, ok := cat.Find("missing"); ok {
		t.Error("Find must miss unknown names")
	}

	body, err := LoadBody(dep)
	if err != nil {
		t.Fatal(err)
	}
	if body != "Body of deploy" {
		t.Errorf("body = %q", body)
	}
}

func TestActiveFiltersDisabled(t *testing.T) {
	cat := Catalog{Skills: []Skill{
		{Name: "on"},
		{Name: "off", Disabled: true},
	}}
	act := cat.Active()
	if len(act) != 1 || act[0].Name != "on" {
		t.Errorf("Active = %+v", act)
	}
}

func TestReadReportsNameDirMismatch(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "folder-name", "other-name", "d")
	s, err := Read(filepath.Join(root, "folder-name"))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, iss := range s.Issues {
		if strings.Contains(iss, "does not match parent directory") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected name/dir mismatch issue, got %v", s.Issues)
	}
}
