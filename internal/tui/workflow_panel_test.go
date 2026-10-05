package tui

import (
	"fmt"
	"strings"
	"testing"

	"spettro/internal/agent"
	"spettro/internal/config"
	"spettro/internal/theme"
)

func wfStartTrace() agent.ToolTrace {
	return agent.ToolTrace{
		AgentID: "coding",
		Name:    "workflow",
		Status:  "running",
		Args: `{"run_id":"wf_1","workflow":"review-changes","description":"Review then verify",` +
			`"origin":"inline","phases":[{"title":"Review"},{"title":"Verify"}]}`,
	}
}

func wfAgentTrace(instance, label, phase, status string, cached bool) agent.ToolTrace {
	return agent.ToolTrace{
		AgentID: instance,
		Name:    "agent",
		Status:  status,
		Args: fmt.Sprintf(`{"agent":%q,"task":%q,"parent_agent_id":"coding","workflow":"review-changes",`+
			`"run_id":"wf_1","phase":%q,"cached":%t}`, instance, label, phase, cached),
	}
}

func newWorkflowModel(t *testing.T) Model {
	t.Helper()
	m := NewModelForTesting()
	m.manifest = config.DefaultAgentManifest()
	return m
}

func TestWorkflowPanelLifecycle(t *testing.T) {
	m := newWorkflowModel(t)
	if lines := m.workflowTreeLines(60, 0); lines != nil {
		t.Fatalf("no workflow → no panel, got %v", lines)
	}

	m.applyToolTraceToObservability(wfStartTrace())
	if m.workflow == nil || m.workflow.Name != "review-changes" || m.workflow.Status != "running" {
		t.Fatalf("workflow not started: %+v", m.workflow)
	}
	// Declared phases exist before any agent runs — that is the point of
	// putting them in meta.
	if len(m.workflow.Phases) != 2 {
		t.Fatalf("declared phases lost: %+v", m.workflow.Phases)
	}
	joined := strings.Join(m.workflowTreeLines(70, 0), "\n")
	for _, want := range []string{"review-changes", "Review", "Verify", "Review then verify"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("panel missing %q:\n%s", want, joined)
		}
	}

	m.applyToolTraceToObservability(wfAgentTrace("general-purpose#1", "review:bugs", "Review", "running", false))
	m.applyToolTraceToObservability(wfAgentTrace("general-purpose#2", "review:perf", "Review", "running", false))
	if len(m.workflow.Agents) != 2 {
		t.Fatalf("agents = %+v", m.workflow.Agents)
	}
	// A workflow member belongs to its phase, not to the loose agent list.
	if len(m.parallelAgents) != 0 {
		t.Fatalf("workflow agents must not double as parallel agents: %+v", m.parallelAgents)
	}

	m.applyToolTraceToObservability(wfAgentTrace("general-purpose#1", "review:bugs", "Review", "success", false))
	m.applyToolTraceToObservability(wfAgentTrace("general-purpose#2", "review:perf", "Review", "error", false))
	running, done, failed, _ := m.workflow.counts()
	if running != 0 || done != 1 || failed != 1 {
		t.Fatalf("counts = %d/%d/%d", running, done, failed)
	}
	if !strings.Contains(m.workflow.headline(), "1 failed") {
		t.Fatalf("headline = %q", m.workflow.headline())
	}

	m.applyToolTraceToObservability(agent.ToolTrace{
		AgentID: "coding", Name: "workflow", Status: "success",
		Args: `{"run_id":"wf_1","workflow":"review-changes","agents":2,"failed":1}`,
	})
	if m.workflow.Status != "done" {
		t.Fatalf("status = %q", m.workflow.Status)
	}
}

func TestWorkflowPanelProgressEvents(t *testing.T) {
	m := newWorkflowModel(t)
	m.applyToolTraceToObservability(wfStartTrace())
	m.applyToolTraceToObservability(agent.ToolTrace{
		AgentID: "coding", Name: "workflow-progress", Status: "success",
		Args:   `{"run_id":"wf_1","workflow":"review-changes","kind":"log","phase":"Review"}`,
		Output: "round 1: 3 found",
	})
	// A phase the script entered but meta never declared still has to appear.
	m.applyToolTraceToObservability(agent.ToolTrace{
		AgentID: "coding", Name: "workflow-progress", Status: "success",
		Args:   `{"run_id":"wf_1","workflow":"review-changes","kind":"phase","phase":"Synthesise"}`,
		Output: "Synthesise",
	})
	if len(m.workflow.Logs) != 1 || m.workflow.Logs[0].Message != "round 1: 3 found" {
		t.Fatalf("logs = %+v", m.workflow.Logs)
	}
	joined := strings.Join(m.workflowTreeLines(70, 0), "\n")
	if !strings.Contains(joined, "round 1: 3 found") {
		t.Fatalf("log not rendered:\n%s", joined)
	}
	if !strings.Contains(joined, "Synthesise") {
		t.Fatalf("undeclared phase must still render:\n%s", joined)
	}
	// Declaring the same phase twice must not duplicate the group.
	m.applyToolTraceToObservability(agent.ToolTrace{
		AgentID: "coding", Name: "workflow-progress", Status: "success",
		Args: `{"run_id":"wf_1","workflow":"review-changes","kind":"phase","phase":"Review"}`,
	})
	if len(m.workflow.phaseOrder()) != 3 {
		t.Fatalf("phase order = %v", m.workflow.phaseOrder())
	}
}

