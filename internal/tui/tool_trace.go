package tui

import (
	tea "charm.land/bubbletea/v2"

	"spettro/internal/agent"
)

// applyToolTrace records one tool trace of the running agent: the activity
// feed and remote clients, the progress note of a "comment" call, and the
// tool row in the transcript. It returns the background commands the trace
// needs (the file diff, a throttled git and repo refresh). The caller
// refreshes the viewport, once per batch of traces.
func (m *Model) applyToolTrace(t agent.ToolTrace) []tea.Cmd {
	var cmds []tea.Cmd
	m.applyToolTraceToObservability(t)
	m.publishRemoteToolTrace(t)
	if isWorkflowObserverTrace(t) {
		return nil
	}
	if t.Name == "comment" {
		if t.Status == "success" {
			if message := extractCommentMessage(t.Args, t.Output); message != "" {
				m.setProgressNote(message)
			}
		}
		return nil
	}
	if t.Name == "todo-write" && t.Status != "running" {
		m.syncTodosFromSession()
	}
	m.trackSessionEditFromTrace(t)
	if t.Status != "running" {
		switch t.Name {
		case "file-write", "bash", "agent":
			// Refresh the side-panel file list off the Update goroutine,
			// throttled so a burst of traces does not spawn git serially
			// on the hot path.
			if cmd := m.scheduleModifiedRefresh(); cmd != nil {
				cmds = append(cmds, cmd)
			}
			// Re-scan repo files so @-mention suggestions pick up files
			// created or deleted by the tool.
			if cmd := m.scheduleRepoScan(); cmd != nil {
				cmds = append(cmds, cmd)
			}
		}
	}
	if t.Status == "running" {
		item := ToolItem{Name: t.Name, Args: t.Args, Status: "running"}
		m.currentTool = &item
		m.appendToolStreamMessage(item)
		return cmds
	}
	m.toolSeq++
	completed := ToolItem{
		Name:   t.Name,
		Status: t.Status,
		Args:   t.Args,
		Output: t.Output,
		Seq:    m.toolSeq,
	}
	// Compute the diff off the Update goroutine: computeFileDiff shells out
	// to git, which used to block Update per edit. The result is attached
	// later via toolDiffMsg keyed on Seq.
	cmds = append(cmds, computeFileDiffCmd(completed.Seq, m.cwd, t.Name, t.Args, t.Status))
	// Cap m.liveTools to bound memory and the run summary built at
	// interrupt time. When the LLM emits very large tool batches we keep
	// the most recent maxLiveTools entries so the most useful context (what
	// just happened) survives.
	m.liveTools = append(m.liveTools, completed)
	if len(m.liveTools) > maxLiveTools {
		m.liveTools = append([]ToolItem(nil), m.liveTools[len(m.liveTools)-maxLiveTools:]...)
	}
	m.currentTool = nil
	m.updateToolStreamMessage(completed)
	return cmds
}
