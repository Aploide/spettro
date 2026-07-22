package acp

// Extended slash-command surface for ACP clients: the read-only, text-
// resolvable commands the TUI offers (/stats, /tasks, /jobs, /hooks, /diff,
// /plan, /permissions, /ultra) so GUI clients driving the binary over ACP
// reach feature parity with the interactive CLI without reimplementing any
// of it. Everything here mirrors the TUI implementations in internal/tui
// (model_commands_ext.go, model_stats.go, model_state.go).

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"spettro/internal/config"
	"spettro/internal/hooks"
	"spettro/internal/jobs"
	"spettro/internal/provider"
	"spettro/internal/session"
)

// handleExtendedSlashCommand executes the second tier of slash commands
// (anything beyond the core set in commands.go). handled=false means fall
// through (custom commands, or a plain prompt).
func handleExtendedSlashCommand(b *bridge, s *acpSession, cfg *config.UserConfig, pm *provider.Manager, input string) (reply string, modeChanged bool, handled bool) {
	fields := strings.Fields(input)
	if len(fields) == 0 {
		return "", false, false
	}
	switch strings.ToLower(fields[0]) {
	case "/stats":
		return acpStatsText(pm), false, true

	case "/tasks":
		return acpTasksText(b, s, input, fields), false, true

	case "/jobs":
		return acpJobsText(fields), false, true

	case "/hooks":
		return acpHooksText(s.cwd), false, true

	case "/diff":
		return acpDiffText(s.cwd, fields), false, true

	case "/ultra":
		return acpUltraText(cfg, fields), false, true

	case "/plan":
		if len(fields) == 1 {
			if _, ok := s.manifest.AgentByID("plan"); !ok {
				return "plan agent not found in manifest", false, true
			}
			s.agentID = "plan"
			return "switched to plan mode", true, true
		}
		// /plan <task> runs the plan agent on the task: the caller falls
		// through and runs it as a normal prompt turn in plan mode.
		return "", false, false

	case "/permissions":
		return acpPermissionsText(s, cfg, fields), false, true

	case "/init":
		return "over ACP /init runs as a normal agent turn: just ask \"analyze this codebase and write SPETTRO.md\" — or run `spettro` in a terminal and use /init there", false, true

	case "/approve":
		// Plan approval is implicit over ACP: sending the plan as a prompt in
		// coding mode executes it; there is no separate pending-plan buffer.
		return "over ACP there is no pending-plan buffer: switch to coding mode and send the plan text as a prompt to execute it", false, true
	}
	return "", false, false
}

