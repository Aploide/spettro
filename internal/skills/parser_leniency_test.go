package skills

import (
	"path/filepath"
	"testing"
)

// argument-hint is written the way Claude Code documents it, with bare
// brackets. It is display text, never a list: the brackets and separators
// must survive as typed.
func TestArgumentHintKeepsBrackets(t *testing.T) {
	for _, hint := range []string{"[issue-number]", "[add|remove|list] [tagId]", "[file, line]"} {
		s, err := parse("---\nname: x\ndescription: d\nargument-hint: "+hint+"\n---\nbody\n", "x")
		if err != nil {
			t.Fatal(err)
		}
		if s.ArgumentHint != hint {
			t.Errorf("argument-hint %q parsed as %q", hint, s.ArgumentHint)
		}
	}
	// Other keys still read an inline list as one.
	s, err := parse("---\nname: x\ndescription: d\nallowed-tools: [Read, Grep]\n---\nbody\n", "x")
	if err != nil {
		t.Fatal(err)
	}
	if s.AllowedTools != "Read Grep" {
		t.Errorf("allowed-tools = %q", s.AllowedTools)
	}
}

// An empty frontmatter block is frontmatter, not body text, and a quoted
// value may carry a trailing comment.
func TestParseFrontmatterEdgeForms(t *testing.T) {
	s, err := parse("---\n---\n# Title\nbody\n", "dir")
	if err != nil {
		t.Fatal(err)
	}
	if s.Description != "Title" {
		t.Errorf("empty frontmatter: description = %q, want the body's first line", s.Description)
	}
	s, err = parse("---\ndescription: \"quoted\" # comment\n---\nbody\n", "dir")
	if err != nil {
		t.Fatal(err)
	}
	if s.Description != "quoted" {
		t.Errorf("quoted value with a comment = %q", s.Description)
	}
	s, err = parse("---\ndescription: \"say \\\"hi\\\" now\"\n---\nbody\n", "dir")
	if err != nil {
		t.Fatal(err)
	}
	if s.Description != `say \"hi\" now` {
		t.Errorf("fully quoted value = %q", s.Description)
	}
}

// Hidden folders are not searched, whether or not they hold a SKILL.md.
func TestDiscoverSkipsHiddenSkillFolders(t *testing.T) {
	isolateHome(t)
	repo := t.TempDir()
	root := filepath.Join(repo, ".spettro", "skills")
	writeSkill(t, root, ".draft", ".draft", "a draft")
	writeSkill(t, filepath.Join(repo, ".agents", "skills", "group"), ".hidden", ".hidden", "hidden")
	writeSkill(t, root, "real", "real", "a real one")
	cat, err := Discover(repo, DefaultLookupOptions())
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{".draft", ".hidden"} {
		if _, ok := cat.Find(name); ok {
			t.Errorf("hidden folder %s was loaded as a skill", name)
		}
	}
	if _, ok := cat.Find("real"); !ok {
		t.Error("the visible skill was not loaded")
	}
}
