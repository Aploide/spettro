package tui

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"spettro/internal/skills"
)

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
		m.refreshSkillsCatalog()
		m.showBanner("skills catalog reloaded", "success")
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
  /skill list                              list discovered skills
  /skill install <source> [--force]        install from local path, https git URL, or owner/repo
  /skill install <source> --project        install into <cwd>/.spettro/skills (default: ~/.spettro/skills)
  /skill install <source> --as=<name>      override destination name
  /skill install <source> --path=<subdir>  pick a subdirectory inside the source
  /skill uninstall <name> [--project]      remove an installed skill
  /skill info <name>                       show metadata + body excerpt
  /skill enable <name> | disable <name>    toggle a skill in this project
  /skill where                             show discovery roots
  /skill reload                            re-scan skill directories`

func (m Model) runSkillsList() (tea.Model, tea.Cmd) {
	cat := m.skillsCatalog()
	if len(cat.Skills) == 0 {
		var rows []string
		rows = append(rows, "no skills discovered. install one with /skill install <source>")
		rows = append(rows, "")
		rows = append(rows, "search roots:")
		for _, r := range skills.SearchRoots(m.cwd, skills.DefaultLookupOptions()) {
			rows = append(rows, fmt.Sprintf("- [%s/%s] %s", r.Source, r.Scope, r.Path))
		}
		m.pushSystemMsg(strings.Join(rows, "\n"))
		return m, nil
	}
	rows := []string{fmt.Sprintf("installed skills (%d):", len(cat.Skills))}
	for _, s := range cat.Skills {
		state := "enabled"
		if s.Disabled {
			state = "disabled"
		}
		desc := truncateLabel(s.Description, 96)
		rows = append(rows, fmt.Sprintf("- %s [%s/%s, %s] — %s", s.Name, s.Source, s.Scope, state, desc))
	}
	if len(cat.Shadowed) > 0 {
		rows = append(rows, "", fmt.Sprintf("shadowed (%d):", len(cat.Shadowed)))
		for _, s := range cat.Shadowed {
			rows = append(rows, fmt.Sprintf("- %s [%s/%s] — %s", s.Name, s.Source, s.Scope, s.Location))
		}
	}
	if len(cat.Issues) > 0 {
		rows = append(rows, "", "warnings:")
		for _, msg := range cat.Issues {
			rows = append(rows, "- "+msg)
		}
	}
	m.pushSystemMsg(strings.Join(rows, "\n"))
	return m, nil
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
	m.refreshSkillsCatalog()
	verb := "installed"
	if res.Replaced {
		verb = "reinstalled"
	}
	m.pushSystemMsg(fmt.Sprintf(
		"%s skill %q\n  source: %s\n  destination: %s\n  description: %s",
		verb, res.Skill.Name, res.Source, res.Destination, truncateLabel(res.Skill.Description, 200),
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
		m.showBanner("uninstall failed: "+err.Error(), "error")
		return m, nil
	}
	m.refreshSkillsCatalog()
	m.showBanner(fmt.Sprintf("skill %q uninstalled", name), "success")
	return m, nil
}

func (m Model) runSkillsInfo(args []string) (tea.Model, tea.Cmd) {
	if len(args) == 0 {
		m.showBanner("usage: /skill info <name>", "info")
		return m, nil
	}
	name := args[0]
	cat := m.skillsCatalog()
	skill, ok := cat.Find(name)
	if !ok {
		m.showBanner(fmt.Sprintf("skill %q not found (run /skill list to discover)", name), "error")
		return m, nil
	}
	body, err := skills.LoadBody(skill)
	if err != nil {
		m.showBanner("read skill body failed: "+err.Error(), "error")
		return m, nil
	}
	rows := []string{
		fmt.Sprintf("skill: %s", skill.Name),
		fmt.Sprintf("source: %s/%s", skill.Source, skill.Scope),
		fmt.Sprintf("location: %s", skill.Location),
		fmt.Sprintf("directory: %s", skill.Directory),
	}
	if skill.License != "" {
		rows = append(rows, "license: "+skill.License)
	}
	if skill.Compatibility != "" {
		rows = append(rows, "compatibility: "+skill.Compatibility)
	}
	if skill.AllowedTools != "" {
		rows = append(rows, "allowed-tools: "+skill.AllowedTools)
	}
	if skill.Disabled {
		rows = append(rows, "status: disabled")
	} else {
		rows = append(rows, "status: enabled")
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
	rows = append(rows, "", "description:", skill.Description)
	rows = append(rows, "", "instructions (excerpt):", truncateLabel(body, 1500))
	m.pushSystemMsg(strings.Join(rows, "\n"))
	return m, nil
}

func (m Model) runSkillsEnable(args []string, enable bool) (tea.Model, tea.Cmd) {
	if len(args) == 0 {
		verb := "enable"
		if !enable {
			verb = "disable"
		}
		m.showBanner(fmt.Sprintf("usage: /skill %s <name>", verb), "info")
		return m, nil
	}
	name := args[0]
	cat := m.skillsCatalog()
	skill, ok := cat.Find(name)
	if !ok {
		m.showBanner(fmt.Sprintf("skill %q not found", name), "error")
		return m, nil
	}
	if err := writeSkillEnabledMarker(skill, enable); err != nil {
		m.showBanner("toggle failed: "+err.Error(), "error")
		return m, nil
	}
	m.refreshSkillsCatalog()
	state := "enabled"
	if !enable {
		state = "disabled"
	}
	m.showBanner(fmt.Sprintf("skill %q %s", skill.Name, state), "success")
	return m, nil
}

func (m Model) runSkillsWhere() (tea.Model, tea.Cmd) {
	rows := []string{"skill discovery roots (in priority order):"}
	for _, r := range skills.SearchRoots(m.cwd, skills.DefaultLookupOptions()) {
		marker := " "
		if _, err := os.Stat(r.Path); err == nil {
			marker = "*"
		}
		rows = append(rows, fmt.Sprintf("%s [%s/%s] %s", marker, r.Source, r.Scope, r.Path))
	}
	rows = append(rows, "", "* indicates the directory exists.")
	m.pushSystemMsg(strings.Join(rows, "\n"))
	return m, nil
}

func writeSkillEnabledMarker(s skills.Skill, enable bool) error {
	if strings.TrimSpace(s.Directory) == "" {
		return fmt.Errorf("skill has no directory")
	}
	disabledFlag := filepath.Join(s.Directory, ".spettro-disabled")
	if enable {
		if err := os.Remove(disabledFlag); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	return os.WriteFile(disabledFlag, []byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0o644)
}

// skillsCatalog re-discovers skills and applies any persisted enabled markers.
func (m Model) skillsCatalog() skills.Catalog {
	cat, _ := skills.Discover(m.cwd, skills.DefaultLookupOptions())
	for i := range cat.Skills {
		flag := filepath.Join(cat.Skills[i].Directory, ".spettro-disabled")
		if _, err := os.Stat(flag); err == nil {
			cat.Skills[i].Disabled = true
		}
	}
	return cat
}

// refreshSkillsCatalog is called after install/uninstall/enable changes; it's
// a no-op today (catalog is rebuilt on demand) but reserved for caching.
func (m *Model) refreshSkillsCatalog() {
	_ = m.skillsCatalog()
}