// acpStatsText renders the /stats report from the provider manager's
// accumulated session usage (mirrors internal/tui/model_stats.go).
func acpStatsText(pm *provider.Manager) string {
	snap := pm.UsageSnapshot()
	if snap.Totals.Requests == 0 {
		return "no LLM requests recorded in this session yet"
	}
	t := snap.Totals

	hitRate := func(u provider.Usage) string {
		r := u.CacheHitRate()
		if r < 0 {
			return "n/a"
		}
		return fmt.Sprintf("%.0f%%", r*100)
	}
	plural := func(n int, w string) string {
		if n == 1 {
			return fmt.Sprintf("%d %s", n, w)
		}
		return fmt.Sprintf("%d %ss", n, w)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "session usage — %s\n\n", plural(t.Requests, "request"))
	fmt.Fprintf(&b, "  input        %7s\n", formatACPCount(t.InputTokens))
	fmt.Fprintf(&b, "  output       %7s\n", formatACPCount(t.OutputTokens))
	fmt.Fprintf(&b, "  cache read   %7s\n", formatACPCount(t.CacheReadTokens))
	fmt.Fprintf(&b, "  cache write  %7s\n\n", formatACPCount(t.CacheWriteTokens))
	fmt.Fprintf(&b, "  cache hits   %5s  session\n", hitRate(t.Usage))
	fmt.Fprintf(&b, "               %5s  last request\n", hitRate(snap.Last))

	if len(snap.ByModel) > 0 {
		keys := make([]string, 0, len(snap.ByModel))
		w := 0
		for k := range snap.ByModel {
			keys = append(keys, k)
			if len(k) > w {
				w = len(k)
			}
		}
		sort.Strings(keys)
		b.WriteString("\n  by model\n")
		for _, k := range keys {
			u := snap.ByModel[k]
			fmt.Fprintf(&b, "  %-*s  %s · %s hit · in %s · out %s\n",
				w, k, plural(u.Requests, "req"), hitRate(u.Usage),
				formatACPCount(u.InputTokens+u.CacheReadTokens+u.CacheWriteTokens),
				formatACPCount(u.OutputTokens))
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

func formatACPCount(n int) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	case n >= 1_000:
		return fmt.Sprintf("%.1fk", float64(n)/1_000)
	default:
		return fmt.Sprintf("%d", n)
	}
}

// acpTasksText mirrors the TUI's /tasks command over the shared session todo
// store (list/add/done/set/show/rm/clear).
func acpTasksText(b *bridge, s *acpSession, input string, fields []string) string {
	globalDir := b.opts.GlobalDir
	sid := s.id

	if len(fields) == 1 || strings.EqualFold(fields[1], "list") {
		todos, err := session.LoadTodos(globalDir, sid)
		if err != nil || len(todos) == 0 {
			return "no tasks in this session"
		}
		byID := make(map[string]session.Todo, len(todos))
		for _, t := range todos {
			byID[t.ID] = t
		}
		blocked := session.BlockedIDs(todos)
		var rows []string
		for _, id := range session.TopoOrder(todos) {
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
		return "tasks:\n" + strings.Join(rows, "\n")
	}

	switch strings.ToLower(fields[1]) {
	case "add":
		content := strings.TrimSpace(strings.TrimPrefix(input, "/tasks add"))
		if content == "" {
			return "usage: /tasks add <content>"
		}
		item := session.Todo{Content: content, Status: "pending", Source: "command"}
		if _, err := session.UpsertTodo(globalDir, sid, item); err != nil {
			return "tasks add failed: " + err.Error()
		}
		return "task added"
	case "done":
		if len(fields) < 3 {
			return "usage: /tasks done <id>"
		}
		item, ok, err := session.GetTodo(globalDir, sid, fields[2])
		if err != nil {
			return "tasks done failed: " + err.Error()
		}
		if !ok {
			return "task not found: " + fields[2]
		}
		item.Status = "completed"
		if _, err := session.UpsertTodo(globalDir, sid, item); err != nil {
			return "tasks done failed: " + err.Error()
		}
		return "task marked completed"
	case "set":
		if len(fields) < 4 {
			return "usage: /tasks set <id> <status>"
		}
		st, err := session.NormalizeTaskStatus(fields[3])
		if err != nil {
			return "tasks set failed: " + err.Error()
		}
		item, ok, err := session.GetTodo(globalDir, sid, fields[2])
		if err != nil {
			return "tasks set failed: " + err.Error()
		}
		if !ok {
			return "task not found: " + fields[2]
		}
		item.Status = st
		if _, err := session.UpsertTodo(globalDir, sid, item); err != nil {
			return "tasks set failed: " + err.Error()
		}
		return "task updated"
	case "show":
		if len(fields) < 3 {
			return "usage: /tasks show <id>"
		}
		item, ok, err := session.GetTodo(globalDir, sid, fields[2])
		if err != nil {
			return "tasks show failed: " + err.Error()
		}
		if !ok {
			return "task not found: " + fields[2]
		}
		raw, _ := json.MarshalIndent(item, "", "  ")
		return string(raw)
	case "rm", "delete":
		if len(fields) < 3 {
			return "usage: /tasks rm <id>"
		}
		found, err := session.DeleteTodo(globalDir, sid, fields[2])
		if err != nil {
			return "tasks rm failed: " + err.Error()
		}
		if !found {
			return "task not found: " + fields[2]
		}
		return "task deleted"
	case "clear":
		n, err := session.ClearCompletedTodos(globalDir, sid)
		if err != nil {
			return "tasks clear failed: " + err.Error()
		}
		return fmt.Sprintf("removed %d completed/cancelled tasks", n)
	}
	return "usage: /tasks [list|add|done|set|show|rm|clear]"
}

// acpJobsText mirrors /jobs over the shared background job registry.
func acpJobsText(fields []string) string {
	mgr := jobs.Default()
	if len(fields) == 1 || strings.EqualFold(fields[1], "list") {
		list := mgr.List()
		if len(list) == 0 {
			return "no background jobs in this session"
		}
		var rows []string
		for _, j := range list {
			status := "running"
			if !j.Running() {
				status = "exited"
			}
			cmd := strings.Join(strings.Fields(j.Command), " ")
			if len(cmd) > 60 {
				cmd = cmd[:60] + "…"
			}
			rows = append(rows, fmt.Sprintf("- %s [%s] %s (started %s ago)", j.ID, status, cmd, time.Since(j.Started).Round(time.Second)))
		}
		return "background jobs:\n" + strings.Join(rows, "\n") + "\n\nkill with /jobs kill <id> or /jobs kill all"
	}
	if strings.EqualFold(fields[1], "kill") {
		if len(fields) < 3 {
			return "usage: /jobs kill <id>|all"
		}
		target := fields[2]
		if strings.EqualFold(target, "all") {
			n := mgr.RunningCount()
			mgr.KillAll()
			return fmt.Sprintf("killed %d background job(s)", n)
		}
		if err := mgr.Kill(target); err != nil {
			return "jobs kill failed: " + err.Error()
		}
		return "killed " + target
	}
	return "usage: /jobs [list] | /jobs kill <id>|all"
}

// acpHooksText mirrors /hooks: list the effective runtime hooks.
func acpHooksText(cwd string) string {
	cfg, err := hooks.LoadEffective(cwd)
	if err != nil {
		return "hooks load failed: " + err.Error()
	}
	if len(cfg.Rules) == 0 {
		return "no hooks configured (project: .spettro/hooks.json, global: ~/.spettro/hooks.json)"
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
	return "hooks:\n" + strings.Join(rows, "\n")
}

// acpDiffText mirrors /diff: unified diffs of files modified in the working
// tree (or the given paths), per git.
func acpDiffText(cwd string, fields []string) string {
	var paths []string
	if len(fields) > 1 {
		paths = fields[1:]
	} else {
		paths = gitModifiedPaths(cwd)
	}
	if len(paths) == 0 {
		return "no modified files in the working tree"
	}
	var parts []string
	for _, p := range paths {
		if d := gitPathDiff(cwd, p); strings.TrimSpace(d) != "" {
			parts = append(parts, strings.TrimRight(d, "\n"))
		}
	}
	if len(parts) == 0 {
		return "no diffs to show"
	}
	return strings.Join(parts, "\n")
}

// gitModifiedPaths lists working-tree paths with uncommitted changes
// (mirrors the TUI's queryModifiedFiles, minus the numstat cosmetics).
func gitModifiedPaths(cwd string) []string {
	cmd := exec.Command("git", "status", "--porcelain")
	cmd.Dir = cwd
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	var paths []string
	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimRight(line, "\r")
		if len(line) < 4 {
			continue
		}
		path := strings.TrimSpace(line[3:])
		if strings.Contains(path, " -> ") {
			segs := strings.Split(path, " -> ")
			path = strings.TrimSpace(segs[len(segs)-1])
		}
		path = strings.Trim(path, "\"")
		if path != "" {
			paths = append(paths, path)
		}
	}
	sort.Strings(paths)
	return paths
}

// gitPathDiff renders the diff of one path (tracked vs HEAD, staged, or a
// synthetic all-additions diff for untracked files) — same algorithm as the
// TUI's gitPathDiff.
func gitPathDiff(cwd, path string) string {
	cmd := exec.Command("git", "diff", "HEAD", "--", path)
	cmd.Dir = cwd
	if out, err := cmd.Output(); err == nil && len(strings.TrimSpace(string(out))) > 0 {
		return string(out)
	}
	cmd2 := exec.Command("git", "diff", "--cached", "--", path)
	cmd2.Dir = cwd
	if out2, err2 := cmd2.Output(); err2 == nil && len(strings.TrimSpace(string(out2))) > 0 {
		return string(out2)
	}
	absPath := path
	if !filepath.IsAbs(path) {
		absPath = filepath.Join(cwd, path)
	}
	content, err := exec.Command("git", "ls-files", "--others", "--exclude-standard", "--", absPath).Output()
	if err != nil || len(strings.TrimSpace(string(content))) == 0 {
		return ""
	}
	data, readErr := os.ReadFile(absPath)
	if readErr != nil {
		return ""
	}
	lines := strings.Split(string(data), "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "--- /dev/null\n+++ b/%s\n@@ -0,0 +1,%d @@\n", path, len(lines))
	for _, l := range lines {
		sb.WriteByte('+')
		sb.WriteString(l)
		sb.WriteByte('\n')
	}
	return sb.String()
}

// acpUltraText mirrors /ultra: toggle the Ultra swarm mode.
func acpUltraText(cfg *config.UserConfig, fields []string) string {
	next := !cfg.Ultra
	if len(fields) >= 2 {
		switch strings.ToLower(strings.TrimSpace(fields[1])) {
		case "on":
			next = true
		case "off":
			next = false
		default:
			return "usage: /ultra [on|off]"
		}
	}
	// A swarm runs many sub-agents concurrently; per-action approval prompts
	// would flood the user, so Ultra requires restricted or yolo.
	if next && cfg.Permission == config.PermissionAskFirst {
		return "ultra needs restricted or yolo permission — switch first with /permission"
	}
	if _, err := config.Update(func(c *config.UserConfig) error {
		c.Ultra = next
		return nil
	}); err != nil {
		return "error: " + err.Error()
	}
	cfg.Ultra = next
	if next {
		return "ultra on — hard tasks fan out across a swarm of parallel sub-agents"
	}
	return "ultra off"
}

// acpPermissionsText mirrors /permissions: show the permission summary, set
// the level, or toggle debug output.
func acpPermissionsText(s *acpSession, cfg *config.UserConfig, fields []string) string {
	if len(fields) == 1 {
		var rows []string
		rows = append(rows, fmt.Sprintf("current permission: %s", cfg.Permission))
		if cfg.ShowPermissionDebug {
			rows = append(rows, "permission debug: on")
		} else {
			rows = append(rows, "permission debug: off")
		}
		rows = append(rows, fmt.Sprintf("runtime permission rules: %d", len(s.manifest.Runtime.PermissionRules)))
		if spec, ok := s.manifest.AgentByID(s.agentID); ok {
			rows = append(rows, fmt.Sprintf("agent %s rules: %d", spec.ID, len(spec.PermissionRules)))
		}
		return strings.Join(rows, "\n")
	}
	if strings.EqualFold(fields[1], "debug") {
		if len(fields) == 2 {
			if cfg.ShowPermissionDebug {
				return "permission debug: on"
			}
			return "permission debug: off"
		}
		var on bool
		switch strings.ToLower(fields[2]) {
		case "on":
			on = true
		case "off":
			on = false
		default:
			return "usage: /permissions debug <on|off>"
		}
		if _, err := config.Update(func(c *config.UserConfig) error {
			c.ShowPermissionDebug = on
			return nil
		}); err != nil {
			return "error: " + err.Error()
		}
		cfg.ShowPermissionDebug = on
		if on {
			return "permission debug enabled"
		}
		return "permission debug disabled"
	}
	if len(fields) != 2 {
		return "usage: /permissions <yolo|restricted|ask-first> | /permissions debug <on|off>"
	}
	level := config.PermissionLevel(fields[1])
	switch level {
	case config.PermissionYOLO, config.PermissionRestricted, config.PermissionAskFirst:
	default:
		return "invalid permission: use yolo, restricted, or ask-first"
	}
	if _, err := config.Update(func(c *config.UserConfig) error {
		c.Permission = level
		return nil
	}); err != nil {
		return "error: " + err.Error()
	}
	cfg.Permission = level
	// Live update: an in-flight run reads s.permission before every approval
	// decision, so the new level applies to the current turn too.
	s.permission = level
	return "permission set to " + string(level)
}
