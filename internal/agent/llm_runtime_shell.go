package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"spettro/internal/config"
	"spettro/internal/fsperm"
	"spettro/internal/jobs"
	"spettro/internal/safeio"
	"spettro/internal/sandbox"
	"spettro/internal/shell"
)

func isBlockedCommand(cmd string) bool {
	l := strings.ToLower(normalizeCommand(cmd))
	blocked := []string{
		"git reset --hard",
		"git checkout --",
	}
	for _, b := range blocked {
		if strings.Contains(l, b) {
			return true
		}
	}
	return isDangerousRM(cmd)
}

// shellToolArgs are the shell tool's arguments. cmd is accepted as an alias for
// command, and fields other harnesses send (description, cwd) are ignored by
// the lenient decoder rather than failing the call.
type shellToolArgs struct {
	Command         string   `json:"command"`
	Cmd             string   `json:"cmd"`
	RunInBackground flexBool `json:"run_in_background"`
	// Timeout is an optional per-call limit in seconds; see shellTimeout.
	Timeout flexInt `json:"timeout"`
}

func (r *toolRuntime) runShellTool(ctx context.Context, toolID string, rawArgs []byte, prefix string) (string, error) {
	var args shellToolArgs
	if err := decodeJSONStrict(rawArgs, &args); err != nil {
		return "", fmt.Errorf("%s args: %w", prefix, err)
	}
	cmdText := firstNonEmpty(args.Command, args.Cmd)
	if cmdText == "" {
		return "", fmt.Errorf("%s: command is required", prefix)
	}
	// Approval runs under its own window — the tool's default timeout, as
	// before per-call timeouts existed — so a short per-call timeout never
	// shortens the time the user has to read the prompt.
	approvalWindow := time.Duration(r.defaultToolTimeoutSec(toolID)) * time.Second
	approveCtx, cancelApproval := context.WithTimeout(ctx, approvalWindow)
	err := r.authorizeShellCommand(approveCtx, toolID, cmdText)
	approvalExpired := errors.Is(approveCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil
	cancelApproval()
	if err != nil {
		if approvalExpired {
			return "", fmt.Errorf("%s: no approval decision within %s; command not run", prefix, formatTimeoutSeconds(approvalWindow))
		}
		return "", err
	}
	// Spettro mandates that every commit carries its Co-Authored-By trailer.
	// Auto-inject the `--trailer` flag whenever an LLM agent runs `git commit`
	// through shell/bash so the policy holds even if the model forgets it.
	cmdText = EnforceCommitCoAuthor(cmdText)
	if args.RunInBackground {
		// Detached jobs must outlive this tool call: build the command on a
		// background context so the per-tool timeout doesn't kill it. The same
		// sandbox policy still wraps the process.
		shellName, shellArgs := shell.CommandLine(cmdText)
		cmd := sandbox.Command(context.Background(), r.sandboxPolicy(), r.cwd, shellName, shellArgs...)
		cmd.Dir = r.cwd
		job, err := jobs.Default().Start(cmd, cmdText)
		if err != nil {
			return "", fmt.Errorf("start background job: %w", err)
		}
		return fmt.Sprintf("started background job %s (poll with job-output, terminate with job-kill)", job.ID), nil
	}
	// The deadline starts here, after approval, so time spent waiting on the
	// user is never charged to the command. ctx carries no per-tool deadline
	// for foreground calls (see executeWithTimeout), so the command gets its
	// full timeout and a timeout report always means the command ran that long.
	timeout := r.shellTimeout(toolID, int(args.Timeout))
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	// OS-native confinement enforces the active sandbox policy at the kernel
	// level. The policy is set once at startup (CLI flags / manifest) and is
	// not visible to the model: blocked operations surface as ordinary command
	// failures, with no hint that a sandbox exists.
	shellName, shellArgs := shell.CommandLine(cmdText)
	cmd := sandbox.Command(runCtx, r.sandboxPolicy(), r.cwd, shellName, shellArgs...)
	cmd.Dir = r.cwd
	// Own process group, group kill on timeout/cancel, and a bounded wait for
	// the output pipes: a grandchild holding stdout (go test's test binaries,
	// a server started with &) can no longer hang the call past its deadline.
	shell.ConfigureProcessTree(cmd)
	out, err := cmd.CombinedOutput()
	text := r.spoolResult(toolID, string(out))
	status := shellFailureStatus(runCtx, cmd, err, timeout)
	if status == "" {
		if err != nil {
			// Exited 0, but a background process kept the output pipe open
			// past WaitDelay; anything it printed after that is not captured.
			text = appendToolStatus(text, "note: a background process kept the output open after the command exited; later output was not captured")
		}
		return text, nil
	}
	// The status goes after the (possibly truncated) output, never instead of
	// it: a failing build or test run is exactly when the model needs to read
	// what the command printed.
	return appendToolStatus(text, status), &toolOutputError{msg: status}
}

// shellFailureStatus classifies how a foreground command ended. It returns ""
// for success — including the case where the command exited 0 but a lingering
// child made Wait give up on the pipes (exec.ErrWaitDelay).
func shellFailureStatus(runCtx context.Context, cmd *exec.Cmd, err error, timeout time.Duration) string {
	if err == nil {
		return ""
	}
	switch ctxErr := runCtx.Err(); {
	case errors.Is(ctxErr, context.DeadlineExceeded):
		return fmt.Sprintf("command timed out after %s; process group killed", formatTimeoutSeconds(timeout))
	case errors.Is(ctxErr, context.Canceled):
		return "command cancelled; process group killed"
	}
	if errors.Is(err, exec.ErrWaitDelay) && cmd.ProcessState != nil && cmd.ProcessState.Success() {
		return ""
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if code := exitErr.ExitCode(); code >= 0 {
			return fmt.Sprintf("exit status %d", code)
		}
		return exitErr.String() // e.g. "signal: killed"
	}
	return err.Error()
}

// formatTimeoutSeconds renders a timeout as whole seconds ("120s").
func formatTimeoutSeconds(d time.Duration) string {
	return fmt.Sprintf("%ds", int((d+time.Second-1)/time.Second))
}

// maxShellTimeoutSec caps the per-call timeout a model may request. A
// configured default above it (goal_shell_timeout_sec) raises the cap to match.
const maxShellTimeoutSec = 600

// shellTimeout returns how long a foreground shell command may run: the
// configured default for the tool unless the call asked for its own limit, in
// which case that is clamped to [1s, max(maxShellTimeoutSec, default)].
//
// Models trained on harnesses whose timeout is in milliseconds send values
// like 120000. A request above both the ceiling and an hour is read as
// milliseconds: as seconds it would be clamped to the ceiling anyway. Anything
// up to the ceiling is always seconds, so a raised goal-mode ceiling
// (goal_shell_timeout_sec=7200) lets "timeout": 5400 mean 90 minutes rather
// than 6 seconds.
func (r *toolRuntime) shellTimeout(toolID string, requestedSec int) time.Duration {
	def := r.defaultToolTimeoutSec(toolID)
	if requestedSec <= 0 {
		return time.Duration(def) * time.Second
	}
	ceiling := max(maxShellTimeoutSec, def)
	if requestedSec > max(ceiling, 3600) {
		requestedSec = (requestedSec + 999) / 1000
	}
	return time.Duration(min(max(requestedSec, 1), ceiling)) * time.Second
}

// isForegroundShellCall reports whether a tool call runs a foreground shell
// command, whose deadlines runShellTool manages itself. Background jobs and
// bash-output polling a job are not: they return promptly and keep the
// ordinary per-tool deadline.
func (r *toolRuntime) isForegroundShellCall(call toolCall) bool {
	switch call.Tool {
	case "shell-exec", "bash", "bash-output":
	default:
		return false
	}
	var probe struct {
		JobID           string   `json:"job_id"`
		RunInBackground flexBool `json:"run_in_background"`
	}
	if json.Unmarshal(call.Args, &probe) != nil {
		// Malformed arguments fail in runShellTool before anything runs.
		return true
	}
	return strings.TrimSpace(probe.JobID) == "" && !bool(probe.RunInBackground)
}

// toolOutputError is returned by a tool whose output already describes the
// failure (a shell command's output ending in "[exit status 1]"). The result
// is still marked as an error, but the output is passed to the model as-is
// instead of being replaced by the error text.
type toolOutputError struct{ msg string }

func (e *toolOutputError) Error() string { return e.msg }

// appendToolStatus appends a bracketed status line to a tool's output.
func appendToolStatus(output, status string) string {
	if output == "" {
		return "[" + status + "]"
	}
	return strings.TrimRight(output, "\n") + "\n[" + status + "]"
}

// toolErrorOutput renders a failed call's result for the model. Output the
// tool produced before failing is kept — a failure's diagnostics usually live
// there — with the error appended; a tool that returned nothing gets the plain
// "error: ..." line.
func toolErrorOutput(output string, err error) string {
	var described *toolOutputError
	if errors.As(err, &described) {
		return output
	}
	if strings.TrimSpace(output) == "" {
		return "error: " + err.Error()
	}
	return strings.TrimRight(output, "\n") + "\nerror: " + err.Error()
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

func marshalSubagentResult(agentID string, result RunResult, merge *workspaceMerge) string {
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
	if merge != nil {
		ws := map[string]string{"merge_status": merge.Status, "branch": merge.Branch}
		if merge.Detail != "" {
			ws["detail"] = merge.Detail
		}
		// The worktree path only matters while it still exists (conflict or
		// error keeps it around for manual resolution).
		if merge.Status != "merged" && merge.Status != "no_changes" {
			ws["worktree"] = merge.Path
		}
		payload["workspace"] = ws
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

var alwaysAllowedCommandTokens = [][]string{
	{"ls"},
	{"pwd"},
	{"cat"},
	{"head"},
	{"tail"},
	{"wc"},
	{"grep"},
	{"rg"},
	{"stat"},
	{"git", "status"},
	{"git", "diff"},
	{"go", "test"},
	{"go", "build"},
	{"go", "vet"},
	{"make", "test"},
	{"make", "build"},
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
	needsApproval := r.perm() != config.PermissionYOLO
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
		if r.perm() != config.PermissionYOLO && isBlockedCommand(seg) {
			return fmt.Errorf("blocked dangerous command")
		}
		if isAlwaysAllowedCommand(seg) {
			continue
		}
		// YOLO mode bypasses all permission rules — every command is allowed.
		if r.perm() != config.PermissionYOLO {
			switch evaluatePermissionRule("execute", segNorm, r.runtimeRules, r.agentRules, toolRules) {
			case config.RuleDeny:
				r.emitApprovalTrace("denied", "policy", toolID, segNorm, "blocked by permission rules")
				return fmt.Errorf("shell-exec denied by policy for command segment %q", segNorm)
			case config.RuleAllow:
				continue
			}
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
		segments                            []string
		buf                                 strings.Builder
		inSingle, inDouble, inBacktick, esc bool
		subDepth                            int
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
			if !inDouble && !inBacktick {
				inSingle = !inSingle
			}
			buf.WriteByte(ch)
		case '"':
			if !inSingle && !inBacktick {
				inDouble = !inDouble
			}
			buf.WriteByte(ch)
		case '`':
			if !inSingle && !esc {
				inBacktick = !inBacktick
			}
			buf.WriteByte(ch)
		case '(':
			if !inSingle && !inDouble && !inBacktick && i > 0 && command[i-1] == '$' {
				subDepth++
			}
			buf.WriteByte(ch)
		case ')':
			if !inSingle && !inDouble && !inBacktick && subDepth > 0 {
				subDepth--
			}
			buf.WriteByte(ch)
		case ';':
			if inSingle || inDouble || inBacktick || subDepth > 0 {
				buf.WriteByte(ch)
				continue
			}
			flush()
		case '|':
			if inSingle || inDouble || inBacktick || subDepth > 0 {
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
			if inSingle || inDouble || inBacktick || subDepth > 0 {
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
			if inSingle || inDouble || inBacktick || subDepth > 0 {
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

func isAlwaysAllowedCommand(segment string) bool {
	if segmentHasUnsafeShellFeatures(segment) {
		return false
	}
	tokens := commandTokens(segment)
	if len(tokens) == 0 {
		return false
	}
	for _, allow := range alwaysAllowedCommandTokens {
		if len(tokens) < len(allow) {
			continue
		}
		match := true
		for i := range allow {
			if strings.ToLower(tokens[i]) != allow[i] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

func segmentHasUnsafeShellFeatures(segment string) bool {
	var (
		inSingle, inDouble, esc bool
	)
	for i := 0; i < len(segment); i++ {
		ch := segment[i]
		if esc {
			esc = false
			continue
		}
		if ch == '\\' && !inSingle {
			esc = true
			continue
		}
		switch ch {
		case '\'':
			if !inDouble {
				inSingle = !inSingle
			}
		case '"':
			if !inSingle {
				inDouble = !inDouble
			}
		case '`':
			if !inSingle {
				return true
			}
		case '$':
			if !inSingle && i+1 < len(segment) && segment[i+1] == '(' {
				return true
			}
		case '&':
			if !inSingle && !inDouble {
				if i+1 >= len(segment) || segment[i+1] != '&' {
					return true
				}
			}
		case '<', '>':
			if !inSingle && !inDouble {
				return true
			}
		}
	}
	return false
}

func commandTokens(segment string) []string {
	tokens := lexShellTokens(segment)
	idx := 0
	for idx < len(tokens) {
		t := tokens[idx]
		if t == "" {
			idx++
			continue
		}
		if t == "env" {
			idx++
			continue
		}
		if looksLikeEnvAssignment(t) {
			idx++
			continue
		}
		break
	}
	if idx >= len(tokens) {
		return nil
	}
	return tokens[idx:]
}

func isDangerousRM(command string) bool {
	tokens := commandTokens(command)
	if len(tokens) == 0 {
		return false
	}
	idx := 0
	if strings.ToLower(tokens[idx]) == "sudo" {
		idx++
	}
	if idx >= len(tokens) {
		return false
	}
	cmd := strings.ToLower(tokens[idx])
	if cmd != "rm" && !strings.HasSuffix(cmd, "/rm") {
		return false
	}
	idx++
	hasR := false
	hasF := false
	targetsRoot := false
	noPreserveRoot := false
	for ; idx < len(tokens); idx++ {
		t := tokens[idx]
		if t == "" {
			continue
		}
		l := strings.ToLower(t)
		if strings.HasPrefix(l, "-") {
			switch l {
			case "--recursive":
				hasR = true
			case "--force":
				hasF = true
			case "--no-preserve-root":
				// This flag exists only to bypass GNU rm's refusal to delete
				// "/"; there is no legitimate reason for an agent to pass it.
				noPreserveRoot = true
			}
			if strings.HasPrefix(l, "-") && !strings.HasPrefix(l, "--") {
				for i := 1; i < len(l); i++ {
					switch l[i] {
					case 'r':
						hasR = true
					case 'f':
						hasF = true
					}
				}
			}
			continue
		}
		switch l {
		case "/", "/.", "/..", "/*":
			targetsRoot = true
		}
	}
	return noPreserveRoot || (hasR && hasF && targetsRoot)
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
	// This file records which shell commands the user pre-approved for the
	// project, so it is owner-only (0o600) like the other ~/.spettro secrets
	// stores rather than world-readable.
	if err := fsperm.SecureMkdirAll(filepath.Dir(path)); err != nil {
		return fmt.Errorf("create .spettro dir: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return fmt.Errorf("write allowed commands temp: %w", err)
	}
	return safeio.Replace(tmp, path)
}
