package agent

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"spettro/internal/workflow"
)

// Trace names hosts key their workflow rendering off. The lifecycle trace is a
// single running→finished pair (so ACP clients get one tool call spanning the
// whole run), while progress traces are instantaneous timeline entries.
const (
	workflowTraceName         = "workflow"
	workflowProgressTraceName = "workflow-progress"
)

// workflowObserver turns engine events into ToolTraces. Hosts already
// understand traces — the TUI panel, the ACP bridge and the session log all
// consume the same stream — so a workflow needs no transport of its own.
//
// rt is the runtime of the turn currently driving the run. A run paused at a
// checkpoint outlives the tool call (and possibly the turn) that started it,
// so rt is swapped by rebind when a later call continues it, and cleared while
// nobody does: a callback belonging to a finished turn must never be called.
type workflowObserver struct {
	mu    sync.RWMutex
	rt    *toolRuntime
	runID string
	meta  workflow.Meta
	// origin, size and budget describe the run for the lifecycle trace; they
	// are repeated on a resumed trace so a host that lost the run's state (a
	// new ACP turn) can rebuild its header.
	origin     string
	sizeTier   string
	sizeAgents int
	budget     int
}

// rebind points the observer at rt (nil detaches it). It waits for emits in
// flight against the old runtime, so once it returns the old turn's callback
// is not being called any more.
func (o *workflowObserver) rebind(rt *toolRuntime) {
	o.mu.Lock()
	o.rt = rt
	o.mu.Unlock()
}

func (o *workflowObserver) emit(name, status string, payload map[string]any, output string) {
	o.mu.RLock()
	defer o.mu.RUnlock()
	if o.rt == nil || o.rt.toolCallback == nil {
		return
	}
	payload["run_id"] = o.runID
	payload["workflow"] = o.meta.Name
	args, _ := json.Marshal(payload)
	o.rt.toolCallback(ToolTrace{
		AgentID: o.rt.traceID(),
		Name:    name,
		Status:  status,
		Args:    string(args),
		Output:  truncate(output, 600),
	})
}

// startPayload is the lifecycle trace's description of the run: what it is,
// its declared plan, and the sizing it runs under.
func (o *workflowObserver) startPayload() map[string]any {
	phases := make([]map[string]string, 0, len(o.meta.Phases))
	for _, p := range o.meta.Phases {
		phases = append(phases, map[string]string{"title": p.Title, "detail": p.Detail})
	}
	params := make([]map[string]any, 0, len(o.meta.Params))
	for _, p := range o.meta.Params {
		params = append(params, map[string]any{
			"name": p.Name, "type": p.Type, "required": p.Required, "description": p.Description,
		})
	}
	return map[string]any{
		"description":   o.meta.Description,
		"phases":        phases,
		"origin":        o.origin,
		"size":          o.sizeTier,
		"size_agents":   o.sizeAgents,
		"budget_tokens": o.budget,
		"params":        params,
	}
}

// start opens the lifecycle trace. Phases are published up front, from the
// declared meta, so a host can draw the whole plan before the first agent runs
// instead of growing it one phase at a time.
func (o *workflowObserver) start() {
	o.emit(workflowTraceName, "running", o.startPayload(), "")
}

// resumed reopens the lifecycle trace when a paused run is continued. It is a
// "running" trace like start, marked resumed so hosts keep the run's phases,
// members and log instead of resetting them for what looks like a new run.
func (o *workflowObserver) resumed() {
	payload := o.startPayload()
	payload["resumed"] = true
	o.emit(workflowTraceName, "running", payload, "")
}

// paused closes the tool call's share of the lifecycle: the run is waiting on
// the orchestrator, not finished, so it gets a status of its own that hosts
// must not read as done.
func (o *workflowObserver) paused(cp workflow.Checkpoint) {
	o.emit(workflowTraceName, "paused", map[string]any{
		"checkpoint_id": cp.ID,
		"message":       cp.Message,
		"phase":         cp.Phase,
		"auto":          cp.Auto,
	}, cp.Message)
}

func (o *workflowObserver) finish(res workflow.Result, err error) {
	status := "success"
	output := fmt.Sprintf("%d agents · %d failed · %d replayed", res.Agents, res.Failed, res.Cached)
	if err != nil {
		status = "error"
		output = err.Error()
	}
	o.emit(workflowTraceName, status, map[string]any{
		"agents": res.Agents,
		"failed": res.Failed,
		"cached": res.Cached,
		"tokens": res.Tokens,
	}, output)
}

