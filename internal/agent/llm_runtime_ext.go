package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"spettro/internal/config"
	"spettro/internal/diff"
	"spettro/internal/mcp"
	"spettro/internal/provider"
	"spettro/internal/safeio"
	"spettro/internal/sandbox"
	"spettro/internal/session"
)

// todoWriteItem is one task in a todo-write call. dependencies is a pointer
// so a merge can tell "keep the stored list" (absent) from "clear it" ([]).
type todoWriteItem struct {
	ID           string    `json:"id"`
	Content      string    `json:"content"`
	Status       string    `json:"status"`
	Owner        string    `json:"owner"`
	Source       string    `json:"source"`
	Priority     string    `json:"priority"`
	Dependencies *[]string `json:"dependencies"`
}

// todoRow is one task as todo-write reports it: the stored fields without
// timestamps, plus the scheduling state derived from the graph.
type todoRow struct {
	ID           string   `json:"id"`
	Content      string   `json:"content"`
	Status       string   `json:"status"`
	Owner        string   `json:"owner,omitempty"`
	Source       string   `json:"source,omitempty"`
	Priority     string   `json:"priority,omitempty"`
	Dependencies []string `json:"dependencies,omitempty"`
	BlockedBy    []string `json:"blocked_by,omitempty"`
	Ready        bool     `json:"ready,omitempty"`
}

// runTodoWrite reads and edits the session task list: the one tool behind
// what used to be todo-write plus task-create/get/update/list/delete. todos
// replaces the whole list, or with merge inserts/updates tasks by ID (empty
// fields keep their stored value); delete and clear_completed remove tasks. A
// call with none of those only reads. Every call returns the full list in
// dependency order, so the model never needs a separate read.
//
// Sub-agents share the parent's session folder, so a worker's full replace
// would wipe the orchestrator's list: below the top level, todos always
// merge.
func (r *toolRuntime) runTodoWrite(rawArgs []byte) (string, error) {
	var args struct {
		Todos          *[]todoWriteItem `json:"todos"`
		Merge          flexBool         `json:"merge"`
		Delete         []string         `json:"delete"`
		ClearCompleted flexBool         `json:"clear_completed"`
	}
	if err := decodeJSONStrict(rawArgs, &args); err != nil {
		return "", fmt.Errorf("todo-write args: %w", err)
	}
	if strings.TrimSpace(r.sessionDir) == "" {
		return "", fmt.Errorf("todo-write requires an active session")
	}
	sid := filepath.Base(r.sessionDir)
	globalDir := filepath.Dir(filepath.Dir(r.sessionDir))
	var todos []session.Todo
	var notes []string
	if args.Todos == nil && len(args.Delete) == 0 && !args.ClearCompleted {
		loaded, err := session.LoadTodos(globalDir, sid)
		if err != nil {
			return "", fmt.Errorf("todo-write: %w", err)
		}
		todos = loaded
	} else {
		change := session.TodoChange{Delete: args.Delete, ClearCompleted: bool(args.ClearCompleted)}
		if args.Todos != nil {
			change.Replace = !bool(args.Merge)
			if change.Replace && r.delegationDepth > 0 {
				change.Replace = false
				notes = append(notes, "merged instead of replacing: sub-agents share the parent's task list")
			}
			for _, it := range *args.Todos {
				change.Todos = append(change.Todos, session.TodoPatch{
					ID:           it.ID,
					Content:      it.Content,
					Status:       it.Status,
					Owner:        it.Owner,
					Source:       it.Source,
					Priority:     it.Priority,
					Dependencies: it.Dependencies,
				})
			}
		}
		saved, applyNotes, err := session.ApplyTodos(globalDir, sid, change)
		if err != nil {
			return "", fmt.Errorf("todo-write: %w", err)
		}
		todos = saved
		notes = append(notes, applyNotes...)
	}
	return formatTodoList(todos, notes), nil
}