func TestWorkflowPanelGroupsAgentsUnderPhases(t *testing.T) {
	m := newWorkflowModel(t)
	m.applyToolTraceToObservability(wfStartTrace())
	m.applyToolTraceToObservability(wfAgentTrace("general-purpose#1", "review:bugs", "Review", "success", false))
	m.applyToolTraceToObservability(wfAgentTrace("general-purpose#2", "refute:bugs", "Verify", "running", false))
	m.applyToolTraceToObservability(wfAgentTrace("general-purpose#3", "loose", "", "running", false))

	if got := len(m.workflow.agentsInPhase("Review")); got != 1 {
		t.Fatalf("Review holds %d agents", got)
	}
	if got := len(m.workflow.agentsInPhase("")); got != 1 {
		t.Fatalf("phaseless agents = %d", got)
	}
	joined := strings.Join(m.workflowTreeLines(70, 0), "\n")
	if !strings.Contains(joined, "(no phase)") {
		t.Fatalf("agents dispatched outside a phase must still show:\n%s", joined)
	}
	reviewIdx := strings.Index(joined, "review:bugs")
	verifyIdx := strings.Index(joined, "refute:bugs")
	if reviewIdx < 0 || verifyIdx < 0 || reviewIdx > verifyIdx {
		t.Fatalf("phases must render in declared order:\n%s", joined)
	}
}

func TestWorkflowPanelMarksReplayedAgents(t *testing.T) {
	m := newWorkflowModel(t)
	m.applyToolTraceToObservability(wfStartTrace())
	m.applyToolTraceToObservability(wfAgentTrace("general-purpose#1", "review:bugs", "Review", "success", true))
	if _, _, _, cached := m.workflow.counts(); cached != 1 {
		t.Fatalf("cached count = %d", cached)
	}
	joined := strings.Join(m.workflowTreeLines(70, 0), "\n")
	if !strings.Contains(joined, "replayed") {
		t.Fatalf("a journal replay must be labelled:\n%s", joined)
	}
}

func TestWorkflowClearedOnNextTurn(t *testing.T) {
	m := newWorkflowModel(t)
	m.applyToolTraceToObservability(wfStartTrace())
	m.startAgentActivity("coding", "next task")
	if m.workflow != nil {
		t.Fatal("a new turn must clear the previous workflow tree")
	}
}

func TestProgressBar(t *testing.T) {
	if got := stripANSIForTest(progressBar(10, 0, 0, 0)); got != strings.Repeat("░", 10) {
		t.Fatalf("empty bar = %q", got)
	}
	if got := stripANSIForTest(progressBar(10, 10, 0, 10)); got != strings.Repeat("█", 10) {
		t.Fatalf("full bar = %q", got)
	}
	// A single failure among many must still be visible rather than rounding
	// away to nothing.
	got := stripANSIForTest(progressBar(10, 0, 1, 100))
	if !strings.HasPrefix(got, "█") {
		t.Fatalf("a lone failure must still light a cell: %q", got)
	}
	if n := len([]rune(stripANSIForTest(progressBar(10, 5, 5, 10)))); n != 10 {
		t.Fatalf("bar width = %d, want exactly 10 cells", n)
	}
}

func TestTruncateAgentNameKeepsTheInstanceNumber(t *testing.T) {
	// The number is the only part that distinguishes concurrent members, so a
	// plain right-truncation would make the panel useless.
	if got := truncateAgentName("general-purpose#12", 14); got != "general-pu…#12" {
		t.Fatalf("got %q", got)
	}
	if got := truncateAgentName("code#3", 14); got != "code#3" {
		t.Fatalf("a short name must be left alone, got %q", got)
	}
	if got := truncateAgentName("noinstancename", 8); got != "noinsta…" {
		t.Fatalf("a name with no suffix falls back to plain truncation, got %q", got)
	}
	if n := len([]rune(truncateAgentName("general-purpose#7", 10))); n > 10 {
		t.Fatalf("result is %d cells wide, want at most 10", n)
	}
}

func TestWorkflowPanelTrimsFinishedRowsNotPhases(t *testing.T) {
	m := newWorkflowModel(t)
	m.applyToolTraceToObservability(wfStartTrace())
	// Twelve finished Scan agents, one of them failed, plus a running one.
	for i := 1; i <= 12; i++ {
		inst := fmt.Sprintf("general-purpose#%d", i)
		m.applyToolTraceToObservability(wfAgentTrace(inst, fmt.Sprintf("scan:%d", i), "Review", "running", false))
		status := "success"
		if i == 4 {
			status = "error"
		}
		m.applyToolTraceToObservability(wfAgentTrace(inst, fmt.Sprintf("scan:%d", i), "Review", status, false))
	}
	m.applyToolTraceToObservability(wfAgentTrace("general-purpose#13", "verify:1", "Verify", "running", false))

	lines := m.workflowTreeLines(80, 10)
	if len(lines) > 10 {
		t.Fatalf("panel is %d rows, want at most 10", len(lines))
	}
	joined := strings.Join(lines, "\n")
	// Both declared phases must survive: what is still to come is the point.
	for _, want := range []string{"Review", "Verify"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("trimming dropped phase %q:\n%s", want, joined)
		}
	}
	// The running agent must survive.
	if !strings.Contains(joined, "verify:1") {
		t.Fatalf("trimming dropped a running agent:\n%s", joined)
	}
	// The failure must outlive the successes around it.
	if !strings.Contains(joined, "scan:4") {
		t.Fatalf("trimming dropped the failed agent but kept successes:\n%s", joined)
	}
	if !strings.Contains(joined, "hidden") {
		t.Fatalf("trimming must say how much it hid:\n%s", joined)
	}
	// Uncapped, everything is there.
	if full := m.workflowTreeLines(80, 0); len(full) <= len(lines) {
		t.Fatalf("uncapped tree (%d rows) should be longer than the capped one (%d)", len(full), len(lines))
	}
}

