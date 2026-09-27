package acp

// Agent Skills over ACP. Three pieces mirror the TUI (internal/tui/
// commands_skills.go):
//
//   - sessionCommands advertises every skill the user may run as an
//     available command next to Spettro's own, so the client offers
//     /<skill-name> in its command menu;
//   - resolveSkillCommand turns "/<skill-name> [args]" into the turn's
//     prompt (the skill's instructions with the arguments substituted);
//   - /skills lists the discovered skills as text (skillsText).
//
// $skill-name mentions in an ordinary prompt are expanded in Prompt with
// skills.MentionInstructions. Both a skill command and mentions are read
// from the text the user typed, never from files the editor attached (see
// promptContent). Built-in commands win a name collision: a skill called
// "help" is neither advertised nor run as /help.

import (
	"fmt"
	"strings"

	acpsdk "github.com/coder/acp-go-sdk"

	"spettro/internal/agent"
	"spettro/internal/config"
	"spettro/internal/skills"
)

// acpHiddenCommandNames are commands the bridge handles without advertising
// them (aliases and TUI holdovers). Together with acpAvailableCommands they
// are the names a skill may not take; keep this in sync with the switches in
// handleSlashCommand, handleExtendedSlashCommand and Prompt.
var acpHiddenCommandNames = []string{"next", "model", "think", "workflow", "init", "approve"}

// acpReservedCommandNames returns every command name the bridge handles
// itself, lower-cased and without the leading slash.
func acpReservedCommandNames() map[string]bool {
	names := map[string]bool{}
	for _, c := range acpAvailableCommands {
		names[strings.ToLower(c.Name)] = true
	}
	for _, n := range acpHiddenCommandNames {
		names[n] = true
	}
	return names
}

// skillCommands returns the user-invocable skills of cat as ACP commands,
// skipping names the bridge already handles. The input hint is the skill's
// argument-hint, or a generic one, since any skill accepts free text.
func skillCommands(cat skills.Catalog) []acpsdk.AvailableCommand {
	reserved := acpReservedCommandNames()
	var out []acpsdk.AvailableCommand
	for _, s := range cat.ForUser() {
		if reserved[strings.ToLower(s.Name)] {
			continue
		}
		hint := s.ArgumentHint
		if hint == "" {
			hint = "[arguments]"
		}
		out = append(out, acpsdk.AvailableCommand{
			Name:        s.Name,
			Description: "skill: " + s.ListingDescription(),
			Input:       hintInput(hint),
		})
	}
	return out
}

// sessionCommands is the command list announced for a session: Spettro's
// own commands, then the session workspace's skills. A session the bridge
// no longer knows gets the built-ins only.
func (b *bridge) sessionCommands(sid acpsdk.SessionId) []acpsdk.AvailableCommand {
	b.mu.Lock()
	s, ok := b.sessions[string(sid)]
	cwd := ""
	if ok {
		cwd = s.cwd
	}
	b.mu.Unlock()
	cmds := append([]acpsdk.AvailableCommand(nil), acpAvailableCommands...)
	if cwd == "" {
		return cmds
	}
	cfg, _ := config.Load()
	return append(cmds, skillCommands(agent.SkillCatalogFor(cwd, cfg))...)
}

// resolveSkillCommand maps "/<name> [args]" onto a skill the user may run.
// handled is false when input names no such skill (the caller then treats
// it as an ordinary prompt). When handled, either prompt is the text to run
// the turn with, or reply is an error to show instead of running.
func resolveSkillCommand(cwd string, cfg config.UserConfig, input string) (prompt, reply string, handled bool) {
	fields := strings.Fields(input)
	if len(fields) == 0 || !strings.HasPrefix(fields[0], "/") {
		return "", "", false
	}
	name := strings.TrimPrefix(fields[0], "/")
	if acpReservedCommandNames()[strings.ToLower(name)] {
		return "", "", false
	}
	skill, ok := agent.SkillCatalogFor(cwd, cfg).Find(name)
	if !ok || !skill.UserInvocable() {
		return "", "", false
	}
	args := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(input), fields[0]))
	prompt, err := skills.UserInvocationPrompt(skill, args)
	if err != nil {
		return "", "skill " + skill.Name + ": " + err.Error(), true
	}
	return prompt, "", true
}

// skillsText renders /skills for ACP clients: each skill, who can run it,
// its source and SKILL.md path.
func skillsText(cwd string, cfg config.UserConfig) string {
	cat := agent.SkillCatalogFor(cwd, cfg)
	if len(cat.Skills) == 0 {
		return "no skills discovered. Add a folder with a SKILL.md to .spettro/skills (project) or ~/.spettro/skills; " +
			"skills in .claude/skills, .agents/skills and ~/.codex/skills are found too."
	}
	var b strings.Builder
	fmt.Fprintf(&b, "skills (%d):\n", len(cat.Skills))
	for _, s := range cat.Skills {
		who := "/" + s.Name
		switch {
		case s.Disabled:
			who = "disabled"
		case s.UserInvocationDisabled:
			who = "agent only"
		case s.ModelInvocationDisabled:
			who += " only"
		}
		fmt.Fprintf(&b, "- %s [%s] %s\n    %s/%s %s\n", s.Name, who, s.ListingDescription(), s.Source, s.Scope, s.Location)
	}
	if len(cat.Shadowed) > 0 {
		fmt.Fprintf(&b, "shadowed by a skill of the same name: %d (see the TUI's /skills for details)\n", len(cat.Shadowed))
	}
	b.WriteString("run one with /<name> [args] or mention it as $<name>")
	return b.String()
}