// formatTodoList renders the task list todo-write returns: tasks in
// dependency order, each with the incomplete dependencies gating it and
// whether it can start now.
func formatTodoList(todos []session.Todo, notes []string) string {
	pos := map[string]int{}
	for i, id := range session.TopoOrder(todos) {
		pos[id] = i
	}
	ordered := append([]session.Todo(nil), todos...)
	sort.SliceStable(ordered, func(i, j int) bool { return pos[ordered[i].ID] < pos[ordered[j].ID] })
	ready := map[string]struct{}{}
	for _, t := range session.ReadyTasks(todos) {
		ready[t.ID] = struct{}{}
	}
	out := struct {
		Tasks []todoRow `json:"tasks"`
		Notes []string  `json:"notes,omitempty"`
	}{Tasks: make([]todoRow, 0, len(ordered)), Notes: notes}
	for _, t := range ordered {
		_, isReady := ready[t.ID]
		out.Tasks = append(out.Tasks, todoRow{
			ID:           t.ID,
			Content:      t.Content,
			Status:       t.Status,
			Owner:        t.Owner,
			Source:       t.Source,
			Priority:     t.Priority,
			Dependencies: t.Dependencies,
			BlockedBy:    session.IncompleteDeps(t, todos),
			Ready:        isReady,
		})
	}
	raw, _ := json.Marshal(out)
	return string(raw)
}

func (r *toolRuntime) runToolSearch(allowed map[string]struct{}, rawArgs []byte) (string, error) {
	var args struct {
		Query string `json:"query"`
	}
	if err := decodeJSONStrict(rawArgs, &args); err != nil {
		return "", fmt.Errorf("tool-search args: %w", err)
	}
	q := strings.ToLower(strings.TrimSpace(args.Query))
	seen := map[string]struct{}{}
	var rows []string
	for id := range allowed {
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		spec, hasSpec := r.toolPolicies[id]
		// Aliases (a retired name, or a manifest alias) are callable but not
		// advertised: listing them would offer the model the same tool twice.
		if hasSpec && spec.ID != "" && spec.ID != id {
			continue
		}
		if _, retired := legacyTools[id]; retired && (!hasSpec || spec.Kind == "" || spec.Kind == "builtin") {
			continue
		}
		label := id
		risk := "unknown"
		acts := ""
		desc := ""
		requiresApproval := false
		timeoutSec := 0
		if hasSpec {
			if strings.TrimSpace(spec.Name) != "" {
				label = spec.Name
			}
			desc = strings.TrimSpace(spec.Description)
			if strings.TrimSpace(spec.RiskLevel) != "" {
				risk = spec.RiskLevel
			}
			requiresApproval = spec.RequiresApproval
			timeoutSec = spec.TimeoutSec
			acts = strings.Join(spec.PermittedActions, ",")
		}
		hay := strings.ToLower(id + " " + label + " " + acts + " " + risk + " " + desc)
		if q != "" && !strings.Contains(hay, q) {
			continue
		}
		score := 1
		if strings.Contains(strings.ToLower(id), q) || strings.Contains(strings.ToLower(label), q) {
			score += 3
		}
		if strings.Contains(acts, "search") {
			score++
		}
		rows = append(rows, fmt.Sprintf("%03d | %s | risk=%s | approval=%t | timeout=%ds | actions=%s | %s", score, id, risk, requiresApproval, timeoutSec, emptyIfBlank(acts), emptyIfBlank(desc)))
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i] > rows[j] })
	if len(rows) == 0 {
		return "no tools matched", nil
	}
	return strings.Join(rows, "\n"), nil
}

func (r *toolRuntime) runWebSearch(ctx context.Context, rawArgs []byte) (string, error) {
	var args struct {
		Query      string `json:"query"`
		MaxResults int    `json:"max_results"`
	}
	if err := decodeJSONStrict(rawArgs, &args); err != nil {
		return "", fmt.Errorf("web-search args: %w", err)
	}
	q := strings.TrimSpace(args.Query)
	if q == "" {
		return "", fmt.Errorf("web-search: query is required")
	}
	if err := r.authorizeNetworkAccess(ctx, "web-search", q); err != nil {
		return "", err
	}
	u := "https://html.duckduckgo.com/html/?q=" + url.QueryEscape(q)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "Spettro Agent/1.0")
	resp, err := r.fetchClient(15 * time.Second).Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("web-search failed: %s", resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 512*1024))
	if err != nil {
		return "", fmt.Errorf("web-search read: %w", err)
	}
	out := string(body)
	if args.MaxResults <= 0 {
		args.MaxResults = 10
	}
	lines := extractDuckDuckGoResults(out, args.MaxResults)
	if len(lines) == 0 {
		return truncate(out, 4000), nil
	}
	return strings.Join(lines, "\n"), nil
}

var ddgResultAnchorRE = regexp.MustCompile(`(?is)<a[^>]*class="[^"]*result__a[^"]*"[^>]*href="([^"]+)"[^>]*>(.*?)</a>`)
var htmlTagRE = regexp.MustCompile(`(?is)<[^>]+>`)

