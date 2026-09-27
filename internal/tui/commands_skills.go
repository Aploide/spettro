package tui

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"

	"spettro/internal/agent"
	"spettro/internal/config"
	"spettro/internal/skills"
)

// This file holds the TUI's skill surface:
//
//   - /skill <sub> and /skills: manage and list skills (handleSkillsCommand);
//   - /<skill-name> [args]: run a skill (findUserSkill, runUserSkill), wired
//     into handleCommand after built-ins and custom commands;
//   - $<skill-name> mentions in a prompt (expandSkillMentions), wired into
//     handlePrompt.
//
// All of them read the catalog through skillCatalog, which is the shared
// session catalog from agent.SkillCatalogFor, so the menu, the listing and
// the model always agree.

const skillsUsage = "usage: /skill <list|install|uninstall|info|enable|disable|reload|where> ..."

func (m Model) handleSkillsCommand(input string) (tea.Model, tea.Cmd) {
	fields := strings.Fields(input)
	if len(fields) <= 1 {
		return m.runSkillsList()
	}
	switch strings.ToLower(fields[1]) {
	case "list", "ls":
		return m.runSkillsList()
	case "install", "add":
		return m.runSkillsInstall(fields[2:])
	case "uninstall", "remove", "rm":
		return m.runSkillsUninstall(fields[2:])
	case "info", "show":
		return m.runSkillsInfo(fields[2:])
	case "enable":
		return m.runSkillsEnable(fields[2:], true)
	case "disable":
		return m.runSkillsEnable(fields[2:], false)
	case "reload", "refresh":
		agent.ReloadSkills()
		m.showBanner(fmt.Sprintf("skills reloaded: %d found", len(m.skillCatalog().Skills)), "success")
		return m, nil
	case "where", "paths":
		return m.runSkillsWhere()
	case "help":
		m.pushSystemMsg(skillsHelp)
		return m, nil
	default:
		m.showBanner(skillsUsage, "info")
		return m, nil
	}
}

const skillsHelp = `skills commands:
  /skills                                  list discovered skills and where they come from
  /<skill-name> [args]                     run a skill (also: mention $skill-name in a prompt)
  /skill install <source> [--force]        install from local path, https git URL, or owner/repo
  /skill install <source> --project        install into <cwd>/.spettro/skills (default: ~/.spettro/skills)
  /skill install <source> --as=<name>      override destination name
  /skill install <source> --path=<subdir>  pick a subdirectory inside the source
  /skill uninstall <name> [--project]      remove a skill installed in .spettro/skills
  /skill info <name>                       show metadata + body excerpt
  /skill enable <name> | disable <name>    show or hide a skill everywhere (saved in your config)
  /skill where                             show discovery roots
  /skill reload                            force a re-scan of the skill directories`

// skillCatalog returns the session's skill catalog under the current config.
func (m Model) skillCatalog() skills.Catalog {
	return agent.SkillCatalogFor(m.cwd, m.cfg)
}

// runSkillsList renders /skills: every discovered skill with how it can be
// invoked and where it came from, then shadowed skills and warnings.
func (m Model) runSkillsList() (tea.Model, tea.Cmd) {
	cat := m.skillCatalog()
	if len(cat.Skills) == 0 {
		rows := []string{"no skills discovered. install one with /skill install <source>, or add a folder with a SKILL.md to one of these search roots:", ""}
		rows = append(rows, m.skillRootRows()...)
		m.pushSystemMsg(strings.Join(rows, "\n"))
		return m, nil
	}
	rows := []string{fmt.Sprintf("skills (%d):", len(cat.Skills))}
	for _, s := range cat.Skills {
		rows = append(rows, fmt.Sprintf("- %s  [%s]  %s", s.Name, skillInvocationLabel(s), truncateLabel(s.ListingDescription(), 96)))
		rows = append(rows, fmt.Sprintf("    %s/%s  %s", s.Source, s.Scope, s.Location))
	}
	if len(cat.Shadowed) > 0 {
		rows = append(rows, "", fmt.Sprintf("shadowed by a skill of the same name (%d):", len(cat.Shadowed)))
		for _, s := range cat.Shadowed {
			rows = append(rows, fmt.Sprintf("- %s  %s/%s  %s", s.Name, s.Source, s.Scope, s.Location))
		}
	}
	if len(cat.Issues) > 0 {
		rows = append(rows, "", "warnings:")
		for _, msg := range cat.Issues {
			rows = append(rows, "- "+msg)
		}
	}
	rows = append(rows, "", "run one with /<name> [args] or mention it as $<name>; the agent loads the others by itself when they fit.")
	m.pushSystemMsg(strings.Join(rows, "\n"))
	return m, nil
}

// skillInvocationLabel says who can run a skill, for /skills.
func skillInvocationLabel(s skills.Skill) string {
	switch {
	case s.Disabled:
		return "disabled"
	case s.ModelInvocationDisabled && s.UserInvocationDisabled:
		return "nobody"
	case s.ModelInvocationDisabled:
		return "/" + s.Name + " only"
	case s.UserInvocationDisabled:
		return "agent only"
	default:
		return "/" + s.Name
	}
}

