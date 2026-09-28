package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"spettro/internal/config"
	"spettro/internal/provider"
	"spettro/internal/skills"
)

// writeSkillDir writes root/<name>/SKILL.md with the given frontmatter lines
// and body.
func writeSkillDir(t *testing.T, root, name, front, body string) {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	content := "---\nname: " + name + "\n" + front + "---\n" + body
	if err := os.WriteFile(filepath.Join(dir, skills.SkillFilename), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// skillToolRuntime returns a runtime whose catalog holds three skills in the
// project's .spettro/skills: a normal one, one only the user may run, and
// one disabled.
func skillToolRuntime(t *testing.T) *toolRuntime {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	r, dir := newAliasTestRuntime(t)
	root := filepath.Join(dir, ".spettro", "skills")
	writeSkillDir(t, root, "greet", "description: Greets someone\n", "Say hello to $ARGUMENTS.\n")
	writeSkillDir(t, root, "deploy", "description: Deploys\ndisable-model-invocation: true\n", "Deploy.\n")
	writeSkillDir(t, root, "off", "description: Off\ndisabled: true\n", "Off.\n")
	cat, err := skills.Discover(dir, skills.DefaultLookupOptions())
	if err != nil {
		t.Fatal(err)
	}
	r.skillsCatalog = cat
	return r
}

func runSkillCall(t *testing.T, r *toolRuntime, tool string, args any) (string, error) {
	t.Helper()
	return r.execute(context.Background(), aliasCall(t, tool, args), map[string]struct{}{"skill": {}})
}

func TestSkillToolLoadsBodyWithArgs(t *testing.T) {
	r := skillToolRuntime(t)
	out, err := runSkillCall(t, r, "skill", map[string]any{"name": "greet", "args": "Ada"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `<skill_content name="greet">`) || !strings.Contains(out, "Say hello to Ada.") {
		t.Errorf("activation output:\n%s", out)
	}
	if !strings.Contains(out, "Skill directory: ") {
		t.Error("the model must be told where the skill's bundled files are")
	}
}

// Every retired name reaches the same tool; skill-list lists.
func TestSkillToolAliases(t *testing.T) {
	r := skillToolRuntime(t)
	for _, tool := range []string{"skill-read", "activate-skill", "skill-activate"} {
		out, err := runSkillCall(t, r, tool, map[string]any{"name": "greet"})
		if err != nil || !strings.Contains(out, "Say hello to .") {
			t.Errorf("%s: out=%q err=%v", tool, out, err)
		}
	}
	// Claude Code's Skill tool spells the name "skill".
	if out, err := runSkillCall(t, r, "skill", map[string]any{"skill": "greet"}); err != nil || !strings.Contains(out, "greet") {
		t.Errorf("skill field: out=%q err=%v", out, err)
	}
	out, err := runSkillCall(t, r, "skill-list", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	var rows []skillListRow
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("listing is not JSON: %v\n%s", err, out)
	}
	if len(rows) != 1 || rows[0].Name != "greet" {
		t.Errorf("listing = %+v, want only the model-invocable skill", rows)
	}
}

func TestSkillToolRefusals(t *testing.T) {
	r := skillToolRuntime(t)
	cases := map[string]string{
		"deploy":  "can only be run by the user",
		"off":     "disabled",
		"missing": "available: greet",
	}
	for name, want := range cases {
		_, err := runSkillCall(t, r, "skill", map[string]any{"name": name})
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("skill %q: err = %v, want it to mention %q", name, err, want)
		}
	}
	if _, err := runSkillCall(t, r, "skill", map[string]any{"location": "/etc/passwd"}); err == nil {
		t.Error("a location outside the catalog must be refused")
	}
}

func TestSkillToolListFilter(t *testing.T) {
	r := skillToolRuntime(t)
	out, err := runSkillCall(t, r, "skill", map[string]any{"query": "nothing-matches"})
	if err != nil || out != "[]" {
		t.Errorf("filtered listing = %q, %v", out, err)
	}
}

// The system prompt lists only model-invocable skills and is advertised
// with the skill tool.
func TestSkillCatalogInSystemPrompt(t *testing.T) {
	r := skillToolRuntime(t)
	r.skillTool = r.skillLoadTool(map[string]struct{}{"skill": {}})
	if r.skillTool != "skill" {
		t.Fatalf("skill load tool = %q, want skill", r.skillTool)
	}
	prompt := buildSystemStringWith(toolLoopConfig{SystemPrompt: "base", SkillsCatalog: r.skillsCatalog, skillLoadTool: r.skillTool}, "")
	if !strings.Contains(prompt, "- greet: Greets someone") {
		t.Errorf("system prompt lacks the greet line:\n%s", prompt)
	}
	if strings.Contains(prompt, "deploy") || strings.Contains(prompt, "- off") {
		t.Errorf("user-only and disabled skills leaked into the prompt:\n%s", prompt)
	}
	surface := r.buildToolSurface([]string{"skill"}, "")
	if len(surface.deferredNames()) != 0 {
		t.Errorf("with skills installed the skill tool must be advertised, deferred = %v", surface.deferredNames())
	}
}

// SkillCatalogFor honours the user config: compat discovery switch and the
// disabled_skills list.
func TestSkillCatalogForHonoursConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", "")
	cwd := t.TempDir()
	writeSkillDir(t, filepath.Join(cwd, ".spettro", "skills"), "native", "description: d\n", "b\n")
	writeSkillDir(t, filepath.Join(home, ".claude", "skills"), "claude-one", "description: d\n", "b\n")
	ReloadSkills()
	t.Cleanup(ReloadSkills)

	names := func(c skills.Catalog) []string {
		var out []string
		for _, s := range c.Active() {
			out = append(out, s.Name)
		}
		return out
	}
	if got := names(SkillCatalogFor(cwd, config.UserConfig{})); strings.Join(got, ",") != "claude-one,native" {
		t.Errorf("default catalog = %v", got)
	}
	if got := names(SkillCatalogFor(cwd, config.UserConfig{SkillsCompatDisabled: true})); strings.Join(got, ",") != "native" {
		t.Errorf("compat off = %v", got)
	}
	if got := names(SkillCatalogFor(cwd, config.UserConfig{DisabledSkills: []string{"Native"}})); strings.Join(got, ",") != "claude-one" {
		t.Errorf("disabled native = %v", got)
	}
}

// An agent that cannot load a skill (it holds neither the skill tool nor an
// unfolded skill-read) gets no skill list: the list would tell the model to
// call a tool it does not have, and file-read cannot reach the user-level
// skill folders outside the workspace anyway.
func TestSkillCatalogOnlyForAgentsThatCanLoadSkills(t *testing.T) {
	r := skillToolRuntime(t)
	allowed := map[string]struct{}{"file-read": {}, "glob": {}}
	r.skillTool = r.skillLoadTool(allowed)
	if r.skillTool != "" {
		t.Fatalf("skill load tool = %q for an agent without the skill tool", r.skillTool)
	}
	prompt := buildSystemStringWith(toolLoopConfig{SystemPrompt: "base", SkillsCatalog: r.skillsCatalog, skillLoadTool: r.skillTool}, "")
	if strings.Contains(prompt, "greet") || strings.Contains(prompt, "available_skills") {
		t.Errorf("an agent without a skill tool was given the skill list:\n%s", prompt)
	}
}

// unfoldedSkillRuntime is skillToolRuntime with a manifest in which the
// operator owns a script called skill, so v14 left the built-in skill-read
// (with its old aliases) and skill-list unfolded.
func unfoldedSkillRuntime(t *testing.T) *toolRuntime {
	t.Helper()
	r := skillToolRuntime(t)
	userSkill := config.ToolSpec{ID: "skill", Name: "My skill script", Kind: "script", EntryPoint: "./skill.sh", Enabled: true, PermittedActions: []string{"read"}}
	read := config.ToolSpec{ID: "skill-read", Name: "Skill Read", Kind: "builtin", Enabled: true, PermittedActions: []string{"read"}, Aliases: []string{"activate-skill", "skill-activate"}}
	list := config.ToolSpec{ID: "skill-list", Name: "Skill List", Kind: "builtin", Enabled: true, PermittedActions: []string{"read"}}
	r.manifest = &config.AgentManifest{Tools: []config.ToolSpec{userSkill, read, list}}
	r.toolPolicies = map[string]config.ToolSpec{
		"skill": userSkill, "skill-read": read, "activate-skill": read, "skill-activate": read, "skill-list": list,
	}
	return r
}

// With a tool of the operator's own called skill, the built-in skill tools
// keep their old names: calls are not rewritten into the operator's tool,
// the one the agent holds runs the built-in, it is advertised under its own
// name, and the system prompt points the model at it.
func TestUnfoldedSkillToolsKeepWorking(t *testing.T) {
	r := unfoldedSkillRuntime(t)
	ctx := context.Background()
	held := map[string]struct{}{"skill-read": {}, "activate-skill": {}, "skill-activate": {}, "skill-list": {}}

	for _, name := range []string{"skill-read", "activate-skill", "skill-activate"} {
		call := aliasCall(t, name, map[string]any{"name": "greet", "args": "Ada"})
		if got, err := r.canonicalCall(call); err != nil || got.Tool != name {
			t.Fatalf("%s became %+v, %v", name, got, err)
		}
		out, err := r.execute(ctx, call, held)
		if err != nil || !strings.Contains(out, "Say hello to Ada.") {
			t.Errorf("%s: out=%q err=%v", name, out, err)
		}
	}
	out, err := r.execute(ctx, aliasCall(t, "skill-list", map[string]any{}), held)
	if err != nil || !strings.Contains(out, `"name":"greet"`) {
		t.Errorf("skill-list: out=%q err=%v", out, err)
	}

	// A retired name the agent does not hold never reaches the operator's
	// script.
	res := r.parallelExec(ctx, []toolCall{aliasCall(t, "skill-read", map[string]any{"name": "greet"})}, map[string]struct{}{"skill": {}}, nil)
	if res[0].status != "error" || !strings.Contains(res[0].output, `tool "skill-read" not allowed`) {
		t.Errorf("skill-read, not held: %s %q", res[0].status, res[0].output)
	}

	// The operator's skill is not the built-in: the built-in must neither
	// run under its name nor lend it its schema.
	if out, err := r.execute(ctx, aliasCall(t, "skill", map[string]any{"name": "greet"}), map[string]struct{}{"skill": {}}); err == nil || strings.Contains(out, "skill_content") {
		t.Errorf("the operator's skill ran the built-in: out=%q err=%v", out, err)
	}

	r.skillTool = r.skillLoadTool(held)
	if r.skillTool != "skill-read" {
		t.Fatalf("skill load tool = %q, want skill-read", r.skillTool)
	}
	surface := r.buildToolSurface([]string{"skill", "skill-read", "skill-list", "tool-search"}, "")
	names := map[string]bool{}
	for _, spec := range append(surface.specs(), deferredSpecs(surface)...) {
		names[spec.Name] = true
		if spec.Name != "tool-search" && (len(spec.Schema) == 0 || spec.Description == "") {
			t.Errorf("%s advertised without a schema or description", spec.Name)
		}
	}
	if !names["skill-read"] || !names["skill-list"] || names["skill"] {
		t.Errorf("advertised = %v, want skill-read and skill-list but not the operator's skill", names)
	}
	if slices.Contains(surface.deferredNames(), "skill-read") {
		t.Error("the tool the skill list names must be advertised up front")
	}
	prompt := buildSystemStringWith(toolLoopConfig{SystemPrompt: "base", SkillsCatalog: r.skillsCatalog, skillLoadTool: r.skillTool}, "")
	if !strings.Contains(prompt, "`skill-read`") || strings.Contains(prompt, "`skill`") {
		t.Errorf("the skill list must point at skill-read:\n%s", prompt)
	}
}

// deferredSpecs returns the specs a surface holds back behind tool-search.
func deferredSpecs(s *toolSurface) []provider.ToolSpec {
	var out []provider.ToolSpec
	for _, name := range s.deferredNames() {
		out = append(out, s.deferred[name])
	}
	return out
}