func extractDuckDuckGoResults(s string, limit int) []string {
	matches := ddgResultAnchorRE.FindAllStringSubmatch(s, -1)
	seen := map[string]struct{}{}
	var out []string
	for _, m := range matches {
		if len(m) < 3 {
			continue
		}
		href := strings.TrimSpace(html.UnescapeString(m[1]))
		target := resolveDuckDuckGoResultURL(href)
		if target == "" {
			continue
		}
		title := strings.TrimSpace(html.UnescapeString(htmlTagRE.ReplaceAllString(m[2], "")))
		if title == "" {
			title = target
		}
		if _, ok := seen[target]; ok {
			continue
		}
		seen[target] = struct{}{}
		out = append(out, fmt.Sprintf("%s — %s", title, target))
		if len(out) >= limit {
			break
		}
	}
	return out
}

func resolveDuckDuckGoResultURL(href string) string {
	href = strings.TrimSpace(href)
	if href == "" {
		return ""
	}
	if strings.HasPrefix(href, "//") {
		href = "https:" + href
	}
	if strings.HasPrefix(href, "/l/?") {
		href = "https://duckduckgo.com" + href
	}
	u, err := url.Parse(href)
	if err != nil {
		return ""
	}
	if strings.Contains(strings.ToLower(u.Host), "duckduckgo.com") {
		encoded := u.Query().Get("uddg")
		if encoded == "" {
			return ""
		}
		decoded, err := url.QueryUnescape(encoded)
		if err != nil {
			return ""
		}
		decoded = strings.TrimSpace(decoded)
		if strings.HasPrefix(decoded, "http://") || strings.HasPrefix(decoded, "https://") {
			return decoded
		}
		return ""
	}
	if strings.HasPrefix(href, "http://") || strings.HasPrefix(href, "https://") {
		return href
	}
	return ""
}

func (r *toolRuntime) runMCPListResources(ctx context.Context, rawArgs []byte) (string, error) {
	var args struct {
		ServerID string `json:"server_id"`
	}
	if err := decodeJSONStrict(rawArgs, &args); err != nil {
		return "", fmt.Errorf("mcp-list-resources args: %w", err)
	}
	if err := r.authorizeNetworkAccess(ctx, "mcp-list-resources", emptyIfBlank(args.ServerID)); err != nil {
		return "", err
	}
	rows, err := mcp.ListResources(r.cwd, strings.TrimSpace(args.ServerID))
	if err != nil {
		return "", err
	}
	raw, _ := json.Marshal(rows)
	return string(raw), nil
}

func (r *toolRuntime) runMCPReadResource(ctx context.Context, rawArgs []byte) (string, error) {
	var args struct {
		ServerID   string `json:"server_id"`
		ResourceID string `json:"resource_id"`
	}
	if err := decodeJSONStrict(rawArgs, &args); err != nil {
		return "", fmt.Errorf("mcp-read-resource args: %w", err)
	}
	if strings.TrimSpace(args.ServerID) == "" || strings.TrimSpace(args.ResourceID) == "" {
		return "", fmt.Errorf("mcp-read-resource: server_id and resource_id are required")
	}
	if err := r.authorizeNetworkAccess(ctx, "mcp-read-resource", args.ServerID+":"+args.ResourceID); err != nil {
		return "", err
	}
	out, err := mcp.ReadResource(r.cwd, strings.TrimSpace(args.ServerID), strings.TrimSpace(args.ResourceID))
	if err != nil {
		return "", err
	}
	return truncate(out, 12000), nil
}

func (r *toolRuntime) runMCPAuth(ctx context.Context, rawArgs []byte) (string, error) {
	var args struct {
		ServerID    string `json:"server_id"`
		Token       string `json:"token"`
		ExpiresAt   string `json:"expires_at"`
		Description string `json:"description"`
	}
	if err := decodeJSONStrict(rawArgs, &args); err != nil {
		return "", fmt.Errorf("mcp-auth args: %w", err)
	}
	if strings.TrimSpace(args.ServerID) == "" {
		return "", fmt.Errorf("mcp-auth: server_id is required")
	}
	if strings.TrimSpace(args.Token) == "" {
		return "", fmt.Errorf("mcp-auth: token is required")
	}
	if err := r.authorizeNetworkAccess(ctx, "mcp-auth", args.ServerID); err != nil {
		return "", err
	}
	state := mcp.AuthState{
		ServerID:    strings.TrimSpace(args.ServerID),
		Token:       strings.TrimSpace(args.Token),
		UpdatedAt:   time.Now(),
		Description: strings.TrimSpace(args.Description),
	}
	if strings.TrimSpace(args.ExpiresAt) != "" {
		if t, err := time.Parse(time.RFC3339, strings.TrimSpace(args.ExpiresAt)); err == nil {
			state.ExpiresAt = t
		}
	}
	if err := mcp.SaveAuth(r.cwd, state); err != nil {
		return "", err
	}
	return fmt.Sprintf("mcp auth updated for %s", state.ServerID), nil
}

