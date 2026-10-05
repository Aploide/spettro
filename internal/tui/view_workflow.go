package tui

import (
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"

	"spettro/internal/theme"
)

// The workflow panel is a phase tree, not a flat agent list: a workflow's
// whole point is that the structure was decided before the run, so the panel
// shows every declared phase from the start — including ones nothing has
// reached yet — and fills it in as agents land. A flat list would make a
// 3-phase, 20-agent run unreadable and would hide what is still to come.

// progressBar renders a fixed-width meter. Done and failed both count as
// finished — a failed agent is not still working — but failures are drawn in
// their own colour so a red-heavy bar reads as trouble at a glance.
func progressBar(width, done, failed, total int) string {
	if width < 4 {
		width = 4
	}
	pal := theme.Current()
	if total <= 0 {
		return lipgloss.NewStyle().Foreground(pal.Rule).Render(strings.Repeat("░", width))
	}
	doneCells := done * width / total
	failCells := failed * width / total
	if failed > 0 && failCells == 0 {
		failCells = 1
	}
	if doneCells+failCells > width {
		doneCells = width - failCells
	}
	if doneCells < 0 {
		doneCells = 0
	}
	rest := width - doneCells - failCells
	var b strings.Builder
	b.WriteString(lipgloss.NewStyle().Foreground(pal.Success).Render(strings.Repeat("█", doneCells)))
	b.WriteString(lipgloss.NewStyle().Foreground(pal.Error).Render(strings.Repeat("█", failCells)))
	b.WriteString(lipgloss.NewStyle().Foreground(pal.Rule).Render(strings.Repeat("░", rest)))
	return b.String()
}

// truncateAgentName shortens an instance name while keeping its "#N" suffix.
// Workflow members share a long agent-type prefix ("general-purpose#7"), so a
// plain truncation clips off the only part that tells them apart.
func truncateAgentName(name string, width int) string {
	if width < 4 || lipgloss.Width(name) <= width {
		return truncateLabel(name, width)
	}
	hash := strings.LastIndexByte(name, '#')
	if hash <= 0 {
		return truncateLabel(name, width)
	}
	suffix := name[hash:]
	keep := width - lipgloss.Width(suffix) - 1
	if keep < 1 {
		return truncateLabel(name, width)
	}
	return name[:keep] + "…" + suffix
}

func agentStatusGlyph(status string) (string, lipgloss.Style) {
	switch status {
	case "running":
		return "▶", lipgloss.NewStyle().Foreground(theme.Current().Info)
	case "failed":
		return "✗", lipgloss.NewStyle().Foreground(theme.Current().Error)
	default:
		return "✓", lipgloss.NewStyle().Foreground(theme.Current().Success)
	}
}

// workflowTitleStyle is the run's title colour and marker. A paused run gets
// its own: it is neither working (nothing animates, nothing is spending) nor
// finished — it is waiting on the orchestrator, and must not read as either.
func workflowTitleStyle(status string) (lipgloss.Style, string) {
	pal := theme.Current()
	switch status {
	case "failed":
		return lipgloss.NewStyle().Bold(true).Foreground(pal.Error), "✗"
	case "running":
		return lipgloss.NewStyle().Bold(true).Foreground(pal.Info), "◆"
	case "paused":
		return lipgloss.NewStyle().Bold(true).Foreground(pal.Warning), "⏸"
	default:
		return lipgloss.NewStyle().Bold(true).Foreground(pal.Success), "✓"
	}
}

// pausedLabel is "paused at cp-2", or just "paused" when the trace named no
// checkpoint.
func (w *workflowRun) pausedLabel() string {
	if w.CheckpointID == "" {
		return "paused"
	}
	return "paused at " + w.CheckpointID
}

// waitingLine is what a paused run is waiting for, in the script's words.
func (w *workflowRun) waitingLine() string {
	if w.CheckpointMessage == "" {
		return "waiting for orchestrator"
	}
	return "waiting for orchestrator: " + w.CheckpointMessage
}

