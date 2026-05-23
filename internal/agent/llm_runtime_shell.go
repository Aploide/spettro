package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"spettro/internal/config"
)

func isBlockedCommand(cmd string) bool {
	l := strings.ToLower(cmd)
	blocked := []string{
		"git reset --hard",
		"git checkout --",
		"rm -rf /",
	}
	for _, b := range blocked {
		if strings.Contains(l, b) {
			return true
		}
	}
	return false
}

func (r *toolRuntime) runShellTool(ctx context.Context, toolID string, rawArgs []byte, prefix string) (string, error) {
	var args struct {
		Command string `json:"command"`
	}
	if err := decodeJSONStrict(rawArgs, &args); err != nil {
		return "", fmt.Errorf("%s args: %w", prefix, err)
	}
	cmdText := strings.TrimSpace(args.Command)
	if cmdText == "" {
		return "", fmt.Errorf("%s: command is required", prefix)
	}
	if err := r.authorizeShellCommand(ctx, toolID, cmdText); err != nil {
		return "", err
	}
	// Spettro mandates that every commit carries its Co-Authored-By trailer.
	// Auto-inject the `--trailer` flag whenever an LLM agent runs `git commit`
	// through shell/bash so the policy holds even if the model forgets it.
	cmdText = EnforceCommitCoAuthor(cmdText)
	cmd := exec.CommandContext(ctx, "bash", "-lc", cmdText)
	cmd.Dir = r.cwd
	out, err := cmd.CombinedOutput()
	text := truncate(string(out), 12000)
	if err != nil {
		return text, fmt.Errorf("command failed: %w", err)
	}
	return text, nil
}

