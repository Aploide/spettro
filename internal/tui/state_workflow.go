package tui

import (
	"encoding/json"
	"strings"
	"time"
)

// Workflow runs get their own state rather than folding into parallelAgents:
// a workflow has structure a flat agent list cannot express — declared phases
// that exist before any agent starts, agents that belong to a phase, and log
// lines the script emitted between them — and the panel is only legible if it
// can show that structure.

type workflowAgentEntry struct {
	Instance string
	Label    string
	Phase    string
	Index    int
	Status   string // "running", "done", "failed"
	Cached   bool
	Detail   string
}

type workflowPhaseEntry struct {
	Title  string
	Detail string
	// Dynamic marks a phase the script opened at runtime rather than one meta
	// declared up front. Adaptive scripts decide their shape as they go, and
	// the reader should be able to tell a planned stage from one the run
	// added after seeing interim results.
	Dynamic bool
}

type workflowLogEntry struct {
	Phase   string
	Message string
	At      time.Time
}

type workflowRun struct {
	RunID       string
	Name        string
	Description string
	Origin      string
	Phases      []workflowPhaseEntry
	Agents      []workflowAgentEntry
	Logs        []workflowLogEntry
	Status      string // "running", "paused", "done", "failed"
	Summary     string
	StartedAt   time.Time
	FinishedAt  time.Time
	// Size is the run's size tier (small/medium/large/unbounded) and
	// BudgetTokens its token ceiling (0 = none). Both bound how far the
	// script may grow, so the title carries them.
	Size         string
	BudgetTokens int
	// CheckpointID and CheckpointMessage describe where a paused run is
	// waiting for the orchestrating model; PausedAt is when it stopped.
	CheckpointID      string
	CheckpointMessage string
	PausedAt          time.Time
}

func (w *workflowRun) counts() (running, done, failed, cached int) {
	for _, a := range w.Agents {
		switch a.Status {
		case "running":
			running++
		case "failed":
			failed++
		default:
			done++
		}
		if a.Cached {
			cached++
		}
	}
	return
}

// phaseOrder returns the phases to render: the ones meta declared, in declared
// order, followed by any a script entered that the header did not mention.
// A script is free to call phase() with a title meta never listed, and
// silently dropping those agents from the panel would be worse than showing an
// undeclared group.
func (w *workflowRun) phaseOrder() []string {
	var order []string
	seen := map[string]bool{}
	for _, p := range w.Phases {
		if !seen[p.Title] {
			seen[p.Title] = true
			order = append(order, p.Title)
		}
	}
	for _, a := range w.Agents {
		if a.Phase != "" && !seen[a.Phase] {
			seen[a.Phase] = true
			order = append(order, a.Phase)
		}
	}
	// Agents dispatched outside any phase collect under a final unnamed group.
	for _, a := range w.Agents {
		if a.Phase == "" {
			order = append(order, "")
			break
		}
	}
	return order
}

// phaseEntry returns the recorded entry for a phase title; a phase known only
// from its agents has none, and gets the zero entry.
func (w *workflowRun) phaseEntry(title string) workflowPhaseEntry {
	for _, p := range w.Phases {
		if p.Title == title {
			return p
		}
	}
	return workflowPhaseEntry{}
}

func (w *workflowRun) agentsInPhase(phase string) []workflowAgentEntry {
	var out []workflowAgentEntry
	for _, a := range w.Agents {
		if a.Phase == phase {
			out = append(out, a)
		}
	}
	return out
}

