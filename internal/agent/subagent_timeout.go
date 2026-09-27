package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"spettro/internal/config"
	"spettro/internal/provider"
)

// Sub-agent deadlines.
//
// The agent tool's limit comes from the manifest (tools.agent.timeout_sec).
// Its shipped value, 300s, suits a read-only explorer but not an
// implementation worker that edits several files and runs the tests, so a
// worker delegated under the shipped default gets codingAgentTimeoutSec
// instead. A value the user picked themselves is honoured as is, so the
// manifest schema does not change.
//
// Running out of time no longer throws the work away. Shortly before the
// deadline the sub-agent is told to wrap up; if it still does not finish,
// the parent receives what it has — the sub-agent's last words, the files it
// wrote and its most recent tool results — flagged as timed out. Changes a
// sub-agent made in the shared checkout are never rolled back, so without
// this report the parent would not even know they exist.

const (
	// shippedAgentTimeoutSec is the agent tool's timeout_sec in the default
	// manifest; a manifest still carrying it has not been tuned by the user.
	shippedAgentTimeoutSec = 300
	// codingAgentTimeoutSec is the default limit for sub-agents that can
	// write files or run commands.
	codingAgentTimeoutSec = 900
	// agentWrapUpFraction is how far into its time budget a sub-agent is
	// told to stop starting new work and report.
	agentWrapUpFraction = 0.8
)

// subagentTimeUnit scales timeout_sec into a duration (a test seam).
var subagentTimeUnit = time.Second

// agentWrapUpMessage is delivered to a sub-agent nearing its deadline.
const agentWrapUpMessage = "Time check: you are close to your time limit (about %s left). Do not start new work. Finish or revert the edit in progress so no file is left half-changed, then reply now with a summary: what you changed (files), what you verified, and what remains undone."

// codingAgentTools are the tools that make a sub-agent an implementation
// worker for timeout purposes, by canonical name: an allow-list entry is
// resolved through CanonicalToolName, so a retired name (multi-edit,
// shell-exec, bash-output) counts as its canonical tool.
var codingAgentTools = []string{"file-write", "file-edit", "bash"}

// isCodingAgent reports whether a sub-agent can modify the workspace.
func isCodingAgent(spec config.AgentSpec) bool {
	for _, t := range spec.AllowedTools {
		if slices.Contains(codingAgentTools, CanonicalToolName(t)) {
			return true
		}
	}
	return false
}

// agentTimeout is the wall-clock limit for one delegated sub-agent run.
func (r *toolRuntime) agentTimeout(spec config.AgentSpec) time.Duration {
	sec := r.defaultToolTimeoutSec("agent")
	configured := 0
	if p, ok := r.toolPolicies["agent"]; ok {
		configured = p.TimeoutSec
	}
	untuned := configured == 0 || configured == shippedAgentTimeoutSec
	if untuned && isCodingAgent(spec) && sec < codingAgentTimeoutSec {
		sec = codingAgentTimeoutSec
	}
	return time.Duration(sec) * subagentTimeUnit
}

// scheduleWrapUp pushes the wrap-up notice onto q when agentWrapUpFraction of
// limit has elapsed. The returned stop cancels it.
func scheduleWrapUp(q *SteeringQueue, limit time.Duration) (stop func() bool) {
	at := time.Duration(float64(limit) * agentWrapUpFraction)
	left := (limit - at).Round(time.Second)
	if left <= 0 {
		left = limit - at
	}
	t := time.AfterFunc(at, func() { q.Push(fmt.Sprintf(agentWrapUpMessage, left)) })
	return t.Stop
}

// subagentTimedOut reports whether a sub-agent run ended because its own
// deadline passed, not because the parent was cancelled.
func subagentTimedOut(parent, run context.Context) bool {
	return parent.Err() == nil && run.Err() == context.DeadlineExceeded
}

// lastAssistantText is the most recent non-empty assistant prose in a
// conversation, think blocks removed.
func lastAssistantText(msgs []provider.Message) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role != provider.RoleAssistant {
			continue
		}
		text, _ := stripThinkTags(stripLeakedToolCalls(msgs[i].Content))
		if text = strings.TrimSpace(text); text != "" {
			return text
		}
	}
	return ""
}

// fileWriteTools are the tools whose successful trace names a file written.
var fileWriteTools = map[string]bool{"file-write": true, "file-edit": true, "multi-edit": true}