func (r *toolRuntime) runFileEdit(ctx context.Context, rawArgs []byte) (string, error) {
	args, err := decodeFileEditArgs(rawArgs)
	if err != nil {
		return "", err
	}
	abs, rel, err := r.resolvePath(args.Path)
	if err != nil {
		return "", err
	}
	// A whitespace-only old_string ("\n\n\n" to collapse blank lines) is a
	// real edit; only an absent or empty one is missing.
	hasSingle := args.Single.OldString != ""
	if !hasSingle && len(args.Edits) == 0 {
		return "", fmt.Errorf("file-edit: old_string or edits is required")
	}
	defer r.lockFile(abs)()
	raw, err := os.ReadFile(abs)
	if err != nil {
		return "", err
	}
	if err := r.checkFileStamp("file-edit", rel, raw); err != nil {
		return "", err
	}
	trustLines := r.unchangedSinceRead(rel, raw)
	content := string(raw)
	scope := content
	prefix := ""
	suffix := ""
	lineOffset := 0
	// The line-ending style is the whole file's: a one-line scope of a CRLF
	// file has no "\r\n" of its own, and applyEdit would write bare LFs.
	crlf := false
	if args.StartLine > 0 || args.EndLine > 0 {
		work := content
		if crlf = isCRLF(work); crlf {
			work = strings.ReplaceAll(work, "\r\n", "\n")
		}
		lines := strings.Split(work, "\n")
		start := args.StartLine
		if start <= 0 {
			start = 1
		}
		end := args.EndLine
		if end <= 0 || end > len(lines) {
			end = len(lines)
		}
		if start > end || start > len(lines) {
			return "", fmt.Errorf("file-edit: invalid line range")
		}
		lineOffset = start - 1
		prefix = strings.Join(lines[:start-1], "\n")
		scope = strings.Join(lines[start-1:end], "\n")
		suffix = strings.Join(lines[end:], "\n")
		if prefix != "" {
			prefix += "\n"
		}
		if suffix != "" {
			suffix = "\n" + suffix
		}
	}
	type fileEditOp struct {
		old        string
		new        string
		replaceAll bool
	}
	ops := make([]fileEditOp, 0, len(args.Edits)+1)
	if hasSingle {
		ops = append(ops, fileEditOp{old: args.Single.OldString, new: args.Single.NewString, replaceAll: args.Single.ReplaceAll})
	}
	for i, e := range args.Edits {
		if e.OldString == "" {
			// edits[] is all or nothing: a blank item is an error, never
			// silently skipped while the rest are written.
			return "", fmt.Errorf("file-edit: edit %d: old_string is required (file untouched)", i+1)
		}
		ops = append(ops, fileEditOp{old: e.OldString, new: e.NewString, replaceAll: e.ReplaceAll})
	}
	updated := scope
	totalReplacements := 0
	var notes []string
	for i, op := range ops {
		label := ""
		if len(ops) > 1 {
			label = fmt.Sprintf("edit %d: ", i+1)
		}
		if op.old == op.new {
			return "", fmt.Errorf("file-edit: %sold_string and new_string are identical", label)
		}
		// Line numbers the model copied only describe the file as it was
		// read; after the first op they may have moved.
		res, err := applyEdit(updated, editRequest{Old: op.old, New: op.new, ReplaceAll: op.replaceAll, LineOffset: lineOffset,
			TrustLineNumbers: i == 0 && trustLines})
		if err != nil {
			return "", fmt.Errorf("file-edit: %s%w", label, err)
		}
		updated = res.Content
		totalReplacements += res.Count
		for _, n := range res.Notes {
			notes = append(notes, label+n)
		}
	}
	if args.Expected > 0 && totalReplacements != args.Expected {
		return "", fmt.Errorf("file-edit: expected %d replacements, got %d", args.Expected, totalReplacements)
	}
	updated = prefix + updated + suffix
	if crlf {
		updated = strings.ReplaceAll(updated, "\n", "\r\n")
	}
	// Approval comes after the edit is fully computed so the user can be shown
	// the exact diff that would be applied.
	if err := r.authorizeWriteAccess(ctx, "file-edit", rel, diff.Unified(rel, content, updated)); err != nil {
		return "", err
	}
	if err := r.recheckBeforeWrite("file-edit", rel, abs, true, raw); err != nil {
		return "", err
	}
	if err := os.WriteFile(abs, []byte(updated), 0o644); err != nil {
		return "", err
	}
	r.mu.Lock()
	r.readSet[rel] = struct{}{}
	r.mu.Unlock()
	r.recordFileStamp(rel, []byte(updated))
	r.invalidateSymbolIndex(rel)
	msg := fmt.Sprintf("edited %s (%d replacements)", rel, totalReplacements) + editNotesSuffix(notes) + "\n" + editDiffSummary(rel, content, updated)
	return r.withLSPDiagnostics(ctx, abs, msg), nil
}

