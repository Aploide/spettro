package acp

// Extended slash-command surface for ACP clients: the read-only, text-
// resolvable commands the TUI offers (/stats, /tasks, /jobs, /hooks, /diff,
// /plan, /permissions, /ultra, /workflows, /workflow-size) so GUI
// clients driving the binary reach feature parity with the interactive CLI
// without reimplementing any of it. Everything here mirrors the TUI implementations in internal/tui
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
	"unicode"

	"spettro/internal/config"
	"spettro/internal/hooks"
	"spettro/internal/jobs"
	"spettro/internal/provider"
	"spettro/internal/session"
	"spettro/internal/workflow"
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

	case "/skills":
		return skillsText(s.cwd, *cfg), false, true

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

	case "/ultracode":
		// Not a command — /ultra is the switch. Falling through sent
		// "/ultracode off" to the model as a task, and the keyword in it
		// switched the standing mode ON for that turn: the opposite of what
		// was asked. Answer it here instead, as the TUI does.
		return "unknown command: /ultracode — use /ultra [on|off], or write ultracode in a message to use workflows for that one turn", false, true

	case "/workflow-size":
		return acpWorkflowSizeText(cfg, fields[1:]), false, true

	case "/workflows", "/workflow":
		// "run" is the one subcommand that needs a turn, and only when the
		// named workflow actually resolves — the bridge has already rewritten
		// it into a prompt in that case. Falling through unconditionally would
		// hand "/workflows run nope" to the model as a plain request and send
		// it hunting for a file that does not exist.
		if len(fields) >= 2 {
			switch strings.ToLower(fields[1]) {
			case "run", "start":
				if len(fields) < 3 {
					return "usage: /workflows run <name> [json | task description]", false, true
				}
				if _, _, err := workflow.Load(s.cwd, fields[2]); err != nil {
					return err.Error(), false, true
				}
				return "", false, false
			case "size":
				// The TUI's spelling of /workflow-size, accepted here too so
				// the same muscle memory works in an editor.
				return acpWorkflowSizeText(cfg, fields[2:]), false, true
			}
		}
		return acpWorkflowsText(s.cwd, fields), false, true

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

// acpUltraText mirrors /ultra: toggle the persisted ultracode standing mode
// (cfg.Ultra), in which every substantive task runs as a dynamic workflow, as
// if each message said "ultracode". No argument flips it.
//
// Turning it on under ask-first is allowed rather than refused: the setting
// is shared with every session and a TUI alongside, and the user may simply
// switch permission next. UltraActive keeps it suspended while ask-first is
// selected (workflows cannot run with a prompt per action), so the reply says
// so instead of claiming a mode that is not in effect.
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
	if _, err := config.Update(func(c *config.UserConfig) error {
		c.Ultra = next
		return nil
	}); err != nil {
		return "error: " + err.Error()
	}
	cfg.Ultra = next
	if !next {
		return "ultra off — workflows run only when a message says \"ultracode\""
	}
	if !cfg.UltraActive() {
		return "ultra on (saved) — suspended while permission is ask-first: " +
			"workflows need restricted or yolo, switch with /permission"
	}
	return "ultra on — ultracode: substantive tasks run as dynamic workflows " +
		"(understand → design → implement → review), as if each message said \"ultracode\""
}

