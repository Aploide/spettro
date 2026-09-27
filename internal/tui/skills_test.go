package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"spettro/internal/agent"
	"spettro/internal/commands"
	"spettro/internal/skills"
)

// newSkillModel returns a test model whose workspace holds these skills in
// .spettro/skills, with HOME isolated and the skill cache cleared:
//
//   - greet: a normal skill with an argument hint;
//   - help: collides with the built-in /help;
//   - review: collides with a custom command;
//   - skill-creator: starts with "skill", like the /skill command;
//   - background: user-invocable: false (model only).
func newSkillModel(t *testing.T) Model {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CODEX_HOME", "")
	cwd := t.TempDir()
	root := filepath.Join(cwd, ".spettro", "skills")
	write := func(name, front, body string) {
		dir := filepath.Join(root, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		content := "---\nname: " + name + "\n" + front + "---\n" + body
		if err := os.WriteFile(filepath.Join(dir, skills.SkillFilename), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("greet", "description: Greets a person\nargument-hint: <name>\n", "Say hello to $ARGUMENTS.\n")
	write("help", "description: Skill that collides\n", "never runs\n")
	write("review", "description: Review skill\n", "never runs either\n")
	write("skill-creator", "description: Creates skills\n", "Create a skill.\n")
	write("background", "description: Model only\nuser-invocable: false\n", "context\n")
	agent.ReloadSkills()
	t.Cleanup(agent.ReloadSkills)

	m := NewModelForTesting()
	m.cwd = cwd
	m.customCommands = []commands.Command{{Name: "review", Prompt: "custom review", Scope: "project"}}
	return m
}

func TestSlashMenuListsUserSkills(t *testing.T) {
	m := newSkillModel(t)
	entries := map[string]string{}
	count := map[string]int{}
	for _, c := range m.filterCommands("") {
		entries[c.name] = c.desc
		count[c.name]++
	}
	if desc := entries["/greet"]; !strings.Contains(desc, "<name>") || !strings.Contains(desc, "Greets a person") {
		t.Errorf("/greet entry = %q, want the argument hint and description", desc)
	}
	if count["/help"] != 1 || strings.Contains(entries["/help"], "collides") {
		t.Errorf("a skill must not shadow the built-in /help: %q", entries["/help"])
	}
	if count["/review"] != 1 || entries["/review"] == "skill: Review skill" {
		t.Errorf("a skill must not shadow a custom command: %q", entries["/review"])
	}
	if _, ok := entries["/background"]; ok {
		t.Error("a user-invocable: false skill must stay out of the menu")
	}
}

// Typing /skill-creator must reach the main menu (where the skill is), not
// the /skill sub-command menu.
func TestSkillPrefixedSkillNameUsesMainMenu(t *testing.T) {
	m := newSkillModel(t)
	m.ta.SetValue("/skill-creator")
	m.syncInputSuggestions()
	found := false
	for _, c := range m.cmdItems {
		if c.name == "/skill-creator" {
			found = true
		}
	}
	if !found {
		t.Errorf("menu for /skill-creator = %+v", m.cmdItems)
	}
	m.ta.SetValue("/skill ins")
	m.syncInputSuggestions()
	if len(m.cmdItems) == 0 || m.cmdItems[0].name != "/skill install" {
		t.Errorf("/skill sub-menu = %+v", m.cmdItems)
	}
}

// The /permission, /thinking and /think sub-menus open only once the
// command is followed by a space, like /skill: a skill whose name merely
// starts with one of them (/thinker, /permissions-audit) must be reachable
// from the main menu. With a reasoning model, /thinker used to open the
// (empty) /think sub-menu.
func TestSubMenusNeedTheirCommandAndASpace(t *testing.T) {
	m := newSkillModel(t)
	m.cfg.ActiveProvider = "anthropic"
	m.cfg.ActiveModel = "claude-sonnet-4-5"
	if !m.activeModelSupportsReasoning() {
		t.Fatal("test model must support reasoning, or /think's sub-menu never opens")
	}
	for _, name := range []string{"thinker", "thinking-partner", "permissions-audit"} {
		dir := filepath.Join(m.cwd, ".spettro", "skills", name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		content := "---\nname: " + name + "\ndescription: d\n---\nbody\n"
		if err := os.WriteFile(filepath.Join(dir, skills.SkillFilename), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	agent.ReloadSkills()
	for _, name := range []string{"thinker", "thinking-partner", "permissions-audit"} {
		m.ta.SetValue("/" + name)
		m.syncInputSuggestions()
		found := false
		for _, c := range m.cmdItems {
			found = found || c.name == "/"+name
		}
		if !found {
			t.Errorf("/%s: menu = %+v, want the skill", name, m.cmdItems)
		}
	}
	// With the space, each command still opens its own sub-menu.
	for input, want := range map[string]string{"/think h": "/think high", "/thinking h": "/thinking high", "/permission y": "/permission yolo"} {
		m.ta.SetValue(input)
		m.syncInputSuggestions()
		cmd, _, _ := strings.Cut(input, " ")
		found := false
		for _, c := range m.cmdItems {
			if !strings.HasPrefix(c.name, cmd+" ") {
				t.Errorf("%q: %s is not an entry of the %s sub-menu", input, c.name, cmd)
			}
			found = found || c.name == want
		}
		if !found {
			t.Errorf("%q: sub-menu = %+v, want it to offer %s", input, m.cmdItems, want)
		}
	}
}

// /greet Ada shows what the user typed and sends the skill's instructions.
func TestSlashSkillInvocation(t *testing.T) {
	m := newSkillModel(t)
	m.thinking = true // queue the prompt instead of starting a real run
	nm, _ := m.handleCommand("/greet Ada")
	got := nm.(Model)
	if len(got.pendingPrompts) != 1 {
		t.Fatalf("expected one queued prompt, got %d (banner %q)", len(got.pendingPrompts), got.BannerForTesting())
	}
	q := got.pendingPrompts[0]
	if q.Input != "/greet Ada" {
		t.Errorf("transcript input = %q", q.Input)
	}
	if !strings.Contains(q.Prompt, `<skill_content name="greet">`) || !strings.Contains(q.Prompt, "Say hello to Ada.") {
		t.Errorf("model prompt:\n%s", q.Prompt)
	}
}

func TestSlashBuiltinsAndCustomWinOverSkills(t *testing.T) {
	m := newSkillModel(t)
	m.thinking = true
	nm, _ := m.handleCommand("/review x")
	got := nm.(Model)
	if len(got.pendingPrompts) != 1 || got.pendingPrompts[0].Prompt != "custom review" {
		t.Errorf("custom command must win: %+v", got.pendingPrompts)
	}
	nm, _ = m.handleCommand("/background")
	if !strings.Contains(nm.(Model).BannerForTesting(), "unknown command") {
		t.Error("a model-only skill must not run as /name")
	}
}

func TestDollarMentionCompletesAndExpands(t *testing.T) {
	m := newSkillModel(t)
	m.ta.SetValue("please $gr")
	m.syncInputSuggestions()
	if m.mentionKind != mentionSkill || len(m.mentionItems) != 1 || m.mentionItems[0] != "greet" {
		t.Fatalf("mention items = %v kind %v", m.mentionItems, m.mentionKind)
	}
	m = m.acceptMention()
	if got := m.ta.Value(); got != "please $greet " {
		t.Errorf("accepted mention = %q", got)
	}
	m.ta.SetValue("cost is $5")
	m.syncInputSuggestions()
	if len(m.mentionItems) != 0 {
		t.Errorf("$5 matches no skill, palette must stay closed: %v", m.mentionItems)
	}

	m.thinking = true
	nm, _ := m.handlePrompt("use $greet now")
	got := nm.(Model)
	if len(got.pendingPrompts) != 1 || !strings.Contains(got.pendingPrompts[0].Prompt, "Say hello to .") {
		t.Errorf("mentioned skill not expanded: %+v", got.pendingPrompts)
	}
}

func TestSkillsListShowsSourceAndLocation(t *testing.T) {
	m := newSkillModel(t)
	nm, _ := m.handleCommand("/skills")
	msgs := nm.(Model).MessagesForTesting()
	if len(msgs) == 0 {
		t.Fatal("no /skills output")
	}
	out := msgs[len(msgs)-1].Content
	for _, want := range []string{"greet  [/greet]", "spettro/project", filepath.Join(".spettro", "skills", "greet", "SKILL.md"), "background  [agent only]"} {
		if !strings.Contains(out, want) {
			t.Errorf("/skills output missing %q:\n%s", want, out)
		}
	}
}

func TestSkillDisableEnableUsesConfig(t *testing.T) {
	m := newSkillModel(t)
	nm, _ := m.handleCommand("/skill disable greet")
	m = nm.(Model)
	if len(m.cfg.DisabledSkills) != 1 || m.cfg.DisabledSkills[0] != "greet" {
		t.Fatalf("disabled_skills = %v", m.cfg.DisabledSkills)
	}
	if _, ok := m.findUserSkill("/greet"); ok {
		t.Error("a disabled skill must not run")
	}
	if _, err := os.Stat(filepath.Join(m.cwd, ".spettro", "skills", "greet", skills.DisabledMarker)); err == nil {
		t.Error("disable must not write into the skill folder")
	}
	nm, _ = m.handleCommand("/skill enable GREET")
	m = nm.(Model)
	if len(m.cfg.DisabledSkills) != 0 {
		t.Errorf("enable must clear the entry case-insensitively: %v", m.cfg.DisabledSkills)
	}
}

// The mention palette keeps every row on one line of its box, so the
// height state_render.go reserves (one row per item) is exact.
func TestMentionPaletteRowsFit(t *testing.T) {
	m := newSkillModel(t)
	m.mentionKind = mentionSkill
	m.mentionItems = []string{"greet", "skill-creator"}
	for _, width := range []int{30, 50, 90} {
		view := m.viewMentionPalette(width)
		for _, line := range strings.Split(view, "\n") {
			if w := ansi.StringWidth(line); w > width {
				t.Errorf("width %d: palette line is %d cells: %q", width, w, line)
			}
		}
		if got, want := strings.Count(view, "\n")+1, 6+len(m.mentionItems); got != want {
			t.Errorf("width %d: palette is %d lines, want %d", width, got, want)
		}
	}
}

// With the mention palette open the whole frame still fits the terminal:
// recalcLayout must reserve exactly the rows the palette renders.
func TestMentionPaletteFitsTheFrame(t *testing.T) {
	for _, kind := range []mentionKind{mentionFile, mentionSkill} {
		for _, height := range []int{24, 30, 40} {
			m := footerModel(100, height)
			m.cwd = newSkillModel(t).cwd
			m.mentionKind = kind
			m.mentionItems = []string{"greet", "skill-creator", "a/very/long/path/that/goes/on/and/on/and/on/and/on/and/on/file.go"}
			m = m.recalcLayout()
			frame := m.View().Content
			if got := strings.Count(frame, "\n") + 1; got > height {
				t.Errorf("kind %v height %d: frame is %d lines", kind, height, got)
			}
		}
	}
}

// No slash-menu row may be wider than the dialog, whatever the name or
// description length.
func TestCmdMenuColumnsFit(t *testing.T) {
	for _, innerW := range []int{28, 40, 64} {
		for _, name := range []string{"/help", "/a-very-long-skill-name-that-goes-on-and-on"} {
			n, d := cmdMenuColumns(name, strings.Repeat("description ", 20), innerW)
			if w := ansi.StringWidth(n + "  " + d); w > innerW {
				t.Errorf("innerW %d name %q: row is %d cells", innerW, name, w)
			}
		}
	}
}