// editNotesSuffix renders the match notes of an edit, nudging the model to
// quote verbatim when a fuzzy tier was needed.
func editNotesSuffix(notes []string) string {
	if len(notes) == 0 {
		return ""
	}
	return " — " + strings.Join(notes, "; ") + "; old_string was not byte-exact, quote the file verbatim next time"
}

func (r *toolRuntime) runPlanModeToggle(rawArgs []byte, entering bool) (string, error) {
	var args struct {
		Reason string `json:"reason"`
	}
	if err := decodeJSONStrict(rawArgs, &args); err != nil {
		if entering {
			return "", fmt.Errorf("enter-plan-mode args: %w", err)
		}
		return "", fmt.Errorf("exit-plan-mode args: %w", err)
	}
	mode := "EXITED"
	if entering {
		mode = "ENTERED"
	}
	reason := strings.TrimSpace(args.Reason)
	if reason == "" {
		return fmt.Sprintf("PLAN_MODE_%s", mode), nil
	}
	return fmt.Sprintf("PLAN_MODE_%s: %s", mode, reason), nil
}

func (r *toolRuntime) runEnterWorktree(ctx context.Context, rawArgs []byte) (string, error) {
	var args struct {
		Path       string `json:"path"`
		Branch     string `json:"branch"`
		AllowDirty bool   `json:"allow_dirty"`
	}
	if err := decodeJSONStrict(rawArgs, &args); err != nil {
		return "", fmt.Errorf("enter-worktree args: %w", err)
	}
	path := strings.TrimSpace(args.Path)
	if path == "" {
		return "", fmt.Errorf("enter-worktree: path is required")
	}
	abs, _, err := r.resolvePath(path)
	if err != nil {
		return "", err
	}
	branch := strings.TrimSpace(args.Branch)
	if strings.HasPrefix(branch, "-") {
		return "", fmt.Errorf("enter-worktree: invalid branch name %q (must not start with '-')", branch)
	}
	if !args.AllowDirty {
		if dirty, err := isGitDirty(ctx, r.cwd); err == nil && dirty {
			return "", fmt.Errorf("enter-worktree: repository has uncommitted changes (set allow_dirty=true to bypass)")
		}
	}
	if branch != "" {
		exists, err := localBranchExists(ctx, r.cwd, branch)
		if err != nil {
			return "", fmt.Errorf("enter-worktree: check branch: %w", err)
		}
		if exists {
			return "", fmt.Errorf("enter-worktree: branch %q already exists", branch)
		}
	}
	cmdArgs := []string{"worktree", "add"}
	if branch != "" {
		cmdArgs = append(cmdArgs, "-b", branch)
	}
	// "--" terminates option parsing so the resolved path can never be treated
	// as a git flag.
	cmdArgs = append(cmdArgs, "--", abs)
	cmd := exec.CommandContext(ctx, "git", cmdArgs...)
	cmd.Dir = r.cwd
	out, err := cmd.CombinedOutput()
	if err != nil {
		return truncate(string(out), 2000), fmt.Errorf("enter-worktree: %w", err)
	}
	return truncate(string(out), 2000), nil
}

func (r *toolRuntime) runExitWorktree(ctx context.Context, rawArgs []byte) (string, error) {
	var args struct {
		Path  string `json:"path"`
		Force bool   `json:"force"`
	}
	if err := decodeJSONStrict(rawArgs, &args); err != nil {
		return "", fmt.Errorf("exit-worktree args: %w", err)
	}
	path := strings.TrimSpace(args.Path)
	if path == "" {
		return "", fmt.Errorf("exit-worktree: path is required")
	}
	abs, _, err := r.resolvePath(path)
	if err != nil {
		return "", err
	}
	if !args.Force {
		dirty, err := isGitDirty(ctx, abs)
		if err == nil && dirty {
			return "", fmt.Errorf("exit-worktree: worktree has uncommitted changes (use force=true)")
		}
	}
	cmdArgs := []string{"worktree", "remove", abs}
	if args.Force {
		cmdArgs = append(cmdArgs, "--force")
	}
	cmd := exec.CommandContext(ctx, "git", cmdArgs...)
	cmd.Dir = r.cwd
	out, err := cmd.CombinedOutput()
	if err != nil {
		return truncate(string(out), 2000), fmt.Errorf("exit-worktree: %w", err)
	}
	return truncate(string(out), 2000), nil
}

