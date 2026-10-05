package tui

import (
	"encoding/json"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
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
	// Status is "running", "paused", "done", "failed" or "stopped" — the
	// last for a run ended on purpose (the orchestrator's stop, the idle
	// reaper, the session going away), which did not fail.
	Status     string
	Summary    string
	StartedAt  time.Time
	FinishedAt time.Time
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
	// Reason is why a "stopped" run was stopped.
	Reason string `json:"reason"`
}

// workflowToolInputKeys are the workflow tool's own arguments. The observer
// never puts one in a lifecycle payload, so a "workflow" trace carrying any
// of them is the tool loop reporting the call itself.
var workflowToolInputKeys = []string{
	"script", "script_path", "name", "args", "resume_from_run_id", "max_concurrency",
	"save_as", "save_scope", "continue_run_id", "reply", "stop", "auto_checkpoint",
}

// workflowLifecycleMarkers are, per lifecycle status, payload keys only the
// observer writes: a start or continue describes the run, a pause names its
// checkpoint, a settle reports what the run did.
var workflowLifecycleMarkers = map[string][]string{
	"running": {"origin", "phases", "description", "params", "size_agents", "resumed"},
	"paused":  {"checkpoint_id", "message"},
	"settled": {"agents", "failed", "tokens", "reason"},
}