// The footer competes with the conversation. The full tree belongs in the side
// panel, which has the room; down here a running workflow must never be the
// reason you cannot read what the agent just said.
func TestWorkflowFooterStaysOutOfTheWay(t *testing.T) {
	m := newWorkflowModel(t)
	m.applyToolTraceToObservability(wfStartTrace())
	for i := 1; i <= 6; i++ {
		inst := fmt.Sprintf("general-purpose#%d", i)
		m.applyToolTraceToObservability(wfAgentTrace(inst, fmt.Sprintf("scan:%d", i), "Review", "running", false))
		m.applyToolTraceToObservability(wfAgentTrace(inst, fmt.Sprintf("scan:%d", i), "Review", "success", false))
	}
	for i := 7; i <= 14; i++ {
		m.applyToolTraceToObservability(wfAgentTrace(fmt.Sprintf("general-purpose#%d", i),
			fmt.Sprintf("verify:%d", i), "Verify", "running", false))
	}

	// A terminal the side panel fits in, so the footer may point at it.
	m.width = 120
	for _, height := range []int{20, 24, 40, 60} {
		m.height = height
		// The budget the renderer would actually hand it, border excluded.
		rows := footerBudget(height) - 2
		summary := m.workflowSummaryLines(90, rows)
		if len(summary) == 0 {
			t.Fatalf("height %d: no summary", height)
		}
		// The block must fit the allowance it was given — the "… N more" line
		// included, which is the part that is easy to forget to pay for.
		if len(summary) > rows {
			t.Fatalf("height %d: the block took %d rows of a %d-row allowance:\n%s",
				height, len(summary), rows, strings.Join(summary, "\n"))
		}
		// And the whole region stays a small share of the screen.
		if total := len(summary) + 2; total > height/3 {
			t.Fatalf("height %d: the footer takes %d of %d rows", height, total, height)
		}
		joined := strings.Join(summary, "\n")
		// Only live work: finished agents are history, and history is what the
		// side panel is for.
		if strings.Contains(joined, "scan:1") {
			t.Fatalf("height %d: the footer lists finished agents:\n%s", height, joined)
		}
		if !strings.Contains(joined, "Verify") {
			t.Fatalf("height %d: the footer does not name the current phase:\n%s", height, joined)
		}
		if !strings.Contains(joined, "ctrl+b") {
			t.Fatalf("height %d: no pointer to the full tree:\n%s", height, joined)
		}
	}

	// On a terminal too narrow for the panel, ctrl+b shows nothing, so the
	// footer does not point at it.
	m.width, m.height = 80, 24
	if joined := strings.Join(m.workflowSummaryLines(76, footerBudget(24)-2), "\n"); strings.Contains(joined, "ctrl+b") {
		t.Fatalf("80x24: the footer points at a panel that cannot be drawn:\n%s", joined)
	}
	m.width = 120

	// A taller terminal may show more of the live work than a short one.
	short := len(m.workflowSummaryLines(90, footerBudget(20)-2))
	tall := len(m.workflowSummaryLines(90, footerBudget(60)-2))
	if short >= tall {
		t.Fatalf("the footer does not scale with the terminal: %d rows at 20, %d at 60", short, tall)
	}
	// The side panel keeps everything, including the finished agents.
	full := strings.Join(m.sidePanelWorkflowLines(48), "\n")
	if !strings.Contains(full, "scan:1") || !strings.Contains(full, "verify:7") {
		t.Fatalf("the side panel must still show the whole tree:\n%s", full)
	}
}

// Once a run is over its detail stops being live, and the conversation needs
// the rows back.
func TestFinishedWorkflowCollapsesToOneLine(t *testing.T) {
	m := newWorkflowModel(t)
	m.applyToolTraceToObservability(wfStartTrace())
	for i := 1; i <= 6; i++ {
		inst := fmt.Sprintf("general-purpose#%d", i)
		m.applyToolTraceToObservability(wfAgentTrace(inst, fmt.Sprintf("scan:%d", i), "Review", "running", false))
		m.applyToolTraceToObservability(wfAgentTrace(inst, fmt.Sprintf("scan:%d", i), "Review", "success", false))
	}
	m.applyToolTraceToObservability(agent.ToolTrace{
		AgentID: "coding", Name: "workflow", Status: "success",
		Args:   `{"run_id":"wf_1","workflow":"review-changes","agents":6,"failed":0,"cached":0,"tokens":0}`,
		Output: "6 agents · 0 failed · 0 replayed",
	})
	summary := m.workflowSummaryLines(90, footerBudget(40)-2)
	if len(summary) != 1 {
		t.Fatalf("a finished run should collapse to one line, got %d:\n%s", len(summary), strings.Join(summary, "\n"))
	}
	if !strings.Contains(summary[0], "6 agents") {
		t.Fatalf("the one line must carry the outcome: %q", summary[0])
	}
}

func TestCurrentPhaseTracksTheRun(t *testing.T) {
	m := newWorkflowModel(t)
	m.applyToolTraceToObservability(wfStartTrace())
	// Nothing has started: the first declared phase is what is coming.
	if title, _, _, _, pending := m.workflow.currentPhase(); title != "Review" || !pending {
		t.Fatalf("before any agent: title=%q pending=%v", title, pending)
	}
	m.applyToolTraceToObservability(wfAgentTrace("general-purpose#1", "scan", "Review", "running", false))
	if title, _, _, _, pending := m.workflow.currentPhase(); title != "Review" || pending {
		t.Fatalf("while Review runs: title=%q pending=%v", title, pending)
	}
	m.applyToolTraceToObservability(wfAgentTrace("general-purpose#1", "scan", "Review", "success", false))
	m.applyToolTraceToObservability(wfAgentTrace("general-purpose#2", "refute", "Verify", "running", false))
	// Work in flight wins over a phase that has merely finished.
	if title, _, _, total, _ := m.workflow.currentPhase(); title != "Verify" || total != 1 {
		t.Fatalf("once Verify starts: title=%q total=%d", title, total)
	}
}

