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
	Status      string // "running", "done", "failed"
	Summary     string
	StartedAt   time.Time
	FinishedAt  time.Time
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

	if name == "workflow" {
		if status == "running" {
			run := &workflowRun{
				RunID:       args.RunID,
				Name:        args.Workflow,
				Description: args.Description,
				Origin:      args.Origin,
				Status:      "running",
				StartedAt:   time.Now(),
			}
			for _, p := range args.Phases {
				run.Phases = append(run.Phases, workflowPhaseEntry{Title: p.Title, Detail: p.Detail})
			}
			m.workflow = run
			if !m.showSidePanel {
				m.showBanner("workflow "+run.Name+m.panelKeyHint(" — ", "the phase tree"), "info")
			}
			return true
		}
		if m.workflow == nil {
			return true
		}
		m.workflow.Status = "done"
		if status == "error" {
			m.workflow.Status = "failed"
		}
		m.workflow.Summary = strings.TrimSpace(output)
		m.workflow.FinishedAt = time.Now()
		return true
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
		for _, p := range m.workflow.Phases {
			if p.Title == title {
				return true
			}
		}
		m.workflow.Phases = append(m.workflow.Phases, workflowPhaseEntry{Title: title})
	case "log":
		if msg := strings.TrimSpace(output); msg != "" {
			m.workflow.Logs = append(m.workflow.Logs, workflowLogEntry{
				Phase: args.Phase, Message: msg, At: time.Now(),
			})
		}
	}
	return true
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
