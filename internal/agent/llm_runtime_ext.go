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
	"spettro/internal/mcp"
	"spettro/internal/session"
)

func (r *toolRuntime) runAskUser(ctx context.Context, rawArgs []byte) (string, error) {
	var args struct {
		Question          string   `json:"question"`
		Options           []string `json:"options"`
		Context           string   `json:"context"`
		DefaultOption     string   `json:"default_option"`
		AllowFreeResponse bool     `json:"allow_free_response"`
	}
	if err := decodeJSONStrict(rawArgs, &args); err != nil {
		return "", fmt.Errorf("ask-user args: %w", err)
	}
	q := strings.TrimSpace(args.Question)
	if q == "" {
		return "", fmt.Errorf("ask-user: question is required")
	}
	opts := make([]string, 0, len(args.Options))
	for _, o := range args.Options {
		o = strings.TrimSpace(o)
		if o != "" {
			opts = append(opts, o)
		}
	}
	if len(opts) == 0 {
		if !args.AllowFreeResponse {
			args.AllowFreeResponse = true
		}
	}
	if r.askUser == nil {
		return "", fmt.Errorf("ask-user: interactive callback not configured")
	}
	answer, err := r.askUser(ctx, AskUserRequest{
		Question:          q,
		Options:           opts,
		Context:           strings.TrimSpace(args.Context),
		DefaultOption:     strings.TrimSpace(args.DefaultOption),
		AllowFreeResponse: args.AllowFreeResponse,
	})
	if err != nil {
		return "", fmt.Errorf("ask-user: %w", err)
	}
	answer = strings.TrimSpace(answer)
	if answer == "" {
		return "", fmt.Errorf("ask-user: empty response")
	}
	payload := map[string]any{
		"question": q,
		"answer":   answer,
	}
	if len(opts) > 0 {
		payload["options"] = opts
	}
	if strings.TrimSpace(args.Context) != "" {
		payload["context"] = strings.TrimSpace(args.Context)
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Sprintf("User answered %q to %q", answer, q), nil
	}
	return string(raw), nil
}

func (r *toolRuntime) runTaskCreate(rawArgs []byte) (string, error) {
	var args struct {
		ID           string   `json:"id"`
		Content      string   `json:"content"`
		Status       string   `json:"status"`
		Owner        string   `json:"owner"`
		Source       string   `json:"source"`
		Priority     string   `json:"priority"`
		Dependencies []string `json:"dependencies"`
	}
	if err := decodeJSONStrict(rawArgs, &args); err != nil {
		return "", fmt.Errorf("task-create args: %w", err)
	}
	if strings.TrimSpace(r.sessionDir) == "" {
		return "", fmt.Errorf("task-create requires an active session")
	}
	id := strings.TrimSpace(args.ID)
	if id == "" {
		id = fmt.Sprintf("task-%d", time.Now().UnixMilli())
	}
	item := session.Todo{
		ID:           id,
		Content:      strings.TrimSpace(args.Content),
		Status:       strings.TrimSpace(args.Status),
		Owner:        strings.TrimSpace(args.Owner),
		Source:       strings.TrimSpace(args.Source),
		Priority:     strings.TrimSpace(args.Priority),
		Dependencies: append([]string(nil), args.Dependencies...),
	}
	if item.Status == "" {
		item.Status = "pending"
	}
	sid := filepath.Base(r.sessionDir)
	out, err := session.UpsertTodo(filepath.Dir(filepath.Dir(r.sessionDir)), sid, item)
	if err != nil {
		return "", err
	}
	raw, _ := json.Marshal(out)
	return string(raw), nil
}

func (r *toolRuntime) runTaskGet(rawArgs []byte) (string, error) {
	var args struct {
		ID string `json:"id"`
	}
	if err := decodeJSONStrict(rawArgs, &args); err != nil {
		return "", fmt.Errorf("task-get args: %w", err)
	}
	if strings.TrimSpace(r.sessionDir) == "" {
		return "", fmt.Errorf("task-get requires an active session")
	}
	id := strings.TrimSpace(args.ID)
	if id == "" {
		return "", fmt.Errorf("task-get: id is required")
	}
	sid := filepath.Base(r.sessionDir)
	item, ok, err := session.GetTodo(filepath.Dir(filepath.Dir(r.sessionDir)), sid, id)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("task-get: task %q not found", id)
	}
	raw, _ := json.Marshal(item)
	return string(raw), nil
}