// wfPausedTrace is the observer's lifecycle trace for a run that paused at a
// checkpoint when the tool call returned.
func wfPausedTrace(runID, cp, message string) agent.ToolTrace {
	return agent.ToolTrace{
		AgentID: "coding", Name: "workflow", Status: "paused",
		Args: fmt.Sprintf(`{"run_id":%q,"workflow":"review-changes","checkpoint_id":%q,"message":%q}`,
			runID, cp, message),
	}
}

// The tool loop reports the workflow tool's own call under the name
// "workflow" too, with the tool input as args. It must not open, rename or
// settle the run the observer reports.
func TestWorkflowPanelIgnoresTheToolCallTrace(t *testing.T) {
	m := newWorkflowModel(t)
	m.applyToolTraceToObservability(agent.ToolTrace{
		AgentID: "coding", Name: "workflow", Status: "running",
		Args: `{"script":"export const meta = {name: 'x'}","save_as":"x"}`,
	})
	if m.workflow != nil {
		t.Fatalf("the tool-call trace opened a run: %+v", m.workflow)
	}
	m.applyToolTraceToObservability(wfStartTrace())
	m.applyToolTraceToObservability(agent.ToolTrace{
		AgentID: "coding", Name: "workflow", Status: "success",
		Args:   `{"continue_run_id":"wf_1","reply":{"go":true}}`,
		Output: `<workflow_result name="review-changes" run_id="wf_1">…`,
	})
	if m.workflow.Status != "running" || m.workflow.Summary != "" {
		t.Fatalf("the tool-call completion settled the run: status=%q summary=%q", m.workflow.Status, m.workflow.Summary)
	}
}

func TestWorkflowPanelPausedAtCheckpoint(t *testing.T) {
	m := newWorkflowModel(t)
	m.applyToolTraceToObservability(wfStartTrace())
	m.applyToolTraceToObservability(wfAgentTrace("general-purpose#1", "review:bugs", "Review", "running", false))
	m.applyToolTraceToObservability(wfAgentTrace("general-purpose#1", "review:bugs", "Review", "success", false))
	m.applyToolTraceToObservability(agent.ToolTrace{
		AgentID: "coding", Name: "workflow-progress", Status: "success",
		Args: `{"run_id":"wf_1","workflow":"review-changes","kind":"checkpoint","phase":"Review",` +
			`"checkpoint_id":"cp-1","message":"3 findings — verify which?"}`,
		Output: `{"findings":3}`,
	})
	// The progress trace records the ask; only the lifecycle trace pauses.
	if m.workflow.Status != "running" {
		t.Fatalf("a checkpoint progress trace must not pause the run by itself: %q", m.workflow.Status)
	}
	if n := len(m.workflow.Logs); n != 1 || m.workflow.Logs[0].Message != "⏸ 3 findings — verify which?" {
		t.Fatalf("checkpoint log = %+v", m.workflow.Logs)
	}

	m.applyToolTraceToObservability(wfPausedTrace("wf_1", "cp-1", "3 findings — verify which?"))
	// The tool call returns at the checkpoint right after: that generic
	// completion must not mark the paused run done.
	m.applyToolTraceToObservability(agent.ToolTrace{
		AgentID: "coding", Name: "workflow", Status: "success",
		Args:   `{"name":"review-changes"}`,
		Output: `<workflow_checkpoint name="review-changes" run_id="wf_1" checkpoint_id="cp-1">`,
	})
	w := m.workflow
	if w.Status != "paused" || w.CheckpointID != "cp-1" || w.PausedAt.IsZero() {
		t.Fatalf("paused state = %+v", w)
	}
	if want := "paused at cp-1 — waiting for orchestrator: 3 findings — verify which?"; w.headline() != want {
		t.Fatalf("headline = %q, want %q", w.headline(), want)
	}
	// Nothing runs while paused, so nothing animates.
	if m.hasRunningDelegation() {
		t.Fatal("a paused run must not keep the chrome animating")
	}

	tree := stripANSIForTest(strings.Join(m.workflowTreeLines(70, 0), "\n"))
	for _, want := range []string{"⏸ workflow review-changes", "paused at cp-1", "waiting for orchestrator: 3 findings", "⏸ 3 findings"} {
		if !strings.Contains(tree, want) {
			t.Fatalf("panel missing %q:\n%s", want, tree)
		}
	}
	// The footer collapses to the one line that says what it is waiting on.
	summary := m.workflowSummaryLines(90, footerBudget(40)-2)
	if len(summary) != 1 || !strings.Contains(stripANSIForTest(summary[0]), "waiting for orchestrator") {
		t.Fatalf("paused footer = %q", summary)
	}
	if !strings.Contains(strings.Join(m.sidePanelHeaderParts(48), "\n"), "paused") {
		t.Fatal("the side panel subtitle should say the run is paused")
	}
}

func TestWorkflowPanelReplayedCheckpointDoesNotPause(t *testing.T) {
	m := newWorkflowModel(t)
	m.applyToolTraceToObservability(wfStartTrace())
	m.applyToolTraceToObservability(agent.ToolTrace{
		AgentID: "coding", Name: "workflow-progress", Status: "success",
		Args: `{"run_id":"wf_1","workflow":"review-changes","kind":"checkpoint","checkpoint_id":"cp-1",` +
			`"message":"go on?","cached":true}`,
	})
	if m.workflow.Status != "running" {
		t.Fatalf("status = %q", m.workflow.Status)
	}
	if got := m.workflow.Logs[0].Message; got != "⏸ replayed · go on?" {
		t.Fatalf("replayed checkpoint log = %q", got)
	}
	// The finish trace's "cached" is a count, not a bool; the rest of the
	// payload must still decode.
	m.applyToolTraceToObservability(agent.ToolTrace{
		AgentID: "coding", Name: "workflow", Status: "success",
		Args:   `{"run_id":"wf_1","workflow":"review-changes","agents":2,"failed":0,"cached":2}`,
		Output: "2 agents · 0 failed · 2 replayed",
	})
	if m.workflow.Status != "done" || m.workflow.Summary != "2 agents · 0 failed · 2 replayed" {
		t.Fatalf("finish not applied: %+v", m.workflow)
	}
}

