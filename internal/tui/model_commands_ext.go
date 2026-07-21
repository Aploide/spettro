package tui

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"spettro/internal/config"
	"spettro/internal/hooks"
	"spettro/internal/jobs"
	"spettro/internal/mcp"
	"spettro/internal/memory"
	"spettro/internal/session"
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
		// Render in dependency order with blocked derivation so the graph
		// state is readable straight from /tasks.
		byID := make(map[string]session.Todo, len(m.todos))
		for _, t := range m.todos {
			byID[t.ID] = t
		}
		blocked := session.BlockedIDs(m.todos)
		var rows []string
		for _, id := range session.TopoOrder(m.todos) {
			t := byID[id]
			row := fmt.Sprintf("- [%s] %s (%s)", t.Status, t.Content, t.ID)
			if len(t.Dependencies) > 0 {
				row += " deps: " + strings.Join(t.Dependencies, ", ")
			}
			if _, ok := blocked[t.ID]; ok && t.Status != "blocked" {
				row += " [blocked]"
			}
			rows = append(rows, row)
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
		// ID is minted by UpsertTodo (collision-free against the stored list).
		item := session.Todo{
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
		id := strings.TrimSpace(fields[2])
		st, err := session.NormalizeTaskStatus(fields[3])
		if err != nil {
			m.showBanner("tasks set failed: "+err.Error(), "error")
			return m, nil
		}
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
	case "rm", "delete":
		if len(fields) < 3 {
			m.showBanner("usage: /tasks rm <id>", "error")
			return m, nil
		}
		id := strings.TrimSpace(fields[2])
		found, err := session.DeleteTodo(globalDir, m.sessionID, id)
		if err != nil {
			m.showBanner("tasks rm failed: "+err.Error(), "error")
			return m, nil
		}
		if !found {
			m.showBanner("task not found: "+id, "error")
			return m, nil
		}
		m.syncTodosFromSession()
		m.showBanner("task deleted", "success")
	case "clear":
		n, err := session.ClearCompletedTodos(globalDir, m.sessionID)
		if err != nil {
			m.showBanner("tasks clear failed: "+err.Error(), "error")
			return m, nil
		}
		m.syncTodosFromSession()
		m.showBanner(fmt.Sprintf("removed %d completed/cancelled tasks", n), "success")
	default:
		m.showBanner("usage: /tasks [list|add|done|set|show|rm|clear]", "info")
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

// handleJobsCommand lists and kills background shell jobs started by the
// agent (bash run_in_background). /jobs, /jobs kill <id>, /jobs kill all.
func (m Model) handleJobsCommand(input string) (tea.Model, tea.Cmd) {
	mgr := jobs.Default()
	fields := strings.Fields(input)
	if len(fields) == 1 || strings.EqualFold(fields[1], "list") {
		list := mgr.List()
		if len(list) == 0 {
			m.pushSystemMsg("no background jobs in this session")
			return m, nil
		}
		var rows []string
		for _, j := range list {
			status := "running"
			if !j.Running() {
				status = "exited"
			}
			rows = append(rows, fmt.Sprintf("- %s [%s] %s (started %s ago)", j.ID, status, truncateLabel(j.Command, 60), time.Since(j.Started).Round(time.Second)))
		}
		m.pushSystemMsg("background jobs:\n" + strings.Join(rows, "\n") + "\n\nkill with /jobs kill <id> or /jobs kill all")
		m.refreshViewport()
		return m, nil
	}
	if strings.EqualFold(fields[1], "kill") {
		if len(fields) < 3 {
			m.showBanner("usage: /jobs kill <id>|all", "error")
			return m, nil
		}
		target := fields[2]
		if strings.EqualFold(target, "all") {
			n := mgr.RunningCount()
			mgr.KillAll()
			m.showBanner(fmt.Sprintf("killed %d background job(s)", n), "success")
			return m, nil
		}
		if err := mgr.Kill(target); err != nil {
			m.showBanner("jobs kill failed: "+err.Error(), "error")
			return m, nil
		}
		m.showBanner("killed "+target, "success")
		return m, nil
	}
	m.showBanner("usage: /jobs [list] | /jobs kill <id>|all", "error")
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

// handleMemoryCommand implements /memory show|edit|clear over the persistent
// cross-session memory files (user: ~/.spettro/memory.md, project:
// <root>/.spettro/memory.md). Changes take effect at the next session start —
// the running session's context snapshot is frozen for prompt-cache stability.
func (m Model) handleMemoryCommand(input string) (tea.Model, tea.Cmd) {
	store := memory.DefaultStore(m.cwd)
	fields := strings.Fields(input)
	sub := "show"
	if len(fields) >= 2 {
		sub = strings.ToLower(fields[1])
	}
	scopeArg := ""
	if len(fields) >= 3 {
		scopeArg = strings.ToLower(fields[2])
	}
	switch sub {
	case "show":
		var rows []string
		for _, sc := range []memory.Scope{memory.ScopeUser, memory.ScopeProject} {
			path := store.Path(sc)
			if path == "" {
				continue
			}
			data, err := os.ReadFile(path)
			content := strings.TrimSpace(string(data))
			if err != nil || content == "" {
				rows = append(rows, fmt.Sprintf("%s memory (%s): empty", sc, path))
				continue
			}
			rows = append(rows, fmt.Sprintf("%s memory (%s):\n%s", sc, path, content))
		}
		if cands, err := memory.DefaultInbox().Load(); err == nil && len(cands) > 0 {
			rows = append(rows, "", fmt.Sprintf("%d candidate(s) pending in the review inbox — /memory review", len(cands)))
		}
		rows = append(rows, "", "changes take effect at the next session start")
		m.pushSystemMsg(strings.Join(rows, "\n"))
		m.refreshViewport()
		return m, nil
	case "mine":
		limit := 0
		if scopeArg != "" {
			if _, err := fmt.Sscanf(scopeArg, "%d", &limit); err != nil || limit <= 0 {
				m.showBanner("usage: /memory mine [max-sessions]", "error")
				return m, nil
			}
		}
		return m.runMemoryMine(limit)
	case "review":
		return m.openMemoryReview()
	case "edit":
		scope := memory.ScopeUser
		if scopeArg == "project" {
			scope = memory.ScopeProject
		} else if scopeArg != "" && scopeArg != "user" {
			m.showBanner("usage: /memory edit [user|project]", "error")
			return m, nil
		}
		path := store.Path(scope)
		if path == "" {
			m.showBanner("no "+string(scope)+" memory file available", "error")
			return m, nil
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			m.showBanner("memory edit failed: "+err.Error(), "error")
			return m, nil
		}
		editor := strings.TrimSpace(os.Getenv("EDITOR"))
		if editor == "" {
			editor = "vi"
		}
		parts := strings.Fields(editor)
		parts = append(parts, path)
		c := exec.Command(parts[0], parts[1:]...)
		return m, tea.ExecProcess(c, func(err error) tea.Msg {
			return memoryEditDoneMsg{err: err}
		})
	case "clear":
		scopes := []memory.Scope{memory.ScopeUser, memory.ScopeProject}
		switch scopeArg {
		case "", "all":
		case "user":
			scopes = []memory.Scope{memory.ScopeUser}
		case "project":
			scopes = []memory.Scope{memory.ScopeProject}
		default:
			m.showBanner("usage: /memory clear [user|project|all]", "error")
			return m, nil
		}
		for _, sc := range scopes {
			if err := store.Clear(sc); err != nil {
				m.showBanner("memory clear failed: "+err.Error(), "error")
				return m, nil
			}
		}
		m.showBanner("memory cleared — applies from the next session", "success")
		return m, nil
	}
	m.showBanner("usage: /memory [show] | /memory edit [user|project] | /memory clear [user|project|all] | /memory mine [n] | /memory review", "error")
	return m, nil
}
