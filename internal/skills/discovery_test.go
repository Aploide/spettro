package skills

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// writeRaw writes a SKILL.md with exactly the given content into root/dir.
func writeRaw(t *testing.T, root, dir, content string) string {
	t.Helper()
	path := filepath.Join(root, dir)
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, SkillFilename), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// isolateHome points HOME (and CODEX_HOME) at fresh temp directories so the
// developer's real skills never leak into a test.
func isolateHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("CODEX_HOME", "")
	return home
}

func TestParseToleratesMissingFrontmatter(t *testing.T) {
	s, err := parse("# Deploy the app\n\nRun make deploy.\n", "deploy")
	if err != nil {
		t.Fatal(err)
	}
	if s.Name != "deploy" {
		t.Errorf("name = %q, want the directory name", s.Name)
	}
	if s.Description != "Deploy the app" {
		t.Errorf("description = %q, want the first body line without the heading marker", s.Description)
	}
	if len(s.Issues) == 0 {
		t.Error("a defaulted description must be reported as an issue")
	}
}

func TestParseClaudeCodeFields(t *testing.T) {
	s, err := parse(`---
description: Migrate a component
when_to_use: when porting UI code
argument-hint: "[component] [from] [to]"
arguments: [component, from-lang, to-lang]
disable-model-invocation: true
user-invocable: false
allowed-tools:
  - Read
  - "Bash(git:*)"
model: inherit
---
Body`, "migrate-component")
	if err != nil {
		t.Fatal(err)
	}
	if s.Name != "migrate-component" {
		t.Errorf("name = %q", s.Name)
	}
	if s.WhenToUse != "when porting UI code" || s.ArgumentHint != "[component] [from] [to]" {
		t.Errorf("when_to_use/argument-hint = %q / %q", s.WhenToUse, s.ArgumentHint)
	}
	if !slices.Equal(s.Arguments, []string{"component", "from-lang", "to-lang"}) {
		t.Errorf("arguments = %v", s.Arguments)
	}
	if !s.ModelInvocationDisabled || !s.UserInvocationDisabled {
		t.Errorf("invocation flags = model %v user %v", s.ModelInvocationDisabled, s.UserInvocationDisabled)
	}
	if s.AllowedTools != "Read Bash(git:*)" {
		t.Errorf("allowed-tools list = %q", s.AllowedTools)
	}
	if s.Metadata["model"] != "inherit" {
		t.Errorf("unknown keys must land in metadata: %v", s.Metadata)
	}
	if got := s.ListingDescription(); got != "Migrate a component when porting UI code" {
		t.Errorf("listing description = %q", got)
	}
	if s.ModelInvocable() || s.UserInvocable() {
		t.Error("both invocation paths are switched off")
	}
}

func TestParseUserInvocableDefaultsTrue(t *testing.T) {
	s, err := parse("---\nname: a\ndescription: d\nuser-invocable: true\n---\n", "a")
	if err != nil {
		t.Fatal(err)
	}
	if s.UserInvocationDisabled {
		t.Error("user-invocable: true must keep the skill invocable")
	}
}