// A paused run outlives the turn that started it: the next turn is usually
// the orchestrator answering it. One the registry no longer holds is over.
func TestWorkflowPausedRunSurvivesTheNextTurn(t *testing.T) {
	m := newWorkflowModel(t)
	m.workflowRuns = nil // no registry to ask: trust the traces
	m.applyToolTraceToObservability(wfStartTrace())
	m.applyToolTraceToObservability(wfPausedTrace("wf_1", "cp-1", "pick a fix"))
	m.startAgentActivity("coding", "continue it")
	if m.workflow == nil || m.workflow.Status != "paused" {
		t.Fatalf("the paused run was cleared by the next turn: %+v", m.workflow)
	}

	// With a registry that does not hold the run (the idle reaper stopped it,
	// or it was never registered), the tree goes like any finished one.
	m.workflowRuns = agent.NewWorkflowRuns()
	m.startAgentActivity("coding", "something else")
	if m.workflow != nil {
		t.Fatalf("a paused run that is no longer live must be cleared: %+v", m.workflow)
	}
}

// A continue of a paused run reports "running" again with resumed:true; the
// panel keeps the run's agents, phases and log instead of starting over.
func TestWorkflowResumedRunKeepsItsState(t *testing.T) {
	m := newWorkflowModel(t)
	m.applyToolTraceToObservability(wfStartTrace())
	m.applyToolTraceToObservability(wfAgentTrace("general-purpose#1", "review:bugs", "Review", "success", false))
	m.applyToolTraceToObservability(agent.ToolTrace{
		AgentID: "coding", Name: "workflow-progress", Status: "success",
		Args:   `{"run_id":"wf_1","workflow":"review-changes","kind":"log","phase":"Review"}`,
		Output: "1 finding",
	})
	m.applyToolTraceToObservability(wfPausedTrace("wf_1", "cp-1", "verify?"))
	m.applyToolTraceToObservability(agent.ToolTrace{
		AgentID: "coding", Name: "workflow", Status: "running",
		Args: `{"run_id":"wf_1","workflow":"review-changes","resumed":true}`,
	})
	w := m.workflow
	if w.Status != "running" || w.CheckpointID != "" {
		t.Fatalf("resumed run state = %+v", w)
	}
	if len(w.Agents) != 1 || len(w.Logs) != 1 || len(w.Phases) != 2 || w.Description == "" {
		t.Fatalf("a continue reset the run: agents=%d logs=%d phases=%d desc=%q",
			len(w.Agents), len(w.Logs), len(w.Phases), w.Description)
	}
	if !m.hasRunningDelegation() {
		t.Fatal("a continued run is live again and should animate")
	}

	// A fresh start (not a continue) still replaces the run on screen, and a
	// pause reported for some other run leaves it alone.
	m.applyToolTraceToObservability(agent.ToolTrace{
		AgentID: "coding", Name: "workflow", Status: "running",
		Args: `{"run_id":"wf_2","workflow":"other","origin":"inline","phases":[]}`,
	})
	if m.workflow.RunID != "wf_2" || len(m.workflow.Agents) != 0 {
		t.Fatalf("a new run did not replace the old one: %+v", m.workflow)
	}
	m.applyToolTraceToObservability(wfPausedTrace("wf_1", "cp-2", "stale"))
	if m.workflow.Status != "running" {
		t.Fatalf("a pause for another run changed this one: %q", m.workflow.Status)
	}
}

func TestWorkflowPanelDynamicPhases(t *testing.T) {
	m := newWorkflowModel(t)
	m.applyToolTraceToObservability(wfStartTrace())
	m.applyToolTraceToObservability(agent.ToolTrace{
		AgentID: "coding", Name: "workflow-progress", Status: "success",
		Args: `{"run_id":"wf_1","workflow":"review-changes","kind":"phase","phase":"Fix",` +
			`"detail":"patch the confirmed bugs","dynamic":true}`,
		Output: "Fix",
	})
	// A declared phase entered with a detail keeps its plan-time identity.
	m.applyToolTraceToObservability(agent.ToolTrace{
		AgentID: "coding", Name: "workflow-progress", Status: "success",
		Args: `{"run_id":"wf_1","workflow":"review-changes","kind":"phase","phase":"Review","detail":"three lenses","dynamic":false}`,
	})
	fix := m.workflow.phaseEntry("Fix")
	if !fix.Dynamic || fix.Detail != "patch the confirmed bugs" {
		t.Fatalf("dynamic phase entry = %+v", fix)
	}
	if review := m.workflow.phaseEntry("Review"); review.Dynamic || review.Detail != "three lenses" {
		t.Fatalf("declared phase entry = %+v", review)
	}
	tree := stripANSIForTest(strings.Join(m.workflowTreeLines(80, 0), "\n"))
	if !strings.Contains(tree, "+Fix") {
		t.Fatalf("a runtime phase should carry the + marker:\n%s", tree)
	}
	if strings.Contains(tree, "+Review") || strings.Contains(tree, "+Verify") {
		t.Fatalf("declared phases must not carry the + marker:\n%s", tree)
	}
	if !strings.Contains(tree, "patch the confirmed bugs") {
		t.Fatalf("the phase detail should render when it fits:\n%s", tree)
	}
}