// modifiedFilesFromTraces lists, in first-write order, the files a run's
// successful file tools wrote.
func modifiedFilesFromTraces(traces []ToolTrace) []string {
	var files []string
	for _, tr := range traces {
		if !fileWriteTools[tr.Name] || tr.Status != "success" {
			continue
		}
		var probe struct {
			Path     string `json:"path"`
			FilePath string `json:"file_path"`
		}
		if json.Unmarshal([]byte(tr.Args), &probe) != nil {
			continue
		}
		if p := firstNonEmpty(probe.Path, probe.FilePath); p != "" && !slices.Contains(files, p) {
			files = append(files, p)
		}
	}
	return files
}

// shellCommandsFromTraces lists the shell commands a run executed that were
// not provably read-only: they may have changed files the traces cannot name.
func shellCommandsFromTraces(traces []ToolTrace, limit int) []string {
	var cmds []string
	for _, tr := range traces {
		if (tr.Name != "shell-exec" && tr.Name != "bash") || tr.Status == "running" {
			continue
		}
		if isReadOnlyShellCall(json.RawMessage(tr.Args)) {
			continue
		}
		var probe struct {
			Command string `json:"command"`
			Cmd     string `json:"cmd"`
		}
		if json.Unmarshal([]byte(tr.Args), &probe) != nil {
			continue
		}
		if c := firstNonEmpty(probe.Command, probe.Cmd); c != "" {
			cmds = append(cmds, truncate(c, 200))
		}
	}
	if len(cmds) > limit {
		cmds = cmds[len(cmds)-limit:]
	}
	return cmds
}

// marshalSubagentPartial reports a sub-agent run that did not finish:
// status "timed_out" when it ran out of time, "failed" otherwise. It carries
// everything the parent needs to pick the work up instead of redoing it.
func marshalSubagentPartial(agentID, status, reason string, result RunResult, files []string, merge *workspaceMerge) string {
	payload := map[string]any{
		"agent":            agentID,
		"status":           status,
		"partial":          true,
		"error":            reason,
		"summary":          truncate(lastAssistantText(result.Messages), 4000),
		"tool_trace_count": len(result.Tools),
		"tokens_used":      result.TokensUsed,
	}
	switch status {
	case "timed_out":
		payload["note"] = "The sub-agent ran out of time before finishing. Its changes were kept, not rolled back: check files_modified (and any shell commands) before continuing the task yourself or delegating the remainder."
	default:
		payload["note"] = "The sub-agent stopped with an error before finishing. Its changes were kept, not rolled back: check files_modified (and any shell commands) before retrying."
	}
	if len(files) > 0 {
		payload["files_modified"] = files
	}
	if cmds := shellCommandsFromTraces(result.Tools, 10); len(cmds) > 0 {
		payload["shell_commands"] = cmds
	}
	if tail := summarizeSubagentToolResultsTail(result.Tools, 6); len(tail) > 0 {
		payload["last_tool_results"] = tail
	}
	if merge != nil {
		ws := map[string]string{"merge_status": merge.Status, "branch": merge.Branch}
		if merge.Detail != "" {
			ws["detail"] = merge.Detail
		}
		if merge.Path != "" {
			ws["worktree"] = merge.Path
		}
		payload["workspace"] = ws
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Sprintf("{\"agent\":%q,\"status\":%q,\"partial\":true}", agentID, status)
	}
	return string(raw)
}

// summarizeSubagentToolResultsTail is summarizeSubagentToolResults over the
// last limit finished calls: for an unfinished run the latest results say
// where it stopped.
func summarizeSubagentToolResultsTail(traces []ToolTrace, limit int) []map[string]string {
	var finished []ToolTrace
	for _, tr := range traces {
		if tr.Status == "running" || tr.Name == "comment" {
			continue
		}
		finished = append(finished, tr)
	}
	if len(finished) > limit {
		finished = finished[len(finished)-limit:]
	}
	return summarizeSubagentToolResults(finished, limit)
}

// changedFiles lists the files the workspace differs from its fork point in:
// committed, uncommitted and untracked (ignored files excluded).
func (w *agentWorkspace) changedFiles(ctx context.Context) []string {
	var files []string
	add := func(out string) {
		for _, f := range strings.Split(out, "\n") {
			if f = strings.TrimSpace(f); f != "" && !slices.Contains(files, f) {
				files = append(files, f)
			}
		}
	}
	if out, err := workspaceGit(ctx, w.path, "diff", "--name-only", w.baseRef); err == nil {
		add(out)
	}
	if out, err := workspaceGit(ctx, w.path, "ls-files", "--others", "--exclude-standard"); err == nil {
		add(out)
	}
	return files
}