// sizeLabel is the run's size tier and token budget for its title:
// "large · 500k budget". Empty when the run reported neither.
func (w *workflowRun) sizeLabel(withBudget bool) string {
	var parts []string
	if w.Size != "" {
		parts = append(parts, w.Size)
	}
	if withBudget && w.BudgetTokens > 0 {
		parts = append(parts, compactTokenCount(w.BudgetTokens)+" budget")
	}
	return strings.Join(parts, " · ")
}

// compactTokenCount is formatTokenCount without the ".0" of a round number:
// a "+500k" directive reads back as "500k", not "500.0k".
func compactTokenCount(n int) string {
	return strings.Replace(formatTokenCount(n), ".0", "", 1)
}

// workflowHeadline is the one-line summary shown in the panel title and in the
// compact footer block.
func (w *workflowRun) headline() string {
	if w.Status == "paused" {
		return w.pausedLabel() + " — " + w.waitingLine()
	}
	running, done, failed, cached := w.counts()
	parts := []string{fmt.Sprintf("%d running", running), fmt.Sprintf("%d done", done)}
	if failed > 0 {
		parts = append(parts, fmt.Sprintf("%d failed", failed))
	}
	if cached > 0 {
		parts = append(parts, fmt.Sprintf("%d replayed", cached))
	}
	return strings.Join(parts, " · ")
}

// compactHeadline is the same counts in glyphs, for a panel too narrow to
// spell them out. Clipping "1 failed" to "1 fai…" would hide the one number
// that matters, so the narrow case gets its own form rather than a truncation.
func (w *workflowRun) compactHeadline() string {
	if w.Status == "paused" {
		return w.pausedLabel()
	}
	running, done, failed, _ := w.counts()
	out := fmt.Sprintf("%d▶ %d✓", running, done)
	if failed > 0 {
		out += fmt.Sprintf(" %d✗", failed)
	}
	return out
}

// workflowRow is one agent line, tagged so the row budget drops the least
// interesting work first.
type workflowRow struct {
	line string
	// prio orders survival when rows must be dropped: still running beats
	// failed beats succeeded. A failure is the row a reader most wants to see
	// among finished work, so it must outlive the successes around it.
	prio int
}

const (
	wfRowRunning = iota
	wfRowFailed
	wfRowDone
)

// workflowGroup is a phase header plus the agents under it.
type workflowGroup struct {
	header string
	rows   []workflowRow
}

// workflowTreeLines renders the panel body: title, description, one group per
// phase with its own meter and agent rows, then the tail of the script's log.
//
// maxRows caps the output (0 = uncapped, which is what the side panel uses).
// The cap never drops a phase: a phase nobody has reached yet is exactly the
// information the panel exists to show, so trimming takes finished agent rows
// instead and reports how many it hid.
func (m Model) workflowTreeLines(width, maxRows int) []string {
	w := m.workflow
	if w == nil {
		return nil
	}
	budget := max(width, 24)

	titleStyle, marker := workflowTitleStyle(w.Status)

	running, done, failed, _ := w.counts()
	total := running + done + failed
	// The header is built to fit: in the side panel an unclamped one wraps
	// onto a second line and knocks the whole tree out of alignment.
	name := truncateLabel(w.Name, max(10, budget/2))
	headline := w.headline()
	if w.Status == "paused" {
		// The message gets a line of its own below; the title only says
		// where the run stopped.
		headline = w.pausedLabel()
	}
	prefix := marker + " workflow " + name
	// The size tier (and budget) sit after the name — they say how big this
	// run is allowed to grow — but give way, budget first, before even the
	// compact headline would have to be clipped.
	need := 1 + lipgloss.Width(w.compactHeadline())
	for _, withBudget := range []bool{true, false} {
		if size := w.sizeLabel(withBudget); size != "" && lipgloss.Width(prefix+" · "+size)+need <= budget {
			prefix += " · " + size
			break
		}
	}
	if lipgloss.Width(prefix)+1+lipgloss.Width(headline) > budget {
		headline = w.compactHeadline()
	}
	title := titleStyle.Render(prefix) + " " +
		styleMuted.Render(truncateLabel(headline, max(4, budget-lipgloss.Width(prefix)-1)))
	head := []string{title}
	if w.Status == "paused" {
		head = append(head, lipgloss.NewStyle().Foreground(theme.Current().Warning).
			Render("  "+truncateLabel(strings.ReplaceAll(w.waitingLine(), "\n", " "), budget-2)))
	}
	if w.Description != "" {
		head = append(head, styleMuted.Render("  "+truncateLabel(w.Description, budget-2)))
	}
	if total > 0 {
		head = append(head, "  "+progressBar(min(24, max(8, budget-24)), done, failed, total)+" "+
			styleMuted.Render(fmt.Sprintf("%d/%d", done+failed, total)))
	}

	var groups []workflowGroup
	for _, phase := range w.phaseOrder() {
		groups = append(groups, m.workflowPhaseGroup(w, phase, budget))
	}

	var tail []string
	if len(w.Logs) > 0 {
		tail = append(tail, "")
		for _, entry := range lastLogs(w.Logs, 4) {
			tail = append(tail, styleMuted.Render("  log ")+
				lipgloss.NewStyle().Foreground(theme.Current().Text).Render(truncateLabel(entry.Message, budget-7)))
		}
	}
	if w.Status != "running" && w.Summary != "" {
		tail = append(tail, styleMuted.Render("  "+truncateLabel(w.Summary, budget-2)))
	}

	return assembleWorkflowTree(head, groups, tail, maxRows)
}