// Precedence: project before user, nearest project directory first, and
// within one directory Spettro before the compat families.
func TestDiscoverPrecedenceAcrossFamiliesAndParents(t *testing.T) {
	home := isolateHome(t)
	repo := t.TempDir()
	if err := os.Mkdir(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	pkg := filepath.Join(repo, "apps", "web")
	if err := os.MkdirAll(pkg, 0o755); err != nil {
		t.Fatal(err)
	}

	writeSkill(t, filepath.Join(pkg, ".claude", "skills"), "lint", "lint", "package claude")
	writeSkill(t, filepath.Join(pkg, ".spettro", "skills"), "lint", "lint", "package spettro")
	writeSkill(t, filepath.Join(repo, ".agents", "skills"), "lint", "lint", "repo agents")
	writeSkill(t, filepath.Join(repo, ".agents", "skills"), "release", "release", "repo agents release")
	writeSkill(t, filepath.Join(home, ".spettro", "skills"), "release", "release", "user release")
	writeSkill(t, filepath.Join(home, ".codex", "skills"), "codex-only", "codex-only", "user codex")

	cat, err := Discover(pkg, DefaultLookupOptions())
	if err != nil {
		t.Fatal(err)
	}
	lint, _ := cat.Find("lint")
	if lint.Description != "package spettro" || lint.Source != SourceSpettro {
		t.Errorf("lint = %+v, want the package's .spettro copy", lint)
	}
	release, _ := cat.Find("release")
	if release.Description != "repo agents release" || release.Scope != ScopeProject {
		t.Errorf("release = %+v, want the repository root's copy over the user one", release)
	}
	codex, ok := cat.Find("codex-only")
	if !ok || codex.Source != SourceCodex || codex.Scope != ScopeUser {
		t.Errorf("codex user skill = %+v (found %v)", codex, ok)
	}
	if len(cat.Shadowed) != 3 {
		t.Errorf("shadowed = %d, want 3 (two lint, one release)", len(cat.Shadowed))
	}
}

func TestDiscoverCompatOff(t *testing.T) {
	home := isolateHome(t)
	cwd := t.TempDir()
	writeSkill(t, filepath.Join(cwd, ".claude", "skills"), "a", "a", "claude")
	writeSkill(t, filepath.Join(home, ".agents", "skills"), "b", "b", "agents")
	writeSkill(t, filepath.Join(home, ".spettro", "skills"), "c", "c", "spettro")

	opts := DefaultLookupOptions()
	opts.IncludeCompat = false
	cat, _ := Discover(cwd, opts)
	if len(cat.Skills) != 1 || cat.Skills[0].Name != "c" {
		t.Errorf("compat off must only see .spettro skills, got %+v", cat.Skills)
	}
	for _, r := range SearchRoots(cwd, opts) {
		if r.ReadOnly() {
			t.Errorf("compat off must not list %s", r.Path)
		}
	}
}

func TestSearchRootsCodexHome(t *testing.T) {
	isolateHome(t)
	codex := t.TempDir()
	t.Setenv("CODEX_HOME", codex)
	want := filepath.Join(codex, "skills")
	for _, r := range SearchRoots("", DefaultLookupOptions()) {
		if r.Source == SourceCodex {
			if r.Path != want {
				t.Errorf("codex user root = %s, want %s", r.Path, want)
			}
			return
		}
	}
	t.Error("no codex root listed")
}

// Outside a repository only the working directory is project scope: a
// skills folder in a parent must not leak in.
func TestProjectDirsWithoutRepository(t *testing.T) {
	isolateHome(t)
	parent := t.TempDir()
	cwd := filepath.Join(parent, "child")
	if err := os.Mkdir(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	writeSkill(t, filepath.Join(parent, ".spettro", "skills"), "p", "p", "parent")
	cat, _ := Discover(cwd, DefaultLookupOptions())
	if _, ok := cat.Find("p"); ok {
		t.Error("a parent outside any repository must not be scanned")
	}
}

func TestSearchRootsDeduplicatesHomeAsCWD(t *testing.T) {
	home := isolateHome(t)
	writeSkill(t, filepath.Join(home, ".spettro", "skills"), "x", "x", "d")
	cat, _ := Discover(home, DefaultLookupOptions())
	if len(cat.Skills) != 1 || len(cat.Shadowed) != 0 {
		t.Errorf("a root reachable twice must be scanned once: %+v / %+v", cat.Skills, cat.Shadowed)
	}
}

func TestDiscoverSkipsFoldersWithoutManifest(t *testing.T) {
	isolateHome(t)
	cwd := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cwd, ".spettro", "skills", "shared-scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	cat, _ := Discover(cwd, DefaultLookupOptions())
	if len(cat.Skills) != 0 || len(cat.Issues) != 0 {
		t.Errorf("a folder without SKILL.md is not a skill and not an issue: %+v", cat)
	}
}

func TestLegacyDisabledMarkerAndConfigNames(t *testing.T) {
	isolateHome(t)
	cwd := t.TempDir()
	root := filepath.Join(cwd, ".spettro", "skills")
	writeSkill(t, root, "old", "old", "d")
	writeSkill(t, root, "cfg", "cfg", "d")
	writeSkill(t, root, "on", "on", "d")
	if err := os.WriteFile(filepath.Join(root, "old", DisabledMarker), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	cat, _ := Discover(cwd, DefaultLookupOptions())
	cat = cat.WithDisabledNames([]string{"CFG"})
	var names []string
	for _, s := range cat.Active() {
		names = append(names, s.Name)
	}
	if !slices.Equal(names, []string{"on"}) {
		t.Errorf("active = %v, want [on]", names)
	}
}

func TestCatalogInvocationFilters(t *testing.T) {
	cat := Catalog{Skills: []Skill{
		{Name: "both"},
		{Name: "user-only", ModelInvocationDisabled: true},
		{Name: "model-only", UserInvocationDisabled: true},
		{Name: "off", Disabled: true},
	}}
	names := func(list []Skill) []string {
		var out []string
		for _, s := range list {
			out = append(out, s.Name)
		}
		return out
	}
	if got := names(cat.ForModel()); !slices.Equal(got, []string{"both", "model-only"}) {
		t.Errorf("ForModel = %v", got)
	}
	if got := names(cat.ForUser()); !slices.Equal(got, []string{"both", "user-only"}) {
		t.Errorf("ForUser = %v", got)
	}
}

func TestCacheReusesAndInvalidates(t *testing.T) {
	isolateHome(t)
	cwd := t.TempDir()
	root := filepath.Join(cwd, ".spettro", "skills")
	writeSkill(t, root, "first", "first", "d")

	var c Cache
	if got := len(c.Get(cwd, DefaultLookupOptions()).Skills); got != 1 {
		t.Fatalf("first Get = %d skills", got)
	}
	writeSkill(t, root, "second", "second", "d")
	if got := len(c.Get(cwd, DefaultLookupOptions()).Skills); got != 1 {
		t.Errorf("cached Get must not rescan, got %d skills", got)
	}
	c.Invalidate()
	if got := len(c.Get(cwd, DefaultLookupOptions()).Skills); got != 2 {
		t.Errorf("Get after Invalidate = %d skills, want 2", got)
	}

	// The returned catalog is a copy: changing it leaves the cache intact.
	cat := c.Get(cwd, DefaultLookupOptions())
	cat.Skills[0].Disabled = true
	if c.Get(cwd, DefaultLookupOptions()).Skills[0].Disabled {
		t.Error("callers must not be able to modify the cached catalog")
	}
}

// The cache is keyed by the roots scanned, so a different home directory
// (or compat setting) is a different entry, not a stale hit.
func TestCacheKeyFollowsRoots(t *testing.T) {
	cwd := t.TempDir()
	var c Cache
	homeA := isolateHome(t)
	writeSkill(t, filepath.Join(homeA, ".spettro", "skills"), "a", "a", "d")
	if got := c.Get(cwd, DefaultLookupOptions()); len(got.Skills) != 1 || got.Skills[0].Name != "a" {
		t.Fatalf("home A: %+v", got.Skills)
	}
	homeB := isolateHome(t)
	writeSkill(t, filepath.Join(homeB, ".spettro", "skills"), "b", "b", "d")
	if got := c.Get(cwd, DefaultLookupOptions()); len(got.Skills) != 1 || got.Skills[0].Name != "b" {
		t.Errorf("home B must not reuse home A's catalog: %+v", got.Skills)
	}
}

func TestUninstallRejectsPathNames(t *testing.T) {
	isolateHome(t)
	for _, name := range []string{"../x", "a/b", ".."} {
		if err := Uninstall(name, ScopeUser, ""); err == nil || !strings.Contains(err.Error(), "invalid") {
			t.Errorf("Uninstall(%q) = %v, want an invalid-name error", name, err)
		}
	}
}