func (m Model) runSkillsInstall(args []string) (tea.Model, tea.Cmd) {
	if len(args) == 0 {
		m.showBanner("usage: /skill install <source> [--project] [--force] [--as=<name>] [--path=<subdir>]", "info")
		return m, nil
	}
	opts := skills.InstallOptions{
		Scope: skills.ScopeUser,
		CWD:   m.cwd,
	}
	for _, a := range args {
		switch {
		case a == "--project" || a == "-p":
			opts.Scope = skills.ScopeProject
		case a == "--user" || a == "-u":
			opts.Scope = skills.ScopeUser
		case a == "--force" || a == "-f":
			opts.Force = true
		case strings.HasPrefix(a, "--as="):
			opts.Name = strings.TrimPrefix(a, "--as=")
		case strings.HasPrefix(a, "--path="):
			opts.SubPath = strings.TrimPrefix(a, "--path=")
		case strings.HasPrefix(a, "-"):
			m.showBanner("unknown flag: "+a, "error")
			return m, nil
		default:
			if opts.Source != "" {
				m.showBanner("multiple sources provided; only one is supported", "error")
				return m, nil
			}
			opts.Source = a
		}
	}
	if opts.Source == "" {
		m.showBanner("usage: /skill install <source>", "error")
		return m, nil
	}
	m.showBanner(fmt.Sprintf("installing skill from %s ...", opts.Source), "info")
	ctx, cancel := context.WithTimeout(context.Background(), skills.InstallTimeout)
	defer cancel()
	res, err := skills.Install(ctx, opts)
	if err != nil {
		m.showBanner("install failed: "+err.Error(), "error")
		return m, nil
	}
	agent.ReloadSkills()
	verb := "installed"
	if res.Replaced {
		verb = "reinstalled"
	}
	m.pushSystemMsg(fmt.Sprintf(
		"%s skill %q\n  source: %s\n  destination: %s\n  description: %s\n  run it with /%s",
		verb, res.Skill.Name, res.Source, res.Destination, truncateLabel(res.Skill.Description, 200), res.Skill.Name,
	))
	m.showBanner(fmt.Sprintf("skill %q %s", res.Skill.Name, verb), "success")
	return m, nil
}

func (m Model) runSkillsUninstall(args []string) (tea.Model, tea.Cmd) {
	if len(args) == 0 {
		m.showBanner("usage: /skill uninstall <name> [--project]", "info")
		return m, nil
	}
	scope := skills.ScopeUser
	var name string
	for _, a := range args {
		switch {
		case a == "--project" || a == "-p":
			scope = skills.ScopeProject
		case a == "--user" || a == "-u":
			scope = skills.ScopeUser
		case strings.HasPrefix(a, "-"):
			m.showBanner("unknown flag: "+a, "error")
			return m, nil
		default:
			if name != "" {
				m.showBanner("only one skill name can be uninstalled at a time", "error")
				return m, nil
			}
			name = a
		}
	}
	if name == "" {
		m.showBanner("usage: /skill uninstall <name> [--project]", "error")
		return m, nil
	}
	if err := skills.Uninstall(name, scope, m.cwd); err != nil {
		msg := "uninstall failed: " + err.Error()
		if s, ok := m.skillCatalog().Find(name); ok && s.Source != skills.SourceSpettro {
			// Spettro never writes to another tool's skill folder.
			msg = fmt.Sprintf("%q lives in %s, which Spettro only reads; remove it there, or /skill disable %s", s.Name, s.Directory, s.Name)
		}
		m.showBanner(msg, "error")
		return m, nil
	}
	agent.ReloadSkills()
	m.showBanner(fmt.Sprintf("skill %q uninstalled", name), "success")
	return m, nil
}

func (m Model) runSkillsInfo(args []string) (tea.Model, tea.Cmd) {
	if len(args) == 0 {
		m.showBanner("usage: /skill info <name>", "info")
		return m, nil
	}
	name := args[0]
	skill, ok := m.skillCatalog().Find(name)
	if !ok {
		m.showBanner(fmt.Sprintf("skill %q not found (run /skills to list them)", name), "error")
		return m, nil
	}
	body, err := skills.LoadBody(skill)
	if err != nil {
		m.showBanner("read skill body failed: "+err.Error(), "error")
		return m, nil
	}
	rows := []string{
		fmt.Sprintf("skill: %s", skill.Name),
		fmt.Sprintf("invocation: %s", skillInvocationLabel(skill)),
		fmt.Sprintf("source: %s/%s", skill.Source, skill.Scope),
		fmt.Sprintf("location: %s", skill.Location),
		fmt.Sprintf("directory: %s", skill.Directory),
	}
	optional := []struct{ label, value string }{
		{"argument-hint", skill.ArgumentHint},
		{"arguments", strings.Join(skill.Arguments, " ")},
		{"license", skill.License},
		{"compatibility", skill.Compatibility},
		{"allowed-tools (not enforced)", skill.AllowedTools},
	}
	for _, f := range optional {
		if f.value != "" {
			rows = append(rows, f.label+": "+f.value)
		}
	}
	if len(skill.Resources) > 0 {
		rows = append(rows, "resources:")
		for _, r := range skill.Resources {
			rows = append(rows, "  - "+r)
		}
	}
	if len(skill.Issues) > 0 {
		rows = append(rows, "warnings:")
		for _, msg := range skill.Issues {
			rows = append(rows, "  - "+msg)
		}
	}
	rows = append(rows, "", "description:", skill.ListingDescription())
	rows = append(rows, "", "instructions (excerpt):", truncateLabel(body, 1500))
	m.pushSystemMsg(strings.Join(rows, "\n"))
	return m, nil
}