// assembleWorkflowTree flattens the tree, dropping rows only when it has to.
// Phase headers and running agents are never dropped; finished agents go
// first, then the log tail, and whatever was hidden is counted in a final line.
func assembleWorkflowTree(head []string, groups []workflowGroup, tail []string, maxRows int) []string {
	totalRows, runningRows := 0, 0
	for _, g := range groups {
		totalRows += len(g.rows)
		for _, r := range g.rows {
			if r.prio == wfRowRunning {
				runningRows++
			}
		}
	}
	fixed := len(head) + len(groups)
	full := fixed + totalRows + len(tail)
	if maxRows <= 0 || full <= maxRows {
		return flattenWorkflowTree(head, groups, tail, nil, 0)
	}
	// Reserve one row for the "… N hidden" notice.
	spare := maxRows - fixed - 1
	keepTail := len(tail)
	if spare-keepTail < runningRows {
		keepTail = 0
	}
	allowedFinished := max(spare-keepTail-runningRows, 0)
	hidden := (totalRows - runningRows - allowedFinished) + (len(tail) - keepTail)
	// Spend the finished-row budget on failures first, then successes, so a
	// trimmed panel still shows what went wrong.
	keep := map[int]bool{}
	budgetLeft := allowedFinished
	for _, prio := range []int{wfRowFailed, wfRowDone} {
		i := 0
		for _, g := range groups {
			for _, r := range g.rows {
				if r.prio != wfRowRunning {
					if r.prio == prio && budgetLeft > 0 {
						keep[i] = true
						budgetLeft--
					}
				}
				i++
			}
		}
	}
	return flattenWorkflowTree(head, groups, tail[:keepTail], keep, hidden)
}

// flattenWorkflowTree emits the lines. A nil keep set means "no trimming";
// otherwise a finished row survives only if its index is in the set.
func flattenWorkflowTree(head []string, groups []workflowGroup, tail []string, keep map[int]bool, hidden int) []string {
	lines := append([]string(nil), head...)
	i := 0
	for _, g := range groups {
		lines = append(lines, g.header)
		for _, r := range g.rows {
			if r.prio == wfRowRunning || keep == nil || keep[i] {
				lines = append(lines, r.line)
			}
			i++
		}
	}
	lines = append(lines, tail...)
	if hidden > 0 {
		lines = append(lines, styleMuted.Render(fmt.Sprintf("  … %d finished rows hidden — ctrl+b for the full tree", hidden)))
	}
	return lines
}

func lastLogs(logs []workflowLogEntry, n int) []workflowLogEntry {
	if len(logs) <= n {
		return logs
	}
	return logs[len(logs)-n:]
}