func (r *toolRuntime) runWebFetch(ctx context.Context, rawArgs []byte) (string, error) {
	var args struct {
		URL string `json:"url"`
	}
	if err := decodeJSONStrict(rawArgs, &args); err != nil {
		return "", fmt.Errorf("web-fetch args: %w", err)
	}
	urlText := strings.TrimSpace(args.URL)
	if urlText == "" {
		return "", fmt.Errorf("web-fetch: url required")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, urlText, nil)
	if err != nil {
		return "", fmt.Errorf("web-fetch: %w", err)
	}
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("web-fetch: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 50*1024))
	if err != nil {
		return "", fmt.Errorf("web-fetch: read: %w", err)
	}
	return string(body), nil
}

type allowedCommandsFile struct {
	AllowedCommands []string `json:"allowed_commands"`
}

func isDelegationRoleAllowed(caller, target config.AgentRole) bool {
	switch caller {
	case config.AgentRolePrimary, config.AgentRoleOrchestrator:
		return target == config.AgentRoleWorker || target == config.AgentRoleSubagent
	case config.AgentRoleWorker, config.AgentRoleSubagent:
		return target == config.AgentRoleSubagent || target == config.AgentRoleWorker
	default:
		return false
	}
}

func marshalSubagentResult(agentID string, result RunResult) string {
	payload := map[string]any{
		"agent":            agentID,
		"status":           "ok",
		"summary":          truncate(strings.TrimSpace(result.Content), 4000),
		"tool_trace_count": len(result.Tools),
		"tokens_used":      result.TokensUsed,
	}
	if toolResults := summarizeSubagentToolResults(result.Tools, 6); len(toolResults) > 0 {
		payload["tool_results"] = toolResults
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Sprintf("{\"agent\":%q,\"status\":\"ok\",\"summary\":%q}", agentID, truncate(strings.TrimSpace(result.Content), 4000))
	}
	return string(raw)
}

func summarizeSubagentToolResults(traces []ToolTrace, limit int) []map[string]string {
	out := make([]map[string]string, 0, limit)
	for _, tr := range traces {
		if tr.Status == "running" || tr.Name == "comment" {
			continue
		}
		item := map[string]string{
			"tool":   tr.Name,
			"status": tr.Status,
		}
		if args := strings.TrimSpace(summarizeLoopToolArgs(tr.Name, tr.Args)); args != "" {
			item["args"] = truncate(args, 160)
		}
		if output := strings.TrimSpace(tr.Output); output != "" {
			item["output"] = truncate(strings.Join(strings.Fields(output), " "), 240)
		}
		out = append(out, item)
		if len(out) >= limit {
			break
		}
	}
	return out
}

var alwaysAllowedCommandPrefixes = []string{
	"ls",
	"pwd",
	"cat",
	"head",
	"tail",
	"wc",
	"grep",
	"rg",
	"find",
	"stat",
	"git status",
	"git diff",
	"go test",
	"go build",
	"go vet",
	"make test",
	"make build",
}

func (r *toolRuntime) authorizeShellCommand(ctx context.Context, toolID, command string) error {
	command = strings.TrimSpace(command)
	normalized := normalizeCommand(command)
	if normalized == "" {
		return fmt.Errorf("shell-exec command is required")
	}

	segments := splitShellCommandSegments(command)
	if len(segments) == 0 {
		segments = []string{normalized}
	}
	needsApproval := r.permission != config.PermissionYOLO
	if spec, ok := r.toolPolicies[toolID]; ok && !spec.RequiresApproval {
		needsApproval = false
	}

	missingApprovals := make([]string, 0, len(segments))
	toolRules := []config.PermissionRule{}
	if spec, ok := r.toolPolicies[toolID]; ok {
		toolRules = append(toolRules, spec.PermissionRules...)
	}
	r.shellMu.Lock()
	defer r.shellMu.Unlock()
	for _, seg := range segments {
		segNorm := normalizeCommand(seg)
		if segNorm == "" {
			continue
		}
		if isBlockedCommand(segNorm) {
			return fmt.Errorf("blocked dangerous command")
		}
		if isAlwaysAllowedCommand(segNorm) {
			continue
		}
		switch evaluatePermissionRule("execute", segNorm, r.runtimeRules, r.agentRules, toolRules) {
		case config.RuleDeny:
			r.emitApprovalTrace("denied", "policy", toolID, segNorm, "blocked by permission rules")
			return fmt.Errorf("shell-exec denied by policy for command segment %q", segNorm)
		case config.RuleAllow:
			continue
		}
		r.mu.Lock()
		_, preapproved := r.allowedShell[segNorm]
		r.mu.Unlock()
		if preapproved {
			continue
		}
		missingApprovals = append(missingApprovals, segNorm)
	}
	if len(missingApprovals) == 0 || !needsApproval {
		return nil
	}
	if decision, reason, err := r.runPermissionRequestHooks(ctx, toolID, command); err != nil {
		return fmt.Errorf("permission hooks failed: %w", err)
	} else if decision == "deny" {
		if strings.TrimSpace(reason) == "" {
			reason = "denied by permission hook"
		}
		r.emitApprovalTrace("denied", "hook", toolID, strings.Join(missingApprovals, " | "), reason)
		return fmt.Errorf("shell-exec denied by hook: %s", reason)
	} else if decision == "allow" {
		r.emitApprovalTrace("allowed", "hook", toolID, strings.Join(missingApprovals, " | "), reason)
		return nil
	}

	if r.shellApproval == nil {
		r.emitApprovalTrace("denied", "policy", toolID, strings.Join(missingApprovals, " | "), "approval required outside yolo mode")
		return fmt.Errorf("shell-exec requires approval outside yolo mode")
	}

	decision, err := r.shellApproval(ctx, ShellApprovalRequest{
		Command:  command,
		ToolID:   toolID,
		Segments: append([]string(nil), missingApprovals...),
		Reason:   "non-whitelisted command requires approval",
	})
	if err != nil {
		return fmt.Errorf("shell approval failed: %w", err)
	}
	switch decision {
	case ShellApprovalAllowOnce:
		r.emitApprovalTrace("allowed", "user", toolID, strings.Join(missingApprovals, " | "), "approved once")
		return nil
	case ShellApprovalAllowAlways:
		r.mu.Lock()
		for _, seg := range missingApprovals {
			r.allowedShell[seg] = struct{}{}
		}
		r.mu.Unlock()
		if err := saveAllowedCommandSet(r.cwd, r.allowedShell); err != nil {
			return fmt.Errorf("persist allowed command: %w", err)
		}
		r.emitApprovalTrace("allowed", "user", toolID, strings.Join(missingApprovals, " | "), "approved and persisted")
		return nil
	default:
		r.emitApprovalTrace("denied", "user", toolID, strings.Join(missingApprovals, " | "), "denied by user")
		return fmt.Errorf("shell-exec denied by user")
	}
}

func normalizeCommand(command string) string {
	return strings.Join(strings.Fields(strings.TrimSpace(command)), " ")
}

func splitShellCommandSegments(command string) []string {
	command = strings.TrimSpace(command)
	if command == "" {
		return nil
	}
	var (
		segments                []string
		buf                     strings.Builder
		inSingle, inDouble, esc bool
		subDepth                int
	)
	flush := func() {
		seg := strings.TrimSpace(buf.String())
		if seg != "" {
			segments = append(segments, seg)
		}
		buf.Reset()
	}

	for i := 0; i < len(command); i++ {
		ch := command[i]
		if esc {
			buf.WriteByte(ch)
			esc = false
			continue
		}
		switch ch {
		case '\\':
			esc = true
			buf.WriteByte(ch)
		case '\'':
			if !inDouble {
				inSingle = !inSingle
			}
			buf.WriteByte(ch)
		case '"':
			if !inSingle {
				inDouble = !inDouble
			}
			buf.WriteByte(ch)
		case '(':
			if !inSingle && !inDouble && i > 0 && command[i-1] == '$' {
				subDepth++
			}
			buf.WriteByte(ch)
		case ')':
			if !inSingle && !inDouble && subDepth > 0 {
				subDepth--
			}
			buf.WriteByte(ch)
		case ';':
			if inSingle || inDouble || subDepth > 0 {
				buf.WriteByte(ch)
				continue
			}
			flush()
		case '|':
			if inSingle || inDouble || subDepth > 0 {
				buf.WriteByte(ch)
				continue
			}
			if i+1 < len(command) && command[i+1] == '|' {
				flush()
				i++
				continue
			}
			flush()
		case '&':
			if inSingle || inDouble || subDepth > 0 {
				buf.WriteByte(ch)
				continue
			}
			if i+1 < len(command) && command[i+1] == '&' {
				flush()
				i++
				continue
			}
			buf.WriteByte(ch)
		case '\n':
			if inSingle || inDouble || subDepth > 0 {
				buf.WriteByte(ch)
				continue
			}
			flush()
		default:
			buf.WriteByte(ch)
		}
	}
	flush()
	return segments
}

func isAlwaysAllowedCommand(command string) bool {
	for _, prefix := range alwaysAllowedCommandPrefixes {
		if command == prefix || strings.HasPrefix(command, prefix+" ") {
			return true
		}
	}
	return false
}

func allowedCommandsPath(cwd string) string {
	return filepath.Join(cwd, ".spettro", "allowed_commands.json")
}

func loadAllowedCommandSet(cwd string) (map[string]struct{}, error) {
	out := map[string]struct{}{}
	data, err := os.ReadFile(allowedCommandsPath(cwd))
	if err != nil {
		if os.IsNotExist(err) {
			return out, nil
		}
		return nil, fmt.Errorf("read allowed commands: %w", err)
	}
	var parsed allowedCommandsFile
	if err := json.Unmarshal(data, &parsed); err != nil {
		return nil, fmt.Errorf("decode allowed commands: %w", err)
	}
	for _, cmd := range parsed.AllowedCommands {
		norm := normalizeCommand(cmd)
		if norm != "" {
			out[norm] = struct{}{}
		}
	}
	return out, nil
}

func saveAllowedCommandSet(cwd string, set map[string]struct{}) error {
	cmds := make([]string, 0, len(set))
	for cmd := range set {
		if strings.TrimSpace(cmd) != "" {
			cmds = append(cmds, cmd)
		}
	}
	sort.Strings(cmds)
	payload := allowedCommandsFile{AllowedCommands: cmds}
	raw, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return fmt.Errorf("encode allowed commands: %w", err)
	}

	path := allowedCommandsPath(cwd)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create .spettro dir: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return fmt.Errorf("write allowed commands temp: %w", err)
	}
	return os.Rename(tmp, path)
}
