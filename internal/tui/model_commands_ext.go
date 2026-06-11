package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"spettro/internal/config"
	"spettro/internal/hooks"
	"spettro/internal/mcp"
	"spettro/internal/session"
	"spettro/internal/skills"
)

func (m Model) handleTasksCommand(input string) (tea.Model, tea.Cmd) {
	m.ensureSession()
	fields := strings.Fields(input)
	if len(fields) == 1 || strings.EqualFold(fields[1], "list") {
		m.syncTodosFromSession()
		if len(m.todos) == 0 {
			m.pushSystemMsg("no tasks in this session")
			return m, nil
		}
		var rows []string
		for _, t := range m.todos {
			rows = append(rows, fmt.Sprintf("- [%s] %s (%s)", t.Status, t.Content, t.ID))
		}
		m.pushSystemMsg("tasks:\n" + strings.Join(rows, "\n"))
		return m, nil
	}
	globalDir := m.store.GlobalDir
	switch strings.ToLower(fields[1]) {
	case "add":
		content := strings.TrimSpace(strings.TrimPrefix(input, "/tasks add"))
		if content == "" {
			m.showBanner("usage: /tasks add <content>", "error")
			return m, nil
		}
		item := session.Todo{
			ID:      fmt.Sprintf("task-%d", time.Now().UnixMilli()),
			Content: content,
			Status:  "pending",
			Source:  "command",
		}
		if _, err := session.UpsertTodo(globalDir, m.sessionID, item); err != nil {
			m.showBanner("tasks add failed: "+err.Error(), "error")
			return m, nil
		}
		m.syncTodosFromSession()
		m.showBanner("task added", "success")
	case "done":
		if len(fields) < 3 {
			m.showBanner("usage: /tasks done <id>", "error")
			return m, nil
		}
		id := strings.TrimSpace(fields[2])
		item, ok, err := session.GetTodo(globalDir, m.sessionID, id)
		if err != nil {
			m.showBanner("tasks done failed: "+err.Error(), "error")
			return m, nil
		}
		if !ok {
			m.showBanner("task not found: "+id, "error")
			return m, nil
		}
		item.Status = "completed"
		if _, err := session.UpsertTodo(globalDir, m.sessionID, item); err != nil {
			m.showBanner("tasks done failed: "+err.Error(), "error")
			return m, nil
		}
		m.syncTodosFromSession()
		m.showBanner("task marked completed", "success")
	case "set":
		if len(fields) < 4 {
			m.showBanner("usage: /tasks set <id> <status>", "error")
			return m, nil
		}
		id, st := strings.TrimSpace(fields[2]), strings.TrimSpace(fields[3])
		item, ok, err := session.GetTodo(globalDir, m.sessionID, id)
		if err != nil {
			m.showBanner("tasks set failed: "+err.Error(), "error")
			return m, nil
		}
		if !ok {
			m.showBanner("task not found: "+id, "error")
			return m, nil
		}
		item.Status = st
		if _, err := session.UpsertTodo(globalDir, m.sessionID, item); err != nil {
			m.showBanner("tasks set failed: "+err.Error(), "error")
			return m, nil
		}
		m.syncTodosFromSession()
		m.showBanner("task updated", "success")
	case "show":
		if len(fields) < 3 {
			m.showBanner("usage: /tasks show <id>", "error")
			return m, nil
		}
		id := strings.TrimSpace(fields[2])
		item, ok, err := session.GetTodo(globalDir, m.sessionID, id)
		if err != nil {
			m.showBanner("tasks show failed: "+err.Error(), "error")
			return m, nil
		}
		if !ok {
			m.showBanner("task not found: "+id, "error")
			return m, nil
		}
		raw, _ := json.MarshalIndent(item, "", "  ")
		m.pushSystemMsg(string(raw))
	default:
		m.showBanner("usage: /tasks [list|add|done|set|show]", "info")
	}
	return m, nil
}