func (m Model) workflowPhaseGroup(w *workflowRun, phase string, budget int) workflowGroup {
	agents := w.agentsInPhase(phase)
	title := phase
	if title == "" {
		title = "(no phase)"
	}
	pDone, pFailed, pRunning := 0, 0, 0
	for _, a := range agents {
		switch a.Status {
		case "running":
			pRunning++
		case "failed":
			pFailed++
		default:
			pDone++
		}
	}
	// A phase with nothing running and nothing finished has not been reached
	// yet; it is still listed, dimmed, because knowing what is coming is half
	// the value of declaring phases up front.
	glyph, style := "○", lipgloss.NewStyle().Foreground(theme.Current().TextDim)
	switch {
	case pRunning > 0:
		glyph, style = "▸", lipgloss.NewStyle().Bold(true).Foreground(theme.Current().Info)
	case pFailed > 0:
		glyph, style = "▸", lipgloss.NewStyle().Bold(true).Foreground(theme.Current().Warning)
	case pDone > 0:
		glyph, style = "▸", lipgloss.NewStyle().Bold(true).Foreground(theme.Current().Text)
	}
	entry := w.phaseEntry(phase)
	head := "  " + style.Render(glyph+" ")
	if entry.Dynamic {
		// A phase the script opened at runtime, not one meta planned: a dim
		// "+" says the run grew this stage after seeing interim results.
		head += styleMuted.Render("+")
	}
	head += style.Render(truncateLabel(title, max(8, budget/2)))
	if len(agents) > 0 {
		head += "  " + progressBar(10, pDone, pFailed, len(agents)) + " " +
			styleMuted.Render(fmt.Sprintf("%d/%d", pDone+pFailed, len(agents)))
	}
	// The phase's detail, when there is room for enough of it to say
	// something; a few clipped letters would only be noise.
	if entry.Detail != "" {
		if room := budget - lipgloss.Width(head) - 3; room >= 10 {
			head += styleMuted.Render(" — " + truncateLabel(strings.ReplaceAll(entry.Detail, "\n", " "), room))
		}
	}
	group := workflowGroup{header: head}
	for _, a := range agents {
		icon, iconStyle := agentStatusGlyph(a.Status)
		name := truncateAgentName(a.Instance, max(8, budget/3))
		detail := a.Label
		if a.Status == "failed" && a.Detail != "" {
			detail = a.Detail
		} else if a.Status == "running" {
			if live := m.latestAgentActivity(a.Instance); live != "" {
				detail = live
			}
		}
		if a.Cached {
			detail = "replayed · " + detail
		}
		prefix := "    " + iconStyle.Render(icon+" ") + lipgloss.NewStyle().Foreground(theme.Current().Text).Render(name)
		prio := wfRowDone
		switch a.Status {
		case "running":
			prio = wfRowRunning
		case "failed":
			prio = wfRowFailed
		}
		group.rows = append(group.rows, workflowRow{
			prio: prio,
			line: prefix + " " + styleMuted.Render(
				truncateLabel(strings.ReplaceAll(detail, "\n", " "), max(6, budget-lipgloss.Width(prefix)-1))),
		})
	}
	return group
}

// currentPhase is the phase a reader should be looking at: the one with work
// in flight, else the last one that produced anything, else the first that has
// not been reached.
func (w *workflowRun) currentPhase() (title string, done, failed, total int, pending bool) {
	order := w.phaseOrder()
	best, bestRank := "", -1
	for _, phase := range order {
		agents := w.agentsInPhase(phase)
		rank := 0 // untouched
		for _, a := range agents {
			if a.Status == "running" {
				rank = 2
				break
			}
			rank = 1
		}
		if rank >= bestRank && rank > 0 {
			best, bestRank = phase, rank
		}
	}
	if bestRank < 0 || best == "" && bestRank <= 0 {
		for _, phase := range order {
			return phase, 0, 0, 0, true
		}
		return "", 0, 0, 0, true
	}
	for _, a := range w.agentsInPhase(best) {
		total++
		switch a.Status {
		case "running":
		case "failed":
			failed++
		default:
			done++
		}
	}
	if best == "" {
		best = "(no phase)"
	}
	return best, done, failed, total, false
}

// liveRowsFor is how many agent rows fit in a row budget, once the header and
// — when there is more live work than fits — the "… N more" line are paid for.
// Forgetting the second one is how a block quietly overruns its allowance.
func liveRowsFor(rows, live int) int {
	avail := rows - 1 // header
	if live > avail {
		avail-- // the "… N more running" line
	}
	return max(avail, 1)
}