// runSkillsEnable records a skill as enabled or disabled in the user config
// (disabled_skills). Enabling also removes a legacy .spettro-disabled marker
// from a Spettro-owned skill folder, since that marker would otherwise keep
// the skill off.
func (m Model) runSkillsEnable(args []string, enable bool) (tea.Model, tea.Cmd) {
	verb := "enable"
	if !enable {
		verb = "disable"
	}
	if len(args) == 0 {
		m.showBanner(fmt.Sprintf("usage: /skill %s <name>", verb), "info")
		return m, nil
	}
	skill, ok := m.skillCatalog().Find(args[0])
	if !ok {
		m.showBanner(fmt.Sprintf("skill %q not found", args[0]), "error")
		return m, nil
	}
	if err := m.updateConfig(func(cfg *config.UserConfig) error {
		cfg.DisabledSkills = setSkillDisabled(cfg.DisabledSkills, skill.Name, !enable)
		return nil
	}); err != nil {
		m.showBanner(verb+" failed: "+err.Error(), "error")
		return m, nil
	}
	if enable && skill.Source == skills.SourceSpettro {
		marker := filepath.Join(skill.Directory, skills.DisabledMarker)
		if err := os.Remove(marker); err == nil {
			// The marker was read at discovery; rescan so it takes effect.
			agent.ReloadSkills()
		}
	}
	state := "enabled"
	if !enable {
		state = "disabled"
	}
	m.showBanner(fmt.Sprintf("skill %q %s", skill.Name, state), "success")
	return m, nil
}

// setSkillDisabled adds name to (disabled) or removes it from (enabled) a
// disabled_skills list, case-insensitively and without duplicates.
func setSkillDisabled(list []string, name string, disabled bool) []string {
	out := slices.DeleteFunc(slices.Clone(list), func(n string) bool { return strings.EqualFold(n, name) })
	if disabled {
		out = append(out, name)
	}
	return out
}

func (m Model) runSkillsWhere() (tea.Model, tea.Cmd) {
	rows := []string{"skill discovery roots (in priority order; the first skill of a name wins):"}
	rows = append(rows, m.skillRootRows()...)
	rows = append(rows, "", "* the directory exists; (read-only) roots belong to other agents and are never written to.")
	if m.cfg.SkillsCompatDisabled {
		rows = append(rows, "Claude Code / Codex roots are off (skills_compat_disabled in config.json).")
	}
	m.pushSystemMsg(strings.Join(rows, "\n"))
	return m, nil
}

// skillRootRows lists the discovery roots, marking the ones that exist.
func (m Model) skillRootRows() []string {
	var rows []string
	for _, r := range skills.SearchRoots(m.cwd, agent.SkillLookupOptions(m.cfg)) {
		marker := " "
		if _, err := os.Stat(r.Path); err == nil {
			marker = "*"
		}
		suffix := ""
		if r.ReadOnly() {
			suffix = " (read-only)"
		}
		rows = append(rows, fmt.Sprintf("%s [%s/%s] %s%s", marker, r.Source, r.Scope, r.Path, suffix))
	}
	return rows
}

// findUserSkill resolves "/name" to a skill the user may run. Built-in and
// custom commands are resolved before this is called, so they win a name
// collision (see handleCommand).
func (m Model) findUserSkill(cmd string) (skills.Skill, bool) {
	s, ok := m.skillCatalog().Find(strings.TrimPrefix(cmd, "/"))
	if !ok || !s.UserInvocable() {
		return skills.Skill{}, false
	}
	return s, true
}

// runUserSkill runs "/name args": the transcript shows what the user typed,
// and the agent receives the skill's instructions with args substituted
// (skills.UserInvocationPrompt).
func (m Model) runUserSkill(input string, skill skills.Skill, args string) (tea.Model, tea.Cmd) {
	prompt, err := skills.UserInvocationPrompt(skill, args)
	if err != nil {
		m.showBanner(err.Error(), "error")
		return m, nil
	}
	return m.handlePromptWith(input, prompt)
}

// expandSkillMentions appends the instructions of every skill the prompt
// names as $skill-name (Codex style) and tells the user which ones were
// loaded. The typed text is kept as is.
func (m *Model) expandSkillMentions(prompt string) string {
	expanded, names := skills.ExpandMentions(prompt, m.skillCatalog())
	if len(names) > 0 {
		m.showBanner("skills loaded: "+strings.Join(names, ", "), "info")
	}
	return expanded
}