func (r *toolRuntime) runTaskUpdate(rawArgs []byte) (string, error) {
	var args struct {
		ID           string   `json:"id"`
		Content      string   `json:"content"`
		Status       string   `json:"status"`
		Owner        string   `json:"owner"`
		Source       string   `json:"source"`
		Priority     string   `json:"priority"`
		Dependencies []string `json:"dependencies"`
	}
	if err := decodeJSONStrict(rawArgs, &args); err != nil {
		return "", fmt.Errorf("task-update args: %w", err)
	}
	if strings.TrimSpace(r.sessionDir) == "" {
		return "", fmt.Errorf("task-update requires an active session")
	}
	id := strings.TrimSpace(args.ID)
	if id == "" {
		return "", fmt.Errorf("task-update: id is required")
	}
	sid := filepath.Base(r.sessionDir)
	globalDir := filepath.Dir(filepath.Dir(r.sessionDir))
	prev, ok, err := session.GetTodo(globalDir, sid, id)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("task-update: task %q not found", id)
	}
	if strings.TrimSpace(args.Content) != "" {
		prev.Content = strings.TrimSpace(args.Content)
	}
	if strings.TrimSpace(args.Status) != "" {
		prev.Status = strings.TrimSpace(args.Status)
	}
	if strings.TrimSpace(args.Owner) != "" {
		prev.Owner = strings.TrimSpace(args.Owner)
	}
	if strings.TrimSpace(args.Source) != "" {
		prev.Source = strings.TrimSpace(args.Source)
	}
	if strings.TrimSpace(args.Priority) != "" {
		prev.Priority = strings.TrimSpace(args.Priority)
	}
	if len(args.Dependencies) > 0 {
		prev.Dependencies = append([]string(nil), args.Dependencies...)
	}
	out, err := session.UpsertTodo(globalDir, sid, prev)
	if err != nil {
		return "", err
	}
	raw, _ := json.Marshal(out)
	return string(raw), nil
}