// workflowFooterCap bounds the footer summary however much room there is. The
// full tree lives in the side panel; down here it competes with the
// conversation, and a fan-out of twenty agents must never be the reason you
// cannot read what the agent just said.
const workflowFooterCap = 5

// workflowSummaryLines is the footer form: where the run is now and what is
// running, not the whole plan.
func (m Model) workflowSummaryLines(width, rows int) []string {
	w := m.workflow
	if w == nil {
		return nil
	}
	budget := max(width, 24)
	running, done, failed, _ := w.counts()
	total := running + done + failed

	titleStyle, marker := workflowTitleStyle(w.Status)
	head := titleStyle.Render(marker + " " + truncateLabel(w.Name, max(10, budget/3)))

	// A finished run collapses to one line: the detail stopped being live and
	// the conversation needs the rows back. So does a paused one — nothing in
	// it moves until the orchestrator answers — and its line says what the
	// run is waiting on, not a result it does not have yet.
	if w.Status != "running" {
		tail := w.Summary
		if tail == "" || w.Status == "paused" {
			tail = strings.ReplaceAll(w.headline(), "\n", " ")
		}
		return []string{head + " " + styleMuted.Render(truncateLabel(tail, max(6, budget-lipgloss.Width(head)-1)))}
	}

	phase, pDone, pFailed, pTotal, pending := w.currentPhase()
	phaseBit := ""
	if phase != "" {
		if pending {
			phaseBit = styleMuted.Render(" ○ " + truncateLabel(phase, max(6, budget/4)))
		} else {
			phaseBit = lipgloss.NewStyle().Bold(true).Foreground(theme.Current().Info).
				Render(" ▸ "+truncateLabel(phase, max(6, budget/4))) +
				styleMuted.Render(fmt.Sprintf(" %d/%d", pDone+pFailed, pTotal))
		}
	}
	counts := fmt.Sprintf("%d/%d", done+failed, total)
	if failed > 0 {
		counts += fmt.Sprintf(" · %d✗", failed)
	}
	line := head + phaseBit + "  " + progressBar(min(14, max(6, budget/4)), done, failed, total) +
		" " + styleMuted.Render(counts)
	lines := []string{line}

	// Then only what is in flight — finished rows are history, and history is
	// what the side panel is for.
	var live []workflowAgentEntry
	for _, a := range w.Agents {
		if a.Status == "running" {
			live = append(live, a)
		}
	}
	shown := min(len(live), liveRowsFor(min(rows, workflowFooterCap), len(live)))
	for _, a := range live[:shown] {
		icon, iconStyle := agentStatusGlyph(a.Status)
		name := truncateAgentName(a.Instance, max(8, budget/3))
		detail := a.Label
		if activity := m.latestAgentActivity(a.Instance); activity != "" {
			detail = activity
		}
		prefix := "  " + iconStyle.Render(icon+" ") + lipgloss.NewStyle().Foreground(theme.Current().Text).Render(name)
		lines = append(lines, prefix+" "+styleMuted.Render(
			truncateLabel(strings.ReplaceAll(detail, "\n", " "), max(6, budget-lipgloss.Width(prefix)-1))))
	}
	if hidden := len(live) - shown; hidden > 0 {
		lines = append(lines, styleMuted.Render(fmt.Sprintf("  … %d more running%s", hidden, m.panelKeyHint(" · ", "the full tree"))))
	} else if len(w.Phases) > 1 && len(lines)+shown < rows && m.sidePanelFits() {
		// The hint only earns a row when one is going spare; on a short
		// terminal every row belongs to the conversation.
		lines = append(lines, styleMuted.Render("  ctrl+b for the full tree"))
	}
	return lines
}

// renderWorkflowBlock is the footer form of the panel, shown under the
// transcript when the side panel is hidden.
func (m Model) renderWorkflowBlock(width, rows int) string {
	lines := m.workflowSummaryLines(width-4, rows)
	if len(lines) == 0 {
		return ""
	}
	return lipgloss.NewStyle().
		Width(width-2).
		BorderStyle(lipgloss.RoundedBorder()).
		BorderForeground(theme.Current().Border).
		Padding(0, 1).
		Render(strings.Join(lines, "\n"))
}