type workflowTraceArgs struct {
	RunID       string `json:"run_id"`
	Workflow    string `json:"workflow"`
	Description string `json:"description"`
	Origin      string `json:"origin"`
	Kind        string `json:"kind"`
	Phase       string `json:"phase"`
	Phases      []struct {
		Title  string `json:"title"`
		Detail string `json:"detail"`
	} `json:"phases"`
	// Phase progress: the optional detail phase(title, {detail}) passed, and
	// whether the title was missing from meta.phases.
	Detail  string `json:"detail"`
	Dynamic bool   `json:"dynamic"`
	// Run start: the size tier and token budget the run was given.
	Size         string `json:"size"`
	BudgetTokens int    `json:"budget_tokens"`
	// Checkpoints: the pause the run waits at, and — on a "running" trace —
	// whether this continues a run already on screen.
	CheckpointID string `json:"checkpoint_id"`
	Message      string `json:"message"`
	Resumed      bool   `json:"resumed"`
	// Cached is a bool on a checkpoint trace (answered from the journal) but
	// a count on the finish trace, so it is decoded loosely.
	Cached any `json:"cached"`
}

// applyWorkflowTrace folds a workflow lifecycle or progress trace into the
// panel state. Returns false when the trace is not a workflow trace.
func (m *Model) applyWorkflowTrace(name, argsJSON, output, status string) bool {
	switch name {
	case "workflow", "workflow-progress":
	default:
		return false
	}
	var args workflowTraceArgs
	_ = json.Unmarshal([]byte(argsJSON), &args)

	// The tool loop also reports every call under the tool's own name, so the
	// workflow tool's call arrives here as a "workflow" trace whose args are
	// the tool input (name, script, continue_run_id …), not the observer's
	// lifecycle payload. Folding it in opened an empty run before the real
	// start arrived and overwrote the summary with the raw result XML — and
	// with checkpoints it would mark a paused run done the moment the tool
	// call returned. Only the observer's traces name the run. (Progress
	// traces need no such check — no tool is called "workflow-progress" —
	// and a workspace merge note legitimately carries no run fields.)
	if name == "workflow" {
		if args.Workflow == "" && args.RunID == "" {
			return true
		}
		return m.applyWorkflowLifecycle(args, output, status)
	}

	if m.workflow == nil {
		return true
	}
	switch args.Kind {
	case "phase":
		title := strings.TrimSpace(args.Phase)
		if title == "" {
			return true
		}
		detail := strings.TrimSpace(args.Detail)
		for i, p := range m.workflow.Phases {
			if p.Title == title {
				// A declared phase entered with a detail its header lacked:
				// keep whichever says more.
				if p.Detail == "" {
					m.workflow.Phases[i].Detail = detail
				}
				return true
			}
		}
		m.workflow.Phases = append(m.workflow.Phases, workflowPhaseEntry{
			Title: title, Detail: detail, Dynamic: args.Dynamic,
		})
	case "log":
		if msg := strings.TrimSpace(output); msg != "" {
			m.workflow.Logs = append(m.workflow.Logs, workflowLogEntry{
				Phase: args.Phase, Message: msg, At: time.Now(),
			})
		}
	case "checkpoint":
		// The pause itself is the "paused" lifecycle trace that follows when
		// the tool call returns; this one records that the script asked. A
		// replayed checkpoint (a resumed run answering from its journal)
		// never pauses, so treating this trace as the pause would strand the
		// panel on "paused" while the run carries on.
		msg := strings.TrimSpace(args.Message)
		if msg == "" {
			msg = args.CheckpointID
		}
		if args.Cached == true {
			msg = "replayed · " + msg
		}
		m.workflow.Logs = append(m.workflow.Logs, workflowLogEntry{
			Phase: args.Phase, Message: "⏸ " + msg, At: time.Now(),
		})
	}
	return true
}