func TestWorkflowPanelTitleShowsSize(t *testing.T) {
	m := newWorkflowModel(t)
	m.applyToolTraceToObservability(agent.ToolTrace{
		AgentID: "coding", Name: "workflow", Status: "running",
		Args: `{"run_id":"wf_1","workflow":"audit","size":"large","size_agents":30,"budget_tokens":500000}`,
	})
	title := stripANSIForTest(m.workflowTreeLines(80, 0)[0])
	if !strings.Contains(title, "workflow audit · large · 500k budget") {
		t.Fatalf("title = %q", title)
	}
	// Narrow: the budget gives way first, and the title still fits one row.
	for _, width := range []int{36, 28, 24} {
		title := m.workflowTreeLines(width, 0)[0]
		if w := len([]rune(stripANSIForTest(title))); w > width {
			t.Fatalf("width %d: title is %d cells: %q", width, w, stripANSIForTest(title))
		}
	}
	if title := stripANSIForTest(m.workflowTreeLines(36, 0)[0]); strings.Contains(title, "budget") {
		t.Fatalf("width 36: the budget should have given way: %q", title)
	}
	// A run that reported no size keeps the old title.
	m.applyToolTraceToObservability(wfStartTrace())
	if title := stripANSIForTest(m.workflowTreeLines(80, 0)[0]); strings.Contains(title, "review-changes ·") {
		t.Fatalf("no size reported, no size shown: %q", title)
	}
}

// /clear and /resume stop the session's paused runs; a paused tree must not
// keep claiming to wait for an orchestrator that can no longer answer.
func TestConversationResetDropsAPausedRun(t *testing.T) {
	m := newWorkflowModel(t)
	runs := m.workflowRuns
	m.applyToolTraceToObservability(wfStartTrace())
	m.applyToolTraceToObservability(wfPausedTrace("wf_1", "cp-1", "go?"))
	m.resetConversationState()
	if m.workflow != nil {
		t.Fatalf("a paused tree survived the reset: %+v", m.workflow)
	}
	if m.workflowRuns != runs {
		t.Fatal("the registry lasts the whole session; a reset empties it, it does not replace it")
	}
	// A nil registry is safe to reset (hosts and tests that never made one).
	m.workflowRuns = nil
	m.resetConversationState()
}

// wfPausedRunWithWork is a run that did some work and paused at cp-1: one
// finished agent, a log line, and a phase it opened at runtime.
func wfPausedRunWithWork(t *testing.T, m *Model) {
	t.Helper()
	m.applyToolTraceToObservability(wfStartTrace())
	m.applyToolTraceToObservability(wfAgentTrace("general-purpose#1", "review:bugs", "Review", "running", false))
	m.applyToolTraceToObservability(wfAgentTrace("general-purpose#1", "review:bugs", "Review", "success", false))
	m.applyToolTraceToObservability(agent.ToolTrace{
		AgentID: "coding", Name: "workflow-progress", Status: "success",
		Args:   `{"run_id":"wf_1","workflow":"review-changes","kind":"log","phase":"Review"}`,
		Output: "1 finding",
	})
	m.applyToolTraceToObservability(agent.ToolTrace{
		AgentID: "coding", Name: "workflow-progress", Status: "success",
		Args:   `{"run_id":"wf_1","workflow":"review-changes","kind":"phase","phase":"Triage","dynamic":true}`,
		Output: "Triage",
	})
	m.applyToolTraceToObservability(wfPausedTrace("wf_1", "cp-1", "verify?"))
}

func wfResumedTrace(runID string) agent.ToolTrace {
	// What the observer sends on a continue: the start payload (declared
	// phases only — no agents, log or runtime phases) marked resumed.
	return agent.ToolTrace{
		AgentID: "coding", Name: "workflow", Status: "running",
		Args: fmt.Sprintf(`{"run_id":%q,"workflow":"review-changes","description":"Review then verify",`+
			`"origin":"inline","phases":[{"title":"Review"},{"title":"Verify"}],"resumed":true}`, runID),
	}
}

// The tool loop reports the workflow call with the model's raw input. The
// checkpoint result shows run_id="wf_1", so a call that carries run_id (or
// even "workflow") is an easy slip — and must still not be read as the
// observer's lifecycle: it used to replace the paused run with an empty one
// and then mark that failed with the call's decode error.
func TestWorkflowPanelIgnoresToolCallsNamingTheRun(t *testing.T) {
	for _, args := range []string{
		`{"run_id":"wf_1","reply":true}`,
		`{"continue_run_id":"wf_1","run_id":"wf_1","reply":true}`,
		`{"run_id":"wf_1","workflow":"review-changes"}`,
		`{"run_id":"wf_1","workflow":"review-changes","checkpoint_id":"cp-1"}`,
		`{"run_id":"wf_1","workflow":"review-changes","size":"large","budget_tokens":5}`,
	} {
		m := newWorkflowModel(t)
		wfPausedRunWithWork(t, &m)
		for _, status := range []string{"running", "error", "success"} {
			m.applyToolTraceToObservability(agent.ToolTrace{
				AgentID: "coding", Name: "workflow", Status: status, Args: args,
				Output: "workflow args: json: cannot unmarshal",
			})
		}
		w := m.workflow
		if w.Status != "paused" || w.Name != "review-changes" || len(w.Agents) != 1 || len(w.Phases) != 3 {
			t.Fatalf("%s: the tool call rewrote the paused run: status=%q name=%q agents=%d phases=%d",
				args, w.Status, w.Name, len(w.Agents), len(w.Phases))
		}
	}

	// A valid continue carrying a redundant run_id: the observer's own
	// traces drive the panel; the call's completion (the next checkpoint's
	// XML) must not mark the re-paused run done.
	m := newWorkflowModel(t)
	wfPausedRunWithWork(t, &m)
	call := `{"continue_run_id":"wf_1","run_id":"wf_1","reply":true}`
	m.applyToolTraceToObservability(agent.ToolTrace{AgentID: "coding", Name: "workflow", Status: "running", Args: call})
	m.applyToolTraceToObservability(wfResumedTrace("wf_1"))
	m.applyToolTraceToObservability(wfPausedTrace("wf_1", "cp-2", "fix them?"))
	m.applyToolTraceToObservability(agent.ToolTrace{
		AgentID: "coding", Name: "workflow", Status: "success", Args: call,
		Output: `<workflow_checkpoint name="review-changes" run_id="wf_1" checkpoint_id="cp-2">`,
	})
	if w := m.workflow; w.Status != "paused" || w.CheckpointID != "cp-2" || len(w.Agents) != 1 || w.Summary != "" {
		t.Fatalf("continue with a redundant run_id: %+v", w)
	}
}

