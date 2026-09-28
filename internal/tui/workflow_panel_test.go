package tui

import (
	"fmt"
	"strings"
	"testing"

	"spettro/internal/agent"
	"spettro/internal/config"
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
		Args:   `{"run_id":"wf_1","workflow":"review-changes"}`,
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