func (m Model) handleMCPCommand(input string) (tea.Model, tea.Cmd) {
	fields := strings.Fields(input)
	if len(fields) < 2 {
		m.showBanner("usage: /mcp <list|read|auth>", "info")
		return m, nil
	}
	switch strings.ToLower(fields[1]) {
	case "list":
		serverID := ""
		if len(fields) >= 3 {
			serverID = fields[2]
		}
		rows, err := mcp.ListResources(m.cwd, serverID)
		if err != nil {
			m.showBanner("mcp list failed: "+err.Error(), "error")
			return m, nil
		}
		if len(rows) == 0 {
			m.pushSystemMsg("no mcp resources configured")
			return m, nil
		}
		raw, _ := json.MarshalIndent(rows, "", "  ")
		m.pushSystemMsg(string(raw))
	case "read":
		if len(fields) < 4 {
			m.showBanner("usage: /mcp read <server_id> <resource_id>", "error")
			return m, nil
		}
		out, err := mcp.ReadResource(m.cwd, fields[2], fields[3])
		if err != nil {
			m.showBanner("mcp read failed: "+err.Error(), "error")
			return m, nil
		}
		m.pushSystemMsg(truncateLabel(out, 6000))
	case "auth":
		if len(fields) < 4 {
			m.showBanner("usage: /mcp auth <server_id> <token>", "error")
			return m, nil
		}
		err := mcp.SaveAuth(m.cwd, mcp.AuthState{
			ServerID:  fields[2],
			Token:     fields[3],
			UpdatedAt: time.Now(),
		})
		if err != nil {
			m.showBanner("mcp auth failed: "+err.Error(), "error")
			return m, nil
		}
		m.showBanner("mcp auth updated", "success")
	default:
		m.showBanner("usage: /mcp <list|read|auth>", "info")
	}
	return m, nil
}

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

func (m Model) handlePlanCommand(input string) (tea.Model, tea.Cmd) {
	task := strings.TrimSpace(strings.TrimPrefix(input, "/plan"))
	if task == "" {
		m.mode = "plan"
		m.persistUIState()
		m.showBanner("switched to plan mode", "success")
		m.publishRemoteState("mode_change")
		return m, nil
	}
	spec, ok := m.manifest.AgentByID("plan")
	if !ok {
		m.showBanner("plan agent not found", "error")
		return m, nil
	}
	return m.runAgent(spec, task, nil, nil)
}

func (m Model) handlePermissionsCommand(input string) (tea.Model, tea.Cmd) {
	fields := strings.Fields(input)
	if len(fields) == 1 {
		m.pushSystemMsg(m.permissionSummary())
		return m, nil
	}
	if strings.EqualFold(fields[1], "debug") {
		if len(fields) == 2 {
			if m.cfg.ShowPermissionDebug {
				m.showBanner("permission debug: on", "info")
			} else {
				m.showBanner("permission debug: off", "info")
			}
			return m, nil
		}
		switch strings.ToLower(fields[2]) {
		case "on":
			m.cfg.ShowPermissionDebug = true
		case "off":
			m.cfg.ShowPermissionDebug = false
		default:
			m.showBanner("usage: /permissions debug <on|off>", "error")
			return m, nil
		}
		_ = m.updateConfig(func(cfg *config.UserConfig) error {
			cfg.ShowPermissionDebug = m.cfg.ShowPermissionDebug
			return nil
		})
		if m.cfg.ShowPermissionDebug {
			m.showBanner("permission debug enabled", "success")
		} else {
			m.showBanner("permission debug disabled", "success")
		}
		return m, nil
	}
	if len(fields) != 2 {
		m.showBanner("usage: /permissions <yolo|restricted|ask-first> | /permissions debug <on|off>", "error")
		return m, nil
	}
	level := config.PermissionLevel(fields[1])
	switch level {
	case config.PermissionYOLO, config.PermissionRestricted, config.PermissionAskFirst:
		_ = m.updateConfig(func(cfg *config.UserConfig) error {
			cfg.Permission = level
			return nil
		})
		m.showBanner(fmt.Sprintf("permission set to %s", level), "success")
	default:
		m.showBanner("invalid permission", "error")
	}
	return m, nil
}

func (m Model) handleHooksCommand() (tea.Model, tea.Cmd) {
	cfg, err := hooks.LoadEffective(m.cwd)
	if err != nil {
		m.showBanner("hooks load failed: "+err.Error(), "error")
		return m, nil
	}
	if len(cfg.Rules) == 0 {
		m.pushSystemMsg("no hooks configured (project: .spettro/hooks.json, global: ~/.spettro/hooks.json)")
		return m, nil
	}
	var rows []string
	for _, r := range cfg.Rules {
		status := "enabled"
		if !r.Enabled {
			status = "disabled"
		}
		matcher := strings.TrimSpace(r.Matcher)
		if matcher == "" {
			matcher = "*"
		}
		rows = append(rows, fmt.Sprintf("- [%s] %s id=%s matcher=%s source=%s cmd=%q", status, r.Event, r.ID, matcher, r.Source, r.Command))
	}
	if len(cfg.Issues) > 0 {
		rows = append(rows, "", "validation warnings:")
		for _, issue := range cfg.Issues {
			rows = append(rows, fmt.Sprintf("- [%s] %s: %s", issue.Source, issue.ID, issue.Message))
		}
	}
	m.pushSystemMsg("hooks:\n" + strings.Join(rows, "\n"))
	return m, nil
}