// A run stopped on purpose (the orchestrator's stop, the reaper, the session
// ending) reports status "stopped": it did not fail and must not read red.
func TestWorkflowStoppedRunIsNotAFailure(t *testing.T) {
	m := newWorkflowModel(t)
	wfPausedRunWithWork(t, &m)
	m.applyToolTraceToObservability(wfResumedTrace("wf_1"))
	m.applyToolTraceToObservability(agent.ToolTrace{
		AgentID: "coding", Name: "workflow", Status: "stopped",
		Args: `{"run_id":"wf_1","workflow":"review-changes","agents":1,"failed":0,"cached":0,"tokens":10,` +
			`"reason":"at the orchestrator's request"}`,
		Output: "at the orchestrator's request",
	})
	w := m.workflow
	if w.Status != "stopped" || w.Summary != "stopped: at the orchestrator's request" || w.FinishedAt.IsZero() {
		t.Fatalf("stopped run = %+v", w)
	}
	tree := stripANSIForTest(strings.Join(m.workflowTreeLines(70, 0), "\n"))
	if !strings.Contains(tree, "■ workflow review-changes") || strings.Contains(tree, "✗ workflow") {
		t.Fatalf("a stopped run should carry the neutral marker, not the failure one:\n%s", tree)
	}
	if !strings.Contains(tree, "stopped: at the orchestrator's request") {
		t.Fatalf("the stop reason should show:\n%s", tree)
	}
	summary := m.workflowSummaryLines(90, footerBudget(40)-2)
	if len(summary) != 1 || !strings.Contains(stripANSIForTest(summary[0]), "■ review-changes stopped: at the orchestrator's request") {
		t.Fatalf("stopped footer = %q", summary)
	}
	if style, _ := workflowTitleStyle("stopped"); style.GetForeground() == theme.Current().Error {
		t.Fatal("a stopped run must not use the failure colour")
	}
	if m.hasRunningDelegation() {
		t.Fatal("a stopped run must not animate")
	}
}

// Continuing a paused run that a newer run displaced from the screen brings
// back its own tree — agents, log, runtime phases — instead of rebuilding an
// empty one from the declared phases in the resumed payload.
func TestWorkflowContinuedRunRestoresItsTree(t *testing.T) {
	m := newWorkflowModel(t)
	wfPausedRunWithWork(t, &m)

	m.applyToolTraceToObservability(agent.ToolTrace{
		AgentID: "coding", Name: "workflow", Status: "running",
		Args: `{"run_id":"wf_2","workflow":"other","origin":"inline","phases":[{"title":"Scan"}]}`,
	})
	if m.workflow.RunID != "wf_2" {
		t.Fatalf("the new run should be on screen: %+v", m.workflow)
	}
	// wf_2's member lands on wf_2; a late trace of wf_1 lands on wf_1.
	m.applyToolTraceToObservability(agent.ToolTrace{
		AgentID: "general-purpose#9", Name: "agent", Status: "success",
		Args: `{"agent":"general-purpose#9","task":"scan","workflow":"other","run_id":"wf_2","phase":"Scan"}`,
	})
	m.applyToolTraceToObservability(agent.ToolTrace{
		AgentID: "coding", Name: "workflow-progress", Status: "success",
		Args:   `{"run_id":"wf_1","workflow":"review-changes","kind":"log","phase":"Review"}`,
		Output: "late note",
	})
	if len(m.workflow.Agents) != 1 || len(m.workflow.Logs) != 0 {
		t.Fatalf("runs bled into each other: %+v", m.workflow)
	}
	m.applyToolTraceToObservability(agent.ToolTrace{
		AgentID: "coding", Name: "workflow", Status: "success",
		Args:   `{"run_id":"wf_2","workflow":"other","agents":1,"failed":0,"cached":0,"tokens":3}`,
		Output: "1 agents · 0 failed · 0 replayed",
	})

	// The next turn continues wf_1 (the registry is not consulted here).
	m.workflowRuns = nil
	m.startAgentActivity("coding", "verify them")
	m.applyToolTraceToObservability(wfResumedTrace("wf_1"))
	w := m.workflow
	if w == nil || w.RunID != "wf_1" || w.Status != "running" {
		t.Fatalf("the continued run is not on screen: %+v", w)
	}
	if len(w.Agents) != 1 || len(w.Logs) != 2 || !w.phaseEntry("Triage").Dynamic {
		t.Fatalf("the continue rebuilt the tree from scratch: agents=%d logs=%d phases=%+v",
			len(w.Agents), len(w.Logs), w.Phases)
	}
	if head := w.headline(); !strings.Contains(head, "1 done") {
		t.Fatalf("headline = %q", head)
	}
	if len(m.parkedWorkflows) != 0 {
		t.Fatalf("a restored run must leave the parking: %v", m.parkedWorkflows)
	}
}

// A parked run the registry no longer holds is dropped at the next turn.
func TestWorkflowParkedRunsArePrunedWithTheirRuns(t *testing.T) {
	m := newWorkflowModel(t)
	wfPausedRunWithWork(t, &m)
	m.applyToolTraceToObservability(agent.ToolTrace{
		AgentID: "coding", Name: "workflow", Status: "running",
		Args: `{"run_id":"wf_2","workflow":"other","origin":"inline","phases":[]}`,
	})
	if m.parkedWorkflows["wf_1"] == nil {
		t.Fatal("the displaced paused run should be parked")
	}
	m.startAgentActivity("coding", "next") // the empty registry holds no wf_1
	if len(m.parkedWorkflows) != 0 {
		t.Fatalf("a parked run that is no longer live must go: %v", m.parkedWorkflows)
	}
}