func (r *toolRuntime) runSendMessage(rawArgs []byte) (string, error) {
	var args struct {
		Target  string `json:"target"`
		Message string `json:"message"`
	}
	if err := decodeJSONStrict(rawArgs, &args); err != nil {
		return "", fmt.Errorf("send-message args: %w", err)
	}
	msg := strings.TrimSpace(args.Message)
	if msg == "" {
		return "", fmt.Errorf("send-message: message is required")
	}
	if strings.TrimSpace(r.sessionDir) == "" {
		return "", fmt.Errorf("send-message requires an active session")
	}
	sid := filepath.Base(r.sessionDir)
	globalDir := filepath.Dir(filepath.Dir(r.sessionDir))
	_ = session.AppendEvent(globalDir, sid, session.AgentEvent{
		At:      time.Now(),
		Kind:    "message",
		AgentID: r.agentID,
		Task:    msg,
		Status:  "sent",
		Summary: "to " + emptyIfBlank(args.Target) + ": " + truncate(msg, 180),
	})
	return "message sent", nil
}

func (r *toolRuntime) runTaskStop(rawArgs []byte) (string, error) {
	var args struct {
		Reason string `json:"reason"`
	}
	if err := decodeJSONStrict(rawArgs, &args); err != nil {
		return "", fmt.Errorf("task-stop args: %w", err)
	}
	reason := strings.TrimSpace(args.Reason)
	if reason == "" {
		reason = "Stopped by task-stop request."
	}
	r.requestStop(reason)
	return reason, nil
}

// setSubAgentThinking records the thinking level the model last accepted:
// when the manager had to step a rejected level down, sub-agents start from
// the accepted one instead of re-sending (and re-failing) the original.
func (r *toolRuntime) setSubAgentThinking(level provider.ThinkingLevel) {
	r.mu.Lock()
	r.thinkingLevel = level
	r.mu.Unlock()
}

// subAgentThinking is the thinking level a sub-agent starts from.
func (r *toolRuntime) subAgentThinking() provider.ThinkingLevel {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.thinkingLevel
}

// requestStop ends the turn after the current batch of tool results is
// appended. The reason becomes the run's closing line, so it is written for the
// user rather than the model.
func (r *toolRuntime) requestStop(reason string) {
	r.mu.Lock()
	r.stopRequested = true
	r.stopReason = reason
	r.mu.Unlock()
}

func (r *toolRuntime) shouldStop() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.stopRequested
}

func (r *toolRuntime) stopMessage() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if strings.TrimSpace(r.stopReason) == "" {
		return "Stopped by task-stop request."
	}
	return r.stopReason
}

// runGoalComplete is the authoritative "the goal is done" signal in goal mode.
// It is only meaningful when the run was started with GoalMode=true; in normal
// runs it is not in the allowed-tool set, so it can't be called.
func (r *toolRuntime) runGoalComplete(rawArgs []byte) (string, error) {
	var args struct {
		Summary  string `json:"summary"`
		Verified bool   `json:"verified"`
	}
	if err := decodeJSONStrict(rawArgs, &args); err != nil {
		return "", fmt.Errorf("goal-complete args: %w", err)
	}
	if !r.goalMode {
		return "", fmt.Errorf("goal-complete is only available in goal mode")
	}
	r.mu.Lock()
	r.goalComplete = true
	r.goalSummary = strings.TrimSpace(args.Summary)
	r.goalVerified = args.Verified
	r.mu.Unlock()
	return "goal marked complete", nil
}

func (r *toolRuntime) goalIsComplete() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.goalComplete
}