// applyWorkflowLifecycle folds the observer's "workflow" traces: a run
// starting or being continued, pausing at a checkpoint, and settling.
func (m *Model) applyWorkflowLifecycle(args workflowTraceArgs, output, status string) bool {
	if status == "running" {
		// A continue of the run on screen keeps everything the panel already
		// knows: its phases, agents and log are what the reader needs to
		// follow what the orchestrator decided at the checkpoint.
		if args.Resumed && m.workflow != nil && m.workflow.RunID == args.RunID {
			m.workflow.Status = "running"
			m.workflow.CheckpointID = ""
			m.workflow.CheckpointMessage = ""
			m.workflow.PausedAt = time.Time{}
			return true
		}
		run := &workflowRun{
			RunID:        args.RunID,
			Name:         args.Workflow,
			Description:  args.Description,
			Origin:       args.Origin,
			Status:       "running",
			StartedAt:    time.Now(),
			Size:         args.Size,
			BudgetTokens: args.BudgetTokens,
		}
		for _, p := range args.Phases {
			run.Phases = append(run.Phases, workflowPhaseEntry{Title: p.Title, Detail: p.Detail})
		}
		m.workflow = run
		if !m.showSidePanel && !args.Resumed {
			m.showBanner("workflow "+run.Name+m.panelKeyHint(" — ", "the phase tree"), "info")
		}
		return true
	}
	if m.workflow == nil {
		return true
	}
	// A pause or settle for some other run — one a newer run replaced on
	// screen — must not rewrite the run being shown.
	if args.RunID != "" && m.workflow.RunID != "" && args.RunID != m.workflow.RunID {
		return true
	}
	if status == "paused" {
		m.workflow.Status = "paused"
		m.workflow.CheckpointID = args.CheckpointID
		m.workflow.CheckpointMessage = strings.TrimSpace(args.Message)
		m.workflow.PausedAt = time.Now()
		return true
	}
	m.workflow.Status = "done"
	if status == "error" {
		m.workflow.Status = "failed"
	}
	m.workflow.CheckpointID = ""
	m.workflow.CheckpointMessage = ""
	m.workflow.Summary = strings.TrimSpace(output)
	m.workflow.FinishedAt = time.Now()
	return true
}

// clearSettledWorkflow drops the workflow tree when a new turn starts, unless
// the run is paused at a checkpoint and still alive: the next turn is then
// most likely the orchestrator answering it, and the tree is what the reader
// needs to follow that. A paused run the registry no longer holds (stopped by
// the idle reaper, or by /clear) is over, and so is its tree.
func (m *Model) clearSettledWorkflow() {
	if w := m.workflow; w != nil && w.Status == "paused" && m.workflowRunLive(w.RunID) {
		return
	}
	m.workflow = nil
}

// workflowRunLive reports whether runID is still waiting in the session's run
// registry. Without a registry there is nothing to ask, so the panel trusts
// the traces it has seen.
func (m *Model) workflowRunLive(runID string) bool {
	if m.workflowRuns == nil {
		return true
	}
	for _, p := range m.workflowRuns.Paused() {
		if p.RunID == runID {
			return true
		}
	}
	return false
}

type workflowAgentArgs struct {
	Agent    string `json:"agent"`
	Task     string `json:"task"`
	Workflow string `json:"workflow"`
	RunID    string `json:"run_id"`
	Phase    string `json:"phase"`
	Index    int    `json:"index"`
	Cached   bool   `json:"cached"`
}

// applyWorkflowAgentTrace records a workflow member's lifecycle. Returns false
// for agent traces that are not part of a workflow, which keeps ordinary
// delegation and Ultra swarms on their existing path.
func (m *Model) applyWorkflowAgentTrace(argsJSON, output, status string) bool {
	var args workflowAgentArgs
	if json.Unmarshal([]byte(argsJSON), &args) != nil || args.Workflow == "" || args.Agent == "" {
		return false
	}
	if m.workflow == nil {
		return false
	}
	entryStatus := "done"
	switch status {
	case "running":
		entryStatus = "running"
	case "error":
		entryStatus = "failed"
	}
	for i := range m.workflow.Agents {
		if m.workflow.Agents[i].Instance == args.Agent {
			m.workflow.Agents[i].Status = entryStatus
			if entryStatus == "failed" {
				m.workflow.Agents[i].Detail = truncateLabel(strings.TrimSpace(output), 160)
			}
			return true
		}
	}
	m.workflow.Agents = append(m.workflow.Agents, workflowAgentEntry{
		Instance: args.Agent,
		Label:    args.Task,
		Phase:    args.Phase,
		Index:    args.Index,
		Status:   entryStatus,
		Cached:   args.Cached,
	})
	return true
}