// acpWorkflowSizeText mirrors /workflows size (TUI): with no argument it shows
// the current tier and what each means; with one it persists the new tier,
// which every session and a TUI alongside then plan their workflows around.
func acpWorkflowSizeText(cfg *config.UserConfig, args []string) string {
	if len(args) == 0 {
		var b strings.Builder
		fmt.Fprintf(&b, "workflow size: %s\n", cfg.WorkflowSizeTier())
		for _, tier := range config.WorkflowSizes {
			marker := " "
			if tier == cfg.WorkflowSizeTier() {
				marker = "*"
			}
			fmt.Fprintf(&b, " %s %-10s %s\n", marker, tier, workflowSizeSummary(tier))
		}
		b.WriteString("a guideline the agent plans scripts around, not a hard cap\n")
		b.WriteString("usage: /workflow-size <" + strings.Join(config.WorkflowSizes, "|") + ">")
		return b.String()
	}
	tier, ok := validWorkflowSize(args[0])
	if !ok || len(args) > 1 {
		return "usage: /workflow-size <" + strings.Join(config.WorkflowSizes, "|") + ">"
	}
	if _, err := config.Update(func(c *config.UserConfig) error {
		c.WorkflowSize = tier
		return nil
	}); err != nil {
		return "error: " + err.Error()
	}
	cfg.WorkflowSize = tier
	return "workflow size set to " + tier + " — " + workflowSizeSummary(tier)
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

// acpWorkflowsText mirrors /workflows for its read-only subcommands: list the
// saved scripts, show one, or report where they are looked up.
func acpWorkflowsText(cwd string, fields []string) string {
	sub := "list"
	if len(fields) >= 2 {
		sub = strings.ToLower(fields[1])
	}
	switch sub {
	case "where", "paths":
		var b strings.Builder
		b.WriteString("workflow search paths (first match wins):\n")
		for _, p := range workflow.SearchPaths(cwd) {
			b.WriteString("  " + p + "\n")
		}
		return strings.TrimRight(b.String(), "\n")
	case "show", "cat", "info":
		if len(fields) < 3 {
			return "usage: /workflows show <name>"
		}
		script, path, err := workflow.Load(cwd, fields[2])
		if err != nil {
			return err.Error()
		}
		return path + "\n\n```javascript\n" + strings.TrimRight(script, "\n") + "\n```"
	case "list", "ls":
		saved := workflow.Discover(cwd)
		if len(saved) == 0 {
			return "no saved workflows — add scripts to .spettro/workflows/<name>.js or ~/.spettro/workflows/<name>.js.\n" +
				"Write \"ultracode\" in a message to give the agent the workflow tool for that turn, or /ultra on for every turn."
		}
		var b strings.Builder
		fmt.Fprintf(&b, "saved workflows (%d):\n", len(saved))
		for _, s := range saved {
			if s.Err != nil {
				fmt.Fprintf(&b, "  %-24s [%s] header does not parse: %v\n", s.Name, s.Scope, s.Err)
				continue
			}
			fmt.Fprintf(&b, "  %-24s [%s] %s\n", s.Name, s.Scope, s.Meta.Description)
		}
		b.WriteString("\nrun one with /workflows run <name> [json | task]: the agent adapts it to the task as a template")
		return strings.TrimRight(b.String(), "\n")
	default:
		return "usage: /workflows [list|show <name>|run <name> [json | task]|size [tier]|where]"
	}
}

// acpWorkflowRunPrompt rewrites "/workflows run <name> [json | task]" into the
// plain request that makes the agent run that saved workflow. ok is false for
// anything else, including a run of a workflow that does not exist — the
// error then travels the normal command path instead of becoming a prompt.
//
// A saved workflow is a template, not a recording: the prompt has the agent
// read it, check it fits, and adapt whatever is task-specific or stale before
// running it — or run it by name when it fits as-is. Whatever follows the
// name is passed through untouched: valid JSON becomes the run's args, and
// anything else is the task the template is applied to. The remainder is cut
// from the raw input rather than re-joined from fields, which would collapse
// the whitespace inside JSON strings and the line breaks of a task.
func acpWorkflowRunPrompt(cwd, input string) (string, bool) {
	fields := strings.Fields(input)
	if len(fields) < 3 {
		return "", false
	}
	switch strings.ToLower(fields[0]) {
	case "/workflows", "/workflow":
	default:
		return "", false
	}
	switch strings.ToLower(fields[1]) {
	case "run", "start":
	default:
		return "", false
	}
	name := fields[2]
	script, path, err := workflow.Load(cwd, name)
	if err != nil {
		return "", false
	}
	meta, err := workflow.ParseMeta(script)
	if err != nil {
		return "", false
	}
	quoted, _ := json.Marshal(name)
	quotedPath, _ := json.Marshal(path)
	rest := restAfterFields(input, 3)
	var rawArgs, taskText string
	if rest != "" && json.Valid([]byte(rest)) {
		rawArgs = rest
	} else {
		taskText = rest
	}

	var b strings.Builder
	fmt.Fprintf(&b, "ultracode: run the saved workflow %s — %s.\n", quoted, meta.Description)
	// Read through the workflow tool's show argument, not the file tools: a
	// global template lives under ~/.spettro, out of the file tools' reach.
	fmt.Fprintf(&b, "It is a template, not a fixed script. Read the saved template (call the workflow tool with {\"name\": %s, \"show\": true}; it is %s) and check it fits", quoted, quotedPath)
	if taskText != "" {
		b.WriteString(" the task below")
	}
	b.WriteString("; adapt anything task-specific or stale (work-lists must be discovered at runtime, never replayed from a hardcoded list) " +
		"and run the adapted script inline, or run it by name with args if it fits as-is: call the workflow tool with {\"name\": ")
	b.Write(quoted)
	if rawArgs != "" {
		b.WriteString(", \"args\": " + rawArgs)
	}
	b.WriteString("}.\n")
	b.WriteString("Declared params: " + describeWorkflowParams(meta.Params) + "\n")
	if taskText != "" {
		b.WriteString("Task: " + taskText + "\n")
	}
	b.WriteString("Then review the result and act on it.")
	return b.String(), true
}

// describeWorkflowParams lists a template's declared params for the run
// prompt, so the agent knows what it may pass before opening the script.
func describeWorkflowParams(params []workflow.ParamMeta) string {
	if len(params) == 0 {
		return "none — anything task-specific is written into the script, so adapt it rather than replay it."
	}
	parts := make([]string, 0, len(params))
	for _, p := range params {
		var quals []string
		if p.Type != "" && p.Type != "any" {
			quals = append(quals, p.Type)
		}
		if p.Required {
			quals = append(quals, "required")
		}
		if p.Default != nil {
			if def, err := json.Marshal(p.Default); err == nil {
				quals = append(quals, "default "+string(def))
			}
		}
		part := p.Name
		if len(quals) > 0 {
			part += " (" + strings.Join(quals, ", ") + ")"
		}
		if p.Description != "" {
			part += " — " + p.Description
		}
		parts = append(parts, part)
	}
	return strings.Join(parts, "; ")
}

// restAfterFields returns input without its first n whitespace-separated
// fields, keeping the rest verbatim (trimmed only at the ends).
func restAfterFields(input string, n int) string {
	rest := strings.TrimLeftFunc(input, unicode.IsSpace)
	for range n {
		i := strings.IndexFunc(rest, unicode.IsSpace)
		if i < 0 {
			return ""
		}
		rest = strings.TrimLeftFunc(rest[i:], unicode.IsSpace)
	}
	return strings.TrimSpace(rest)
}