func (r *toolRuntime) runTaskList(rawArgs []byte) (string, error) {
	var args struct {
		Status string `json:"status"`
	}
	if err := decodeJSONStrict(rawArgs, &args); err != nil {
		return "", fmt.Errorf("task-list args: %w", err)
	}
	if strings.TrimSpace(r.sessionDir) == "" {
		return "", fmt.Errorf("task-list requires an active session")
	}
	sid := filepath.Base(r.sessionDir)
	items, err := session.LoadTodos(filepath.Dir(filepath.Dir(r.sessionDir)), sid)
	if err != nil {
		return "", err
	}
	filter := strings.TrimSpace(args.Status)
	if filter != "" {
		out := make([]session.Todo, 0, len(items))
		for _, t := range items {
			if t.Status == filter {
				out = append(out, t)
			}
		}
		items = out
	}
	raw, _ := json.Marshal(items)
	return string(raw), nil
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
	req.Header.Set("User-Agent", "spettro-web-search/1.0")
	client := newSafeHTTPClient(15 * time.Second)
	resp, err := client.Do(req)
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
	var args struct {
		Path       string `json:"path"`
		OldString  string `json:"old_string"`
		NewString  string `json:"new_string"`
		ReplaceAll bool   `json:"replace_all"`
		StartLine  int    `json:"start_line"`
		EndLine    int    `json:"end_line"`
		Expected   int    `json:"expected_replacements"`
		Edits      []struct {
			OldString  string `json:"old_string"`
			NewString  string `json:"new_string"`
			ReplaceAll bool   `json:"replace_all"`
		} `json:"edits"`
	}
	if err := decodeJSONStrict(rawArgs, &args); err != nil {
		return "", fmt.Errorf("file-edit args: %w", err)
	}
	abs, rel, err := r.resolvePath(args.Path)
	if err != nil {
		return "", err
	}
	if err := r.authorizeWriteAccess(ctx, "file-edit", rel); err != nil {
		return "", err
	}
	hasSingle := strings.TrimSpace(args.OldString) != ""
	if !hasSingle && len(args.Edits) == 0 {
		return "", fmt.Errorf("file-edit: old_string or edits is required")
	}
	raw, err := os.ReadFile(abs)
	if err != nil {
		return "", err
	}
	content := string(raw)
	scope := content
	prefix := ""
	suffix := ""
	if args.StartLine > 0 || args.EndLine > 0 {
		lines := strings.Split(content, "\n")
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
		ops = append(ops, fileEditOp{old: args.OldString, new: args.NewString, replaceAll: args.ReplaceAll})
	}
	for _, e := range args.Edits {
		if strings.TrimSpace(e.OldString) == "" {
			continue
		}
		ops = append(ops, fileEditOp{old: e.OldString, new: e.NewString, replaceAll: e.ReplaceAll})
	}
	updated := scope
	totalReplacements := 0
	for _, op := range ops {
		if !strings.Contains(updated, op.old) {
			return "", fmt.Errorf("file-edit: old_string not found: %q", truncate(op.old, 80))
		}
		before := updated
		if op.replaceAll {
			updated = strings.ReplaceAll(updated, op.old, op.new)
		} else {
			updated = strings.Replace(updated, op.old, op.new, 1)
		}
		totalReplacements += strings.Count(before, op.old) - strings.Count(updated, op.old)
	}
	if args.Expected > 0 && totalReplacements != args.Expected {
		return "", fmt.Errorf("file-edit: expected %d replacements, got %d", args.Expected, totalReplacements)
	}
	updated = prefix + updated + suffix
	if err := os.WriteFile(abs, []byte(updated), 0o644); err != nil {
		return "", err
	}
	r.mu.Lock()
	r.readSet[rel] = struct{}{}
	r.mu.Unlock()
	return fmt.Sprintf("edited %s (%d replacements)", rel, totalReplacements), nil
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
	r.mu.Lock()
	r.stopRequested = true
	r.stopReason = reason
	r.mu.Unlock()
	return reason, nil
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
	return os.Rename(tmp, path)
}

// authorizeWriteAccess gates file-write/file-edit on the tool's approval
// policy. Writes were previously ungated regardless of policy; this makes a
// manifest's `requires_approval = true` on the write tools actually take
// effect. When the policy does not require approval (the default) or we are in
// YOLO mode, writes proceed unchanged.
func (r *toolRuntime) authorizeWriteAccess(ctx context.Context, toolID, relPath string) error {
	// The OS sandbox policy is non-negotiable and independent of the approval
	// flow (it is an operator setting, not a per-command permission). The
	// in-process file tools must honor the same FS scope the kernel enforces on
	// shell children, or read-only confinement would be trivially bypassable by
	// writing through file-write instead of a shell redirect. The error is
	// deliberately generic so it reads as an ordinary filesystem denial.
	if pol := r.sandboxPolicy(); pol.FSEnforced() {
		abs := filepath.Join(r.cwd, filepath.FromSlash(relPath))
		if !pol.WritablePath(abs, r.cwd, []string{os.TempDir()}) {
			return fmt.Errorf("%s: %s is not writable", toolID, relPath)
		}
	}
	spec, ok := r.toolPolicies[toolID]
	if !ok || !spec.RequiresApproval || r.permission == config.PermissionYOLO {
		return nil
	}
	if r.shellApproval == nil {
		return fmt.Errorf("%s requires approval outside yolo mode", toolID)
	}
	decision, err := r.shellApproval(ctx, ShellApprovalRequest{
		ToolID:  toolID,
		Command: toolID + " " + relPath,
		Reason:  "file modification requires approval",
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
	needsApproval := r.permission != config.PermissionYOLO
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
