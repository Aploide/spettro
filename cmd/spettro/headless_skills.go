package main

import (
	"strings"

	"spettro/internal/agent"
	"spettro/internal/config"
	"spettro/internal/skills"
)

// headlessCommandNames are the slash commands the headless remote handles
// itself (handleHeadlessCommand, and /approve through takeApprovedPlan). A
// skill with one of these names cannot be run as /name here: the built-in
// wins, as in the TUI and over ACP.
var headlessCommandNames = map[string]bool{
	"help": true, "mode": true, "next": true, "models": true,
	"permission": true, "approve": true, "exit": true, "quit": true,
}

// resolveHeadlessPrompt decides what the agent runs for a submitted message:
//
//   - "/<skill-name> [args]" naming a skill the user may run: the skill's
//     invocation prompt, with isSkill true;
//   - any other "/..." message: msg unchanged, isSkill false (the caller
//     handles it as a headless command);
//   - an ordinary prompt: msg with its $skill-name mentions expanded.
//
// err is set only when a skill was named but its SKILL.md could not be read.
func resolveHeadlessPrompt(cwd string, cfg config.UserConfig, msg string) (run string, isSkill bool, err error) {
	catalog := agent.SkillCatalogFor(cwd, cfg)
	if !strings.HasPrefix(msg, "/") {
		expanded, _ := skills.ExpandMentions(msg, catalog)
		return expanded, false, nil
	}
	fields := strings.Fields(msg)
	name := strings.TrimPrefix(fields[0], "/")
	if headlessCommandNames[strings.ToLower(name)] {
		return msg, false, nil
	}
	skill, ok := catalog.Find(name)
	if !ok || !skill.UserInvocable() {
		return msg, false, nil
	}
	args := strings.TrimSpace(strings.TrimPrefix(msg, fields[0]))
	prompt, err := skills.UserInvocationPrompt(skill, args)
	if err != nil {
		return "", true, err
	}
	return prompt, true, nil
}