func (r *toolRuntime) runConfigTool(rawArgs []byte) (string, error) {
	var args struct {
		Action string `json:"action"`
		Key    string `json:"key"`
		Value  string `json:"value"`
		Force  bool   `json:"force"`
	}
	if err := decodeJSONStrict(rawArgs, &args); err != nil {
		return "", fmt.Errorf("config args: %w", err)
	}
	action := strings.ToLower(strings.TrimSpace(args.Action))
	if action == "" {
		action = "get"
	}
	key := strings.ToLower(strings.TrimSpace(args.Key))
	rawCfg, err := readRawConfigMap()
	if err != nil {
		return "", err
	}
	cfg, err := config.Load()
	if err != nil {
		return "", fmt.Errorf("config: load: %w", err)
	}
	getOne := func(k string) (string, error) {
		switch k {
		case "permission":
			return string(cfg.Permission), nil
		case "token_budget":
			return fmt.Sprintf("%d", cfg.TokenBudget), nil
		case "show_side_panel":
			if cfg.ShowSidePanel {
				return "true", nil
			}
			return "false", nil
		case "active_provider":
			return cfg.ActiveProvider, nil
		case "active_model":
			return cfg.ActiveModel, nil
		default:
			return "", fmt.Errorf("config: unsupported key %q", k)
		}
	}
	setOne := func(k, v string) error {
		switch k {
		case "permission":
			p := config.PermissionLevel(strings.TrimSpace(v))
			switch p {
			case config.PermissionYOLO, config.PermissionRestricted, config.PermissionAskFirst:
				cfg.Permission = p
			default:
				return fmt.Errorf("config: invalid permission")
			}
		case "token_budget":
			var n int
			if _, err := fmt.Sscanf(strings.TrimSpace(v), "%d", &n); err != nil || n < 0 {
				return fmt.Errorf("config: invalid token_budget")
			}
			cfg.TokenBudget = n
		case "show_side_panel":
			switch strings.ToLower(strings.TrimSpace(v)) {
			case "1", "true", "yes", "on":
				cfg.ShowSidePanel = true
			case "0", "false", "no", "off":
				cfg.ShowSidePanel = false
			default:
				return fmt.Errorf("config: invalid show_side_panel")
			}
		default:
			return fmt.Errorf("config: unsupported mutable key %q", k)
		}
		return config.Save(cfg)
	}
	switch action {
	case "get":
		if key == "" {
			return fmt.Sprintf("permission=%s token_budget=%d show_side_panel=%t active_provider=%s active_model=%s", cfg.Permission, cfg.TokenBudget, cfg.ShowSidePanel, cfg.ActiveProvider, cfg.ActiveModel), nil
		}
		v, err := getOne(key)
		if err != nil {
			return "", err
		}
		return key + "=" + v, nil
	case "set":
		if key == "" {
			return "", fmt.Errorf("config: key is required for set")
		}
		if isConfigKeyPreset(rawCfg, key) && !args.Force {
			v, _ := getOne(key)
			return key + "=" + v + " (preset; unchanged)", nil
		}
		if err := setOne(key, args.Value); err != nil {
			return "", err
		}
		v, _ := getOne(key)
		return key + "=" + v, nil
	default:
		return "", fmt.Errorf("config: action must be get or set")
	}
}

func readRawConfigMap() (map[string]json.RawMessage, error) {
	path, err := config.Path()
	if err != nil {
		return nil, fmt.Errorf("config: path: %w", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]json.RawMessage{}, nil
		}
		return nil, fmt.Errorf("config: read: %w", err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("config: decode: %w", err)
	}
	return m, nil
}

func isConfigKeyPreset(raw map[string]json.RawMessage, key string) bool {
	jsonKey := key
	switch key {
	case "token_budget":
		jsonKey = "token_budget"
	case "show_side_panel":
		jsonKey = "show_side_panel"
	case "active_provider":
		jsonKey = "active_provider"
	case "active_model":
		jsonKey = "active_model"
	case "permission":
		jsonKey = "permission"
	}
	v, ok := raw[jsonKey]
	if !ok {
		return false
	}
	trimmed := strings.TrimSpace(string(v))
	return trimmed != "" && trimmed != "null"
}

func isGitDirty(ctx context.Context, dir string) (bool, error) {
	cmd := exec.CommandContext(ctx, "git", "status", "--porcelain")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(string(out)) != "", nil
}

