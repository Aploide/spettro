package commands

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDiscoverTOMLAndMarkdown(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "review.toml"), "prompt = \"review {{args}}\"\ndescription = \"quick review\"\n")
	writeFile(t, filepath.Join(dir, "git", "pr.md"), "---\ndescription: open a PR\n---\nOpen a pull request for {{args}}.\n")
	writeFile(t, filepath.Join(dir, "notes.txt"), "ignored")

	cmds, issues := DiscoverRoots([]Root{{Path: dir, Scope: "project"}})
	if len(issues) != 0 {
		t.Fatalf("unexpected issues: %v", issues)
	}
	if len(cmds) != 2 {
		t.Fatalf("expected 2 commands, got %d: %+v", len(cmds), cmds)
	}
	if cmds[0].Name != "git:pr" || cmds[0].Description != "open a PR" || cmds[0].Prompt != "Open a pull request for {{args}}." {
		t.Fatalf("bad namespaced md command: %+v", cmds[0])
	}
	if cmds[1].Name != "review" || cmds[1].Description != "quick review" || cmds[1].Prompt != "review {{args}}" {
		t.Fatalf("bad toml command: %+v", cmds[1])
	}
}

func TestDiscoverProjectOverridesUser(t *testing.T) {
	user := t.TempDir()
	proj := t.TempDir()
	writeFile(t, filepath.Join(user, "deploy.toml"), "prompt = \"user version\"\n")
	writeFile(t, filepath.Join(user, "audit.toml"), "prompt = \"audit it\"\n")
	writeFile(t, filepath.Join(proj, "deploy.toml"), "prompt = \"project version\"\n")

	cmds, _ := DiscoverRoots([]Root{{Path: user, Scope: "user"}, {Path: proj, Scope: "project"}})
	if len(cmds) != 2 {
		t.Fatalf("expected 2 commands, got %d", len(cmds))
	}
	for _, c := range cmds {
		if c.Name == "deploy" {
			if c.Prompt != "project version" || c.Scope != "project" {
				t.Fatalf("project should override user: %+v", c)
			}
		}
	}
}

func TestDiscoverReportsParseIssues(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "broken.toml"), "prompt = \n")
	writeFile(t, filepath.Join(dir, "noprompt.toml"), "description = \"nothing\"\n")

	cmds, issues := DiscoverRoots([]Root{{Path: dir, Scope: "project"}})
	if len(cmds) != 0 {
		t.Fatalf("expected no commands, got %+v", cmds)
	}
	if len(issues) != 2 {
		t.Fatalf("expected 2 issues, got %v", issues)
	}
}

func TestDiscoverMissingRootIsFine(t *testing.T) {
	cmds, issues := DiscoverRoots([]Root{{Path: filepath.Join(t.TempDir(), "nope"), Scope: "user"}})
	if len(cmds) != 0 || len(issues) != 0 {
		t.Fatalf("missing root should be silent: %v %v", cmds, issues)
	}
}

func TestExpandArgs(t *testing.T) {
	c := Command{Name: "review", Prompt: "review {{args}} carefully; args again: {{args}}"}
	got, err := Expand(c, "main.go", "", false)
	if err != nil {
		t.Fatal(err)
	}
	want := "review main.go carefully; args again: main.go"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestExpandShellGated(t *testing.T) {
	c := Command{Name: "st", Prompt: "status:\n!`echo hello`"}
	if _, err := Expand(c, "", "", false); err == nil {
		t.Fatal("expected error when shell interpolation is not allowed")
	}
	got, err := Expand(c, "", "", true)
	if err != nil {
		t.Fatal(err)
	}
	if got != "status:\nhello" {
		t.Fatalf("got %q", got)
	}
}

func TestExpandShellFailure(t *testing.T) {
	c := Command{Name: "bad", Prompt: "!`exit 3`"}
	if _, err := Expand(c, "", "", true); err == nil || !strings.Contains(err.Error(), "failed") {
		t.Fatalf("expected failure error, got %v", err)
	}
}
