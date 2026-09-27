package config

import (
	"slices"
	"testing"
)

// v13SkillManifest is a v13 manifest with the two skill tools as v13
// shipped them, and agents holding both, only the listing one, or neither.
const v13SkillManifest = `
version = 13
default_agent = "coder"

[runtime]
default_permission = "ask-first"
default_timeout_sec = 60

[[tools]]
id = "file-read"
name = "File Reader"
kind = "builtin"
enabled = true
timeout_sec = 30
permitted_actions = ["read"]

[[tools]]
id = "skill-read"
name = "Skill Read"
description = "Activate an installed Agent Skill and load its SKILL.md instructions."
kind = "builtin"
enabled = true
timeout_sec = 15
permitted_actions = ["read"]
aliases = ["activate-skill", "skill-activate"]
risk_level = "low"

[[tools]]
id = "skill-list"
name = "Skill List"
description = "List installed Agent Skills with name + description."
kind = "builtin"
enabled = true
timeout_sec = 10
permitted_actions = ["read"]
risk_level = "low"

[[agents]]
id = "coder"
name = "Coder"
mode = "orchestrator"
allowed_tools = ["file-read", "skill-read", "skill-list"]
permitted_actions = ["read"]
permission = "ask-first"
enabled = true

[[agents]]
id = "lister"
name = "Lister"
mode = "worker"
allowed_tools = ["file-read", "skill-list"]
permitted_actions = ["read"]
permission = "ask-first"
enabled = true

[[agents]]
id = "reader"
name = "Reader"
mode = "worker"
allowed_tools = ["file-read"]
permitted_actions = ["read"]
permission = "ask-first"
enabled = true
`

// skill-read becomes skill in place, skill-list merges into it, every old
// name is an alias, and each allow-list names the new tool once.
func TestV14FoldsSkillTools(t *testing.T) {
	m := decodeV12(t, v13SkillManifest)
	if hasTool(m, "skill-read") || hasTool(m, "skill-list") {
		t.Fatal("the retired skill tools must be gone")
	}
	skill := toolByID(t, m, "skill")
	want := []string{"activate-skill", "skill-activate", "skill-read", "skill-list"}
	if !slices.Equal(skill.Aliases, want) {
		t.Errorf("aliases = %v, want %v", skill.Aliases, want)
	}
	if skill.TimeoutSec != 15 || skill.Name != "Skill" {
		t.Errorf("skill = timeout %d name %q", skill.TimeoutSec, skill.Name)
	}
	for id, tools := range map[string][]string{
		"coder":  {"file-read", "skill"},
		"lister": {"file-read", "skill"},
		"reader": {"file-read"},
	} {
		a, _ := m.AgentByID(id)
		if !slices.Equal(a.AllowedTools, tools) {
			t.Errorf("%s allowed_tools = %v, want %v", id, a.AllowedTools, tools)
		}
	}
}

// A tool of the operator's own called "skill" is theirs: the built-ins are
// not folded into it and keep their names.
func TestV14LeavesAUserToolCalledSkillAlone(t *testing.T) {
	src := v13SkillManifest + `
[[tools]]
id = "skill"
name = "My skill script"
kind = "script"
entry_point = "./skill.sh"
enabled = true
timeout_sec = 10
permitted_actions = ["read"]
`
	m := decodeV12(t, src)
	if !hasTool(m, "skill-read") || !hasTool(m, "skill-list") {
		t.Fatal("built-in skill tools must stay when the canonical name is taken")
	}
	if got := toolByID(t, m, "skill"); got.Kind != "script" {
		t.Errorf("the operator's skill tool changed: %+v", got)
	}
}

// The default manifest already has the folded tool, so migrating it again
// changes nothing.
func TestV14DefaultHasSkillTool(t *testing.T) {
	m := DefaultAgentManifest()
	if !hasTool(m, "skill") || hasTool(m, "skill-read") {
		t.Fatal("default manifest must ship the canonical skill tool only")
	}
	for _, a := range m.Agents {
		if slices.Contains(a.AllowedTools, "skill-read") || slices.Contains(a.AllowedTools, "skill-list") {
			t.Errorf("agent %s still lists a retired skill tool: %v", a.ID, a.AllowedTools)
		}
	}
}
