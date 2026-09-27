package skills

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

func TestRenderBodyArguments(t *testing.T) {
	s := Skill{Name: "m", Directory: "/skills/m", Arguments: []string{"component", "from-lang"}}
	cases := []struct {
		name, body, args, want string
	}{
		{"whole string", "Fix $ARGUMENTS now", "issue 42", "Fix issue 42 now"},
		{"indexed", "$ARGUMENTS[1] then $0", "a b", "b then a"},
		{"quoted argument", "[$0]", `"two words" x`, "[two words]"},
		{"missing index kept", "$0 and $2", "a", "a and $2"},
		{"named", "Move $component from $from-lang", "Button js", "Move Button from js"},
		{"named missing is empty", "[$from-lang]", "Button", "[]"},
		{"undeclared word kept", "echo $HOME $component", "Button", "echo $HOME Button"},
		{"skill dir", "run ${CLAUDE_SKILL_DIR}/x.sh and ${SKILL_DIR}", "", "run /skills/m/x.sh and /skills/m"},
		{"unused args appended", "Just do it", "please", "Just do it\n\nARGUMENTS: please"},
		{"no args no placeholder", "Plain $1", "", "Plain $1"},
		{"no args empties $ARGUMENTS", "[$ARGUMENTS]", "", "[]"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := RenderBody(s, tc.body, tc.args); got != tc.want {
				t.Errorf("RenderBody(%q, %q) = %q, want %q", tc.body, tc.args, got, tc.want)
			}
		})
	}
}

func TestCatalogPromptListsModelSkillsOnly(t *testing.T) {
	cat := Catalog{Skills: []Skill{
		{Name: "alpha", Description: "First\nskill <x>", WhenToUse: "when testing"},
		{Name: "hidden", Description: "user only", ModelInvocationDisabled: true},
		{Name: "gone", Description: "disabled", Disabled: true},
	}}
	got := CatalogPrompt(cat, ToolName)
	if !strings.Contains(got, "- alpha: First skill &lt;x&gt; when testing\n") {
		t.Errorf("alpha line missing or not one line:\n%s", got)
	}
	if strings.Contains(got, "hidden") || strings.Contains(got, "gone") {
		t.Errorf("user-only and disabled skills must not be listed:\n%s", got)
	}
	if !strings.Contains(got, "`"+ToolName+"`") {
		t.Error("the prompt must name the skill tool in backticks so it is advertised")
	}
	if got != CatalogPrompt(cat, ToolName) {
		t.Error("the prompt must be deterministic")
	}
	if CatalogPrompt(Catalog{Skills: []Skill{{Name: "x", Disabled: true}}}, ToolName) != "" {
		t.Error("no model-invocable skills means no section")
	}
	if CatalogPrompt(cat, "") != "" {
		t.Error("an agent with no tool to load skills with gets no section")
	}
	if got := CatalogPrompt(cat, "skill-read"); !strings.Contains(got, "call the `skill-read` tool") {
		t.Errorf("the section must name the agent's load tool:\n%s", got)
	}
}

func TestCatalogPromptBudget(t *testing.T) {
	var cat Catalog
	for i := range 200 {
		cat.Skills = append(cat.Skills, Skill{Name: fmt.Sprintf("skill-%03d", i), Description: strings.Repeat("d", 400)})
	}
	got := CatalogPrompt(cat, ToolName)
	if len(got) > maxListingChars+1000 {
		t.Errorf("prompt is %d chars, budget is %d", len(got), maxListingChars)
	}
	if !strings.Contains(got, "more skills not listed") {
		t.Error("an over-budget list must say how many skills it left out")
	}
}

func TestUserInvocationPromptAndActivate(t *testing.T) {
	isolateHome(t)
	root := t.TempDir()
	dir := writeRaw(t, root, "greet", "---\nname: greet\ndescription: Greets\n---\nSay hello to $ARGUMENTS.\n")
	writeRaw(t, dir, "scripts", "x") // makes scripts/SKILL.md a bundled file
	s, err := Read(dir)
	if err != nil {
		t.Fatal(err)
	}
	got, err := UserInvocationPrompt(s, "Ada")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`The user ran the "greet" skill (/greet Ada)`,
		`<skill_content name="greet">`,
		"Say hello to Ada.",
		"Skill directory: " + filepath.ToSlash(dir),
		"<file>scripts/SKILL.md</file>",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("invocation prompt missing %q:\n%s", want, got)
		}
	}
}

func TestExpandMentions(t *testing.T) {
	isolateHome(t)
	root := t.TempDir()
	mk := func(name, extra string) Skill {
		dir := writeRaw(t, root, name, "---\nname: "+name+"\ndescription: d\n"+extra+"---\nBody of "+name+"\n")
		s, err := Read(dir)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	cat := Catalog{Skills: []Skill{mk("deploy", ""), mk("secret", "user-invocable: false\n")}}

	got, names := ExpandMentions("please $deploy, not $HOME or $secret", cat)
	if len(names) != 1 || names[0] != "deploy" {
		t.Errorf("activated = %v, want [deploy]", names)
	}
	if !strings.HasPrefix(got, "please $deploy, not $HOME or $secret") || !strings.Contains(got, "Body of deploy") {
		t.Errorf("expanded prompt:\n%s", got)
	}
	if strings.Contains(got, "Body of secret") {
		t.Error("a skill that is not user-invocable must not be activated by a mention")
	}

	plain := "cost is $5 and a$deploy"
	if got, names := ExpandMentions(plain, cat); got != plain || names != nil {
		t.Errorf("no real mention must leave the prompt alone, got %q %v", got, names)
	}
}