// stopped closes the lifecycle of a run stopped on purpose — by the
// orchestrator, the idle reaper or the host. It is a status of its own, not
// "error": the run did not fail, and hosts render it as a neutral end with
// the reason rather than as a red failure.
func (o *workflowObserver) stopped(res workflow.Result, reason string) {
	o.emit(workflowTraceName, "stopped", map[string]any{
		"reason": reason,
		"agents": res.Agents,
		"failed": res.Failed,
		"cached": res.Cached,
		"tokens": res.Tokens,
	}, "stopped: "+reason)
}

func (o *workflowObserver) handle(ev workflow.Event) {
	switch ev.Kind {
	case workflow.EventPhase:
		o.emit(workflowProgressTraceName, "success", map[string]any{
			"kind":    "phase",
			"phase":   ev.Phase,
			"detail":  ev.Detail,
			"dynamic": ev.Dynamic,
		}, ev.Phase)
	case workflow.EventLog:
		o.emit(workflowProgressTraceName, "success", map[string]any{
			"kind":  "log",
			"phase": ev.Phase,
		}, ev.Message)
	case workflow.EventCheckpoint:
		if ev.Cached {
			// A resumed run replaying an answered checkpoint does not pause;
			// a "checkpoint" entry would make hosts show it as waiting.
			o.emit(workflowProgressTraceName, "success", map[string]any{
				"kind":  "log",
				"phase": ev.Phase,
			}, fmt.Sprintf("checkpoint %s replayed from the journal: %s", ev.CheckpointID, ev.Message))
			return
		}
		o.emit(workflowProgressTraceName, "success", map[string]any{
			"kind":          "checkpoint",
			"phase":         ev.Phase,
			"checkpoint_id": ev.CheckpointID,
			"message":       ev.Message,
			"auto":          ev.Auto,
		}, ev.Output)
	case workflow.EventResume:
		if ev.Cached {
			return
		}
		o.emit(workflowProgressTraceName, "success", map[string]any{
			"kind":  "log",
			"phase": ev.Phase,
		}, fmt.Sprintf("checkpoint %s answered", ev.CheckpointID))
	case workflow.EventAgentStart:
		o.emitAgent(ev, "running", "")
	case workflow.EventAgentDone:
		o.emitAgent(ev, "success", ev.Output)
	case workflow.EventAgentError:
		o.emitAgent(ev, "error", ev.Message)
	}
}

// emitAgent publishes a workflow member's lifecycle as an "agent" trace — the
// same shape delegation produces — with the workflow fields hosts use
// to group it under its phase.
func (o *workflowObserver) emitAgent(ev workflow.Event, status, output string) {
	o.mu.RLock()
	defer o.mu.RUnlock()
	if o.rt == nil || o.rt.toolCallback == nil {
		return
	}
	args, _ := json.Marshal(map[string]any{
		"agent":           ev.Instance,
		"task":            ev.Label,
		"parent_agent_id": o.rt.traceID(),
		"workflow":        o.meta.Name,
		"run_id":          o.runID,
		"phase":           ev.Phase,
		"index":           ev.Index,
		"cached":          ev.Cached,
	})
	o.rt.toolCallback(ToolTrace{
		AgentID: ev.Instance,
		Name:    "agent",
		Status:  status,
		Args:    string(args),
		Output:  truncate(output, 600),
	})
}

// renderWorkflowResult is what the model reads when the tool returns. It is
// the script's return value first — that is the answer the script computed —
// framed by enough run metadata to re-run, resume, or explain the run.
func renderWorkflowResult(runID, dir, origin string, meta workflow.Meta, res workflow.Result, mergeNotes []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "<workflow_result name=%q run_id=%q>\n", meta.Name, runID)
	fmt.Fprintf(&b, "<summary>%d agents · %d failed · %d replayed from journal · %d tokens</summary>\n",
		res.Agents, res.Failed, res.Cached, res.Tokens)
	if len(res.Phases) > 0 {
		fmt.Fprintf(&b, "<phases>%s</phases>\n", strings.Join(res.Phases, " → "))
	}
	if len(res.Logs) > 0 {
		b.WriteString("<log>\n")
		for _, line := range res.Logs {
			b.WriteString(truncate(line, 300))
			b.WriteByte('\n')
		}
		b.WriteString("</log>\n")
	}
	b.WriteString("<returned>\n")
	b.WriteString(truncate(encodeWorkflowValue(res.Value), 24000))
	b.WriteString("\n</returned>\n")
	if len(mergeNotes) > 0 {
		b.WriteString("<unmerged>\n")
		for _, n := range mergeNotes {
			b.WriteString(truncate(n, 600))
			b.WriteByte('\n')
		}
		b.WriteString("</unmerged>\n")
	}
	b.WriteString("</workflow_result>")
	fmt.Fprintf(&b, "\nScript: %s · transcript: %s", origin, dir)
	b.WriteString(fmt.Sprintf("\nTo resume after an edit, re-run with script_path and resume_from_run_id=%q: unchanged calls replay from the journal instead of re-running.", runID))
	if len(mergeNotes) > 0 {
		b.WriteString("\nSome sub-agent workspaces did not merge back. Their work is on the branches listed above: resolve each one yourself (merge it, fix conflicts, commit), then delete the branch and its worktree.")
	}
	if res.Failed > 0 {
		b.WriteString("\nSome agents failed and resolved to null in the script. Check the returned value for gaps before trusting it, and re-dispatch what is missing.")
	}
	return b.String()
}