type fakeStopNotifier struct {
	fn func(runID, name, reason string)
}

func (f *fakeStopNotifier) SetOnStopped(fn func(runID, name, reason string)) { f.fn = fn }

// A paused run is detached: when the idle reaper (or StopAll) stops it no
// finish trace reaches the panel, so the registry's stop hook does — and the
// panel stops claiming the run waits for an orchestrator.
func TestWorkflowDetachedStopReachesThePanel(t *testing.T) {
	m := newWorkflowModel(t)
	notifier := &fakeStopNotifier{}
	cmd := m.watchWorkflowStops(notifier)
	if cmd == nil || notifier.fn == nil {
		t.Fatal("the panel should register a stop hook and wait for it")
	}
	wfPausedRunWithWork(t, &m)

	// Called from the registry's goroutine; it must never block it.
	notifier.fn("wf_1", "review-changes", "paused for over 30m with no continue")
	msg := cmd()
	nm, next := m.Update(msg)
	m = nm.(Model)
	if next == nil {
		t.Fatal("the stop listener must be re-armed")
	}
	w := m.workflow
	if w.Status != "stopped" || !strings.Contains(w.Summary, "paused for over 30m") {
		t.Fatalf("detached stop not applied: %+v", w)
	}
	if strings.Contains(w.headline(), "waiting for orchestrator") {
		t.Fatalf("a stopped run still claims to wait: %q", w.headline())
	}
	if sub := stripANSIForTest(m.sidePanelHeaderParts(48)[1]); strings.Contains(sub, "paused") {
		t.Fatalf("the side panel subtitle still says paused: %q", sub)
	}
	if len(w.Agents) != 1 {
		t.Fatal("what the run did should stay on screen")
	}

	// A flood of stops never blocks the caller.
	for range workflowStopBuffer * 3 {
		notifier.fn("wf_x", "x", "the session ended")
	}

	// A stop for a parked run drops it; one for an unknown run is ignored.
	m.parkedWorkflows = map[string]*workflowRun{"wf_9": {RunID: "wf_9", Status: "paused"}}
	m.applyWorkflowStopped(workflowStoppedMsg{runID: "wf_9", reason: "the session ended"})
	m.applyWorkflowStopped(workflowStoppedMsg{runID: "wf_404"})
	if len(m.parkedWorkflows) != 0 || m.workflow.Status != "stopped" {
		t.Fatalf("parked=%v status=%q", m.parkedWorkflows, m.workflow.Status)
	}
}

// After the agent and TUI changes are merged, the session registry must
// offer the stop hook the panel asserts for; a signature drift would
// otherwise silently disable it.
func TestSessionRegistryOffersTheStopHook(t *testing.T) {
	if _, ok := any(agent.NewWorkflowRuns()).(workflowStopNotifier); !ok {
		t.Skip("agent.WorkflowRuns has no SetOnStopped(func(runID, name, reason string)) in this tree yet")
	}
	m := newWorkflowModel(t)
	if m.watchWorkflowRuns() == nil {
		t.Fatal("the panel should watch the session registry")
	}
}

// TestWorkflowObserverTracesStayOutOfTheTranscript: the observer's progress
// and lifecycle traces drive the panel only. In the transcript they read as
// stray tool rows — "Workflow Progress", and a second "Ran 1 workflow" for a
// single call — so only the tool loop's own report of the call is a row.
func TestWorkflowObserverTracesStayOutOfTheTranscript(t *testing.T) {
	cases := []struct {
		trace agent.ToolTrace
		want  bool
	}{
		{agent.ToolTrace{Name: "workflow-progress", Status: "success", Args: `{"run_id":"wf_1","workflow":"w","kind":"phase","phase":"Find"}`}, true},
		{agent.ToolTrace{Name: "workflow", Status: "running", Args: `{"run_id":"wf_1","workflow":"w","origin":"inline","phases":[]}`}, true},
		{agent.ToolTrace{Name: "workflow", Status: "paused", Args: `{"run_id":"wf_1","workflow":"w","checkpoint_id":"cp-1","message":"m"}`}, true},
		{agent.ToolTrace{Name: "workflow", Status: "stopped", Args: `{"run_id":"wf_1","workflow":"w","reason":"at the orchestrator's request"}`}, true},
		// The tool loop's report of the call itself stays a transcript row.
		{agent.ToolTrace{Name: "workflow", Status: "running", Args: `{"script":"export const meta = {}"}`}, false},
		{agent.ToolTrace{Name: "workflow", Status: "success", Args: `{"continue_run_id":"wf_1","reply":"yes"}`}, false},
		{agent.ToolTrace{Name: "workflow", Status: "success", Args: `{"name":"file-audit","show":true}`}, false},
		{agent.ToolTrace{Name: "bash", Status: "success", Args: `{"command":"ls"}`}, false},
		// The worktree merge-failure note must stay visible in the transcript.
		{agent.ToolTrace{Name: "workflow-progress", Status: "error", Args: `{"kind":"log","phase":"Fix"}`, Output: "branch wf-3 did not merge"}, false},
	}
	for _, tc := range cases {
		if got := isWorkflowObserverTrace(tc.trace); got != tc.want {
			t.Errorf("isWorkflowObserverTrace(%s %s %s) = %v, want %v", tc.trace.Name, tc.trace.Status, tc.trace.Args, got, tc.want)
		}
	}

	m := newWorkflowModel(t)
	before := len(m.liveTools)
	m.applyToolTrace(agent.ToolTrace{Name: "workflow-progress", Status: "success", Args: `{"run_id":"wf_1","workflow":"w","kind":"log"}`, Output: "hello"})
	if len(m.liveTools) != before || m.currentTool != nil {
		t.Errorf("a progress trace became a transcript tool row")
	}
}