func localBranchExists(ctx context.Context, dir, branch string) (bool, error) {
	cmd := exec.CommandContext(ctx, "git", "show-ref", "--verify", "--quiet", "refs/heads/"+branch)
	cmd.Dir = dir
	if err := cmd.Run(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() != 0 {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

type allowedNetworkFile struct {
	Allowed []string `json:"allowed"`
}

func allowedNetworkPath(cwd string) string {
	return filepath.Join(cwd, ".spettro", "allowed_network.json")
}

func loadAllowedNetworkSet(cwd string) (map[string]struct{}, error) {
	out := map[string]struct{}{}
	raw, err := os.ReadFile(allowedNetworkPath(cwd))
	if err != nil {
		if os.IsNotExist(err) {
			return out, nil
		}
		return nil, err
	}
	var parsed allowedNetworkFile
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, err
	}
	for _, s := range parsed.Allowed {
		s = strings.TrimSpace(s)
		if s != "" {
			out[s] = struct{}{}
		}
	}
	return out, nil
}

func saveAllowedNetworkSet(cwd string, set map[string]struct{}) error {
	items := make([]string, 0, len(set))
	for s := range set {
		if strings.TrimSpace(s) != "" {
			items = append(items, s)
		}
	}
	sort.Strings(items)
	raw, err := json.MarshalIndent(allowedNetworkFile{Allowed: items}, "", "  ")
	if err != nil {
		return err
	}
	path := allowedNetworkPath(cwd)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	return safeio.Replace(tmp, path)
}

// authorizeWriteAccess gates file-write/file-edit on the tool's approval
// policy. Writes were previously ungated regardless of policy; this makes a
// manifest's `requires_approval = true` on the write tools actually take
// effect. When the policy does not require approval (the default) or we are in
// YOLO mode, writes proceed unchanged.
func (r *toolRuntime) authorizeWriteAccess(ctx context.Context, toolID, relPath, diff string) error {
	// The OS sandbox policy is non-negotiable and independent of the approval
	// flow (it is an operator setting, not a per-command permission). The
	// in-process file tools must honor the same FS scope the kernel enforces on
	// shell children, or read-only confinement would be trivially bypassable by
	// writing through file-write instead of a shell redirect. The error is
	// deliberately generic so it reads as an ordinary filesystem denial.
	if pol := r.sandboxPolicy(); pol.FSEnforced() {
		abs := filepath.Join(r.cwd, filepath.FromSlash(relPath))
		if !pol.WritablePath(abs, r.cwd, sandbox.WritableTempDirs()) {
			return fmt.Errorf("%s: %s is not writable", toolID, relPath)
		}
	}
	spec, ok := r.toolPolicies[toolID]
	if !ok || !spec.RequiresApproval || r.perm() == config.PermissionYOLO {
		return nil
	}
	if r.shellApproval == nil {
		return fmt.Errorf("%s requires approval outside yolo mode", toolID)
	}
	decision, err := r.shellApproval(ctx, ShellApprovalRequest{
		ToolID:  toolID,
		Command: toolID + " " + relPath,
		Reason:  "file modification requires approval",
		Diff:    diff,
	})
	if err != nil {
		return fmt.Errorf("write approval failed: %w", err)
	}
	switch decision {
	case ShellApprovalAllowOnce, ShellApprovalAllowAlways:
		return nil
	default:
		return fmt.Errorf("%s denied by user", toolID)
	}
}

func (r *toolRuntime) authorizeNetworkAccess(ctx context.Context, toolID, target string) error {
	target = normalizeCommand(target)
	if target == "" {
		target = "(network)"
	}
	needsApproval := r.perm() != config.PermissionYOLO
	if spec, ok := r.toolPolicies[toolID]; ok && !spec.RequiresApproval {
		needsApproval = false
	}
	toolRules := []config.PermissionRule{}
	if spec, ok := r.toolPolicies[toolID]; ok {
		toolRules = append(toolRules, spec.PermissionRules...)
	}
	switch evaluatePermissionRule("network", target, r.runtimeRules, r.agentRules, toolRules) {
	case config.RuleDeny:
		return fmt.Errorf("%s denied by policy for target %q", toolID, target)
	case config.RuleAllow:
		return nil
	}
	allowed, err := loadAllowedNetworkSet(r.cwd)
	if err != nil {
		return fmt.Errorf("read network approvals: %w", err)
	}
	if _, ok := allowed[target]; ok || !needsApproval {
		return nil
	}
	if r.shellApproval == nil {
		return fmt.Errorf("%s requires approval outside yolo mode", toolID)
	}
	decision, err := r.shellApproval(ctx, ShellApprovalRequest{Command: "network " + toolID + " " + target})
	if err != nil {
		return fmt.Errorf("network approval failed: %w", err)
	}
	switch decision {
	case ShellApprovalAllowOnce:
		return nil
	case ShellApprovalAllowAlways:
		allowed[target] = struct{}{}
		if err := saveAllowedNetworkSet(r.cwd, allowed); err != nil {
			return fmt.Errorf("persist network approval: %w", err)
		}
		return nil
	default:
		return fmt.Errorf("%s denied by user", toolID)
	}
}