func encodeWorkflowValue(v any) string {
	if v == nil {
		return "(the script returned nothing)"
	}
	if s, ok := v.(string); ok {
		return s
	}
	encoded, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(encoded)
}

// workflowCheckpointLogLines is how many of the run's latest log lines a
// checkpoint result carries: enough to show what led up to the question
// without replaying a long run's whole log on every pause.
const workflowCheckpointLogLines = 20

// renderWorkflowCheckpoint is what the model reads when the run pauses at a
// checkpoint. The script is asking the orchestrator a question, so the
// message and data come first, then just enough progress to judge them, then
// the exact call that answers — a model that has to guess the continue
// syntax will guess wrong.
//
// turnLocal says the run lives in a registry that dies with the turn (a host
// that owns none: headless, remote relays). There the model must answer
// before it ends its turn — ending it to ask the user stops the run — and
// being promised the half hour a host-owned run waits would tell it the
// opposite.
func renderWorkflowCheckpoint(runID string, meta workflow.Meta, cp workflow.Checkpoint, snap workflow.Result, turnLocal bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "<workflow_checkpoint name=%q run_id=%q checkpoint_id=%q phase=%q", meta.Name, runID, cp.ID, cp.Phase)
	if cp.Auto {
		b.WriteString(` auto="true"`)
	}
	b.WriteString(">\n")
	fmt.Fprintf(&b, "<message>%s</message>\n", cp.Message)
	if cp.Data != nil {
		b.WriteString("<data>\n")
		b.WriteString(truncate(encodeWorkflowValue(cp.Data), 24000))
		b.WriteString("\n</data>\n")
	}
	fmt.Fprintf(&b, "<progress>%d agents · %d failed · %d replayed · %d tokens", snap.Agents, snap.Failed, snap.Cached, snap.Tokens)
	if len(snap.Phases) > 0 {
		fmt.Fprintf(&b, "; phases: %s", strings.Join(snap.Phases, " → "))
	}
	b.WriteString("</progress>\n")
	if logs := snap.Logs; len(logs) > 0 {
		if len(logs) > workflowCheckpointLogLines {
			logs = logs[len(logs)-workflowCheckpointLogLines:]
		}
		b.WriteString("<log>\n")
		for _, line := range logs {
			b.WriteString(truncate(line, 300))
			b.WriteByte('\n')
		}
		b.WriteString("</log>\n")
	}
	b.WriteString("</workflow_checkpoint>\n")
	if cp.Auto {
		fmt.Fprintf(&b, "The run finished a phase and is paused before the next one. Review what it did, then call the workflow tool with {\"continue_run_id\":%q} to go on into the next phase, or {\"continue_run_id\":%q,\"stop\":true} to end the run here.", runID, runID)
	} else {
		fmt.Fprintf(&b, "The run is paused and waiting for you. Read the data, then call the workflow tool with {\"continue_run_id\":%q,\"reply\":<value>} to continue (the script receives reply as checkpoint()'s return value), or {\"continue_run_id\":%q,\"stop\":true} to stop it.", runID, runID)
	}
	if turnLocal {
		b.WriteString(" No agent runs while it waits. Answer it in this turn: the run is stopped when your turn ends, so do not end the turn to ask the user about it — decide yourself, or stop the run and ask (its journal stays resumable with resume_from_run_id).")
		return b.String()
	}
	fmt.Fprintf(&b, " No agent runs while it waits; a run left paused for %s is stopped (its journal stays resumable with resume_from_run_id).", formatIdle(workflowIdleTimeout))
	return b.String()
}

// formatIdle renders the idle timeout the way a person would say it.
func formatIdle(d time.Duration) string {
	if d >= time.Minute && d%time.Minute == 0 {
		return fmt.Sprintf("%d minutes", int(d/time.Minute))
	}
	return d.String()
}