func (m Model) permissionSummary() string {
	var rows []string
	rows = append(rows, fmt.Sprintf("current permission: %s", m.cfg.Permission))
	if m.cfg.ShowPermissionDebug {
		rows = append(rows, "permission debug: on")
	} else {
		rows = append(rows, "permission debug: off")
	}
	rows = append(rows, fmt.Sprintf("runtime permission rules: %d", len(m.manifest.Runtime.PermissionRules)))
	if spec, ok := m.manifest.AgentByID(m.mode); ok {
		rows = append(rows, fmt.Sprintf("agent %s rules: %d", spec.ID, len(spec.PermissionRules)))
	}
	if len(m.recentApprovals) == 0 {
		rows = append(rows, "recent approvals: none")
		return strings.Join(rows, "\n")
	}
	rows = append(rows, "", "recent approvals:")
	for i := len(m.recentApprovals) - 1; i >= 0 && i >= len(m.recentApprovals)-5; i-- {
		ev := m.recentApprovals[i]
		seg := strings.TrimSpace(ev.CommandSegment)
		if seg == "" {
			seg = strings.TrimSpace(ev.Task)
		}
		if seg == "" {
			seg = "(unknown command)"
		}
		rows = append(rows, fmt.Sprintf("- %s [%s] via %s: %s", ev.Decision, ev.ToolID, ev.DecisionSource, truncateLabel(seg, 90)))
	}
	return strings.Join(rows, "\n")
}

func (m Model) handleCompactCommand(input string) (tea.Model, tea.Cmd) {
	fields := strings.Fields(input)
	if len(fields) == 1 {
		return m.runCompact("")
	}
	if len(fields) >= 2 && strings.EqualFold(fields[1], "auto") {
		if len(fields) == 2 || strings.EqualFold(fields[2], "status") {
			state := "off"
			if m.cfg.AutoCompactEnabled {
				state = "on"
			}
			m.showBanner(fmt.Sprintf("auto compact: %s (failures: %d/%d)", state, m.autoCompactFailures, m.cfg.AutoCompactMaxFailures), "info")
			return m, nil
		}
		switch strings.ToLower(fields[2]) {
		case "on":
			m.cfg.AutoCompactEnabled = true
			m.autoCompactFailures = 0
			_ = m.updateConfig(func(cfg *config.UserConfig) error {
				cfg.AutoCompactEnabled = true
				return nil
			})
			m.showBanner("auto compact enabled", "success")
		case "off":
			m.cfg.AutoCompactEnabled = false
			_ = m.updateConfig(func(cfg *config.UserConfig) error {
				cfg.AutoCompactEnabled = false
				return nil
			})
			m.showBanner("auto compact disabled", "success")
		default:
			m.showBanner("usage: /compact auto <status|on|off>", "error")
		}
		return m, nil
	}
	if len(fields) >= 2 && strings.EqualFold(fields[1], "policy") {
		window := m.contextWindow()
		if window == 0 {
			window = contextWindowDefault(m.cfg.ActiveProvider)
		}
		eval := m.evaluateCompact()
		reason := strings.TrimSpace(eval.AutoDisabledReason)
		if reason == "" {
			reason = "none"
		}
		m.pushSystemMsg(fmt.Sprintf(
			"compact policy:\n- context window: %d\n- effective window: %d\n- warning threshold: %d\n- error threshold: %d\n- auto threshold: %d\n- blocking limit: %d\n- auto enabled: %t\n- consecutive failures: %d/%d\n- auto disabled reason: %s",
			window,
			eval.EffectiveWindow,
			eval.WarningThreshold,
			eval.ErrorThreshold,
			eval.AutoCompactThreshold,
			eval.BlockingLimit,
			m.cfg.AutoCompactEnabled,
			m.autoCompactFailures,
			m.cfg.AutoCompactMaxFailures,
			reason,
		))
		return m, nil
	}
	focus := strings.TrimSpace(strings.TrimPrefix(input, fields[0]))
	return m.runCompact(focus)
}