// isWorkflowLifecycleTrace reports whether a "workflow" trace is the
// observer's lifecycle trace rather than the tool loop's report of the
// workflow tool call, which arrives under the same name with the model's raw
// tool input as its args.
//
// Testing only for run_id/workflow is not enough: those are keys the model
// controls, and the checkpoint result it just read shows run_id="wf_…", so a
// call like {"run_id":"wf_1","reply":…} is an easy slip. Let through, its
// "running" trace replaced the paused run on screen with an empty one and
// its decode error marked that run failed, while the real run sat paused.
// So the trace must name the run, carry none of the tool's inputs, and carry
// a key only the observer writes for its status.
func isWorkflowLifecycleTrace(argsJSON, status string) bool {
	var keys map[string]json.RawMessage
	if json.Unmarshal([]byte(argsJSON), &keys) != nil {
		return false
	}
	if _, ok := keys["run_id"]; !ok {
		return false
	}
	if _, ok := keys["workflow"]; !ok {
		return false
	}
	for _, k := range workflowToolInputKeys {
		if _, ok := keys[k]; ok {
			return false
		}
	}
	kind := status
	if kind != "running" && kind != "paused" {
		kind = "settled"
	}
	for _, k := range workflowLifecycleMarkers[kind] {
		if _, ok := keys[k]; ok {
			return true
		}
	}
	return false
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
	// call returned. (Progress traces need no such check — no tool is called
	// "workflow-progress" — and a workspace merge note legitimately carries
	// no run fields.)
	if name == "workflow" {
		if !isWorkflowLifecycleTrace(argsJSON, status) {
			return true
		}
		return m.applyWorkflowLifecycle(args, output, status)
	}

	w := m.workflowFor(args.RunID)
	if w == nil {
		return true
	}
	switch args.Kind {
	case "phase":
		title := strings.TrimSpace(args.Phase)
		if title == "" {
			return true
		}
		detail := strings.TrimSpace(args.Detail)
		for i, p := range w.Phases {
			if p.Title == title {
				// A declared phase entered with a detail its header lacked:
				// keep whichever says more.
				if p.Detail == "" {
					w.Phases[i].Detail = detail
				}
				return true
			}
		}
		w.Phases = append(w.Phases, workflowPhaseEntry{
			Title: title, Detail: detail, Dynamic: args.Dynamic,
		})
	case "log":
		if msg := strings.TrimSpace(output); msg != "" {
			w.Logs = append(w.Logs, workflowLogEntry{
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
		w.Logs = append(w.Logs, workflowLogEntry{
			Phase: args.Phase, Message: "⏸ " + msg, At: time.Now(),
		})
	}
	return true
}

// workflowFor returns the state of run runID: the run on screen, or one a
// newer run displaced while it was still paused or running (parked), so its
// progress keeps landing on its own tree. A trace that names no run belongs
// to the run on screen. Nil when the run is unknown: folding another run's
// agents and log into the tree on screen would misreport both.
func (m *Model) workflowFor(runID string) *workflowRun {
	w := m.workflow
	if runID == "" || (w != nil && (w.RunID == runID || w.RunID == "")) {
		return w
	}
	return m.parkedWorkflows[runID]
}

// showWorkflow puts run on screen. The run it replaces is parked when it is
// not over — a paused run the orchestrator may still continue, or one still
// running beside it — so continuing it later restores its tree instead of
// rebuilding an empty one from the start payload; a settled run is dropped.
func (m *Model) showWorkflow(run *workflowRun) {
	if prev := m.workflow; prev != nil && prev != run && prev.RunID != "" &&
		(prev.Status == "paused" || prev.Status == "running") {
		if m.parkedWorkflows == nil {
			m.parkedWorkflows = map[string]*workflowRun{}
		}
		m.parkedWorkflows[prev.RunID] = prev
	}
	if run != nil {
		delete(m.parkedWorkflows, run.RunID)
	}
	m.workflow = run
}

// dropWorkflows forgets the run on screen and every parked one.
func (m *Model) dropWorkflows() {
	m.workflow = nil
	m.parkedWorkflows = nil
}

// applyWorkflowLifecycle folds the observer's "workflow" traces: a run
// starting or being continued, pausing at a checkpoint, and settling.
func (m *Model) applyWorkflowLifecycle(args workflowTraceArgs, output, status string) bool {
	if status == "running" {
		// A continue keeps everything the panel already knows about the run:
		// its phases, agents and log are what the reader needs to follow what
		// the orchestrator decided at the checkpoint. That holds for a run a
		// newer one displaced from the screen too — the continue brings its
		// own tree back rather than an empty one.
		if args.Resumed {
			if w := m.workflowFor(args.RunID); w != nil && args.RunID != "" {
				w.Status = "running"
				w.CheckpointID = ""
				w.CheckpointMessage = ""
				w.PausedAt = time.Time{}
				m.showWorkflow(w)
				return true
			}
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
		m.showWorkflow(run)
		if !m.showSidePanel && !args.Resumed {
			m.showBanner("workflow "+run.Name+m.panelKeyHint(" — ", "the phase tree"), "info")
		}
		return true
	}
	// A pause or settle lands on its own run — on screen or parked — and
	// never rewrites a different run being shown.
	w := m.workflowFor(args.RunID)
	if w == nil {
		return true
	}
	if status == "paused" {
		w.Status = "paused"
		w.CheckpointID = args.CheckpointID
		w.CheckpointMessage = strings.TrimSpace(args.Message)
		w.PausedAt = time.Now()
		return true
	}
	switch status {
	case "error":
		w.settle("failed", strings.TrimSpace(output))
	case "stopped":
		reason := strings.TrimSpace(args.Reason)
		if reason == "" {
			reason = strings.TrimSpace(output)
		}
		w.settle("stopped", stoppedSummary(reason))
	default:
		w.settle("done", strings.TrimSpace(output))
	}
	// Nobody continues a settled run, so a parked one has nothing left to
	// come back for.
	if w != m.workflow {
		delete(m.parkedWorkflows, w.RunID)
	}
	return true
}

// settle closes the run with its final status and summary line.
func (w *workflowRun) settle(status, summary string) {
	w.Status = status
	w.CheckpointID = ""
	w.CheckpointMessage = ""
	w.Summary = summary
	w.FinishedAt = time.Now()
}

func stoppedSummary(reason string) string {
	if reason == "" {
		return "stopped"
	}
	return "stopped: " + reason
}

// workflowStoppedMsg reports that the session's run registry stopped a run
// nobody was attached to: one paused at a checkpoint, whose tool call had
// already returned, ended by the idle reaper or by StopAll. Such a stop
// emits no finish trace — there is no turn to carry it — so without this the
// panel would go on saying the run waits for an orchestrator.
type workflowStoppedMsg struct {
	runID, name, reason string
}

// workflowStopNotifier is the hook the session's run registry offers hosts:
// it calls fn — from its own goroutine, never under its lock — when it stops
// a detached run. Asserted rather than called directly so the panel keeps
// building against a registry without the hook; it then falls back to the
// turn-start check in clearSettledWorkflow.
type workflowStopNotifier interface {
	SetOnStopped(fn func(runID, name, reason string))
}

// workflowStopBuffer bounds the stops waiting for Update. The callback must
// never block the registry (the reaper and StopAll call it), so a stop that
// finds the buffer full is dropped; the next turn's clearSettledWorkflow
// still notices the run is gone.
const workflowStopBuffer = 16

// watchWorkflowRuns registers the panel's stop hook on the session's run
// registry and returns the command that delivers the first stop to Update;
// each workflowStoppedMsg re-arms it. Nil when there is nothing to watch.
func (m *Model) watchWorkflowRuns() tea.Cmd {
	n, ok := any(m.workflowRuns).(workflowStopNotifier)
	if m.workflowRuns == nil || !ok {
		return nil
	}
	return m.watchWorkflowStops(n)
}

func (m *Model) watchWorkflowStops(n workflowStopNotifier) tea.Cmd {
	ch := make(chan workflowStoppedMsg, workflowStopBuffer)
	m.workflowStops = ch
	n.SetOnStopped(func(runID, name, reason string) {
		select {
		case ch <- workflowStoppedMsg{runID: runID, name: name, reason: reason}:
		default:
		}
	})
	return waitForWorkflowStop(ch)
}

func waitForWorkflowStop(ch <-chan workflowStoppedMsg) tea.Cmd {
	if ch == nil {
		return nil
	}
	return func() tea.Msg {
		msg, ok := <-ch
		if !ok {
			return nil
		}
		return msg
	}
}

// applyWorkflowStopped marks a detached run stopped: the tree on screen says
// so and keeps what the run did; a parked one is simply forgotten.
func (m *Model) applyWorkflowStopped(msg workflowStoppedMsg) {
	if msg.runID == "" {
		return
	}
	if w := m.workflow; w != nil && w.RunID == msg.runID {
		if w.Status == "paused" || w.Status == "running" {
			w.settle("stopped", stoppedSummary(strings.TrimSpace(msg.reason)))
		}
		return
	}
	delete(m.parkedWorkflows, msg.runID)
}

// clearSettledWorkflow drops the workflow tree when a new turn starts, unless
// the run is paused at a checkpoint and still alive: the next turn is then
// most likely the orchestrator answering it, and the tree is what the reader
// needs to follow that. A paused run the registry no longer holds (stopped by
// the idle reaper, or by /clear) is over, and so is its tree. Parked runs
// follow the same rule, so one continued later still has its tree.
func (m *Model) clearSettledWorkflow() {
	for id, w := range m.parkedWorkflows {
		if w.Status != "paused" || !m.workflowRunLive(id) {
			delete(m.parkedWorkflows, id)
		}
	}
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
	w := m.workflowFor(args.RunID)
	if w == nil {
		// With a run on screen, a member of some run this panel does not
		// know is still a workflow member, not a loose parallel agent.
		return m.workflow != nil
	}
	entryStatus := "done"
	switch status {
	case "running":
		entryStatus = "running"
	case "error":
		entryStatus = "failed"
	}
	for i := range w.Agents {
		if w.Agents[i].Instance == args.Agent {
			w.Agents[i].Status = entryStatus
			if entryStatus == "failed" {
				w.Agents[i].Detail = truncateLabel(strings.TrimSpace(output), 160)
			}
			return true
		}
	}
	w.Agents = append(w.Agents, workflowAgentEntry{
		Instance: args.Agent,
		Label:    args.Task,
		Phase:    args.Phase,
		Index:    args.Index,
		Status:   entryStatus,
		Cached:   args.Cached,
	})
	return true
}
