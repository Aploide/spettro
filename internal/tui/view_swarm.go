package tui

import (
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"

	"spettro/internal/theme"
)

// The Ultra swarm gets its own bordered block rather than sharing the plain
// delegation list. Two things made the old rendering hard to read: swarm
// members looked exactly like ordinary sub-agents, so a fan-out of twenty
// disappeared into a wall of identical rows; and only running members were
// drawn, so the list shrank as the swarm progressed and never showed what the
// swarm as a whole had done. This block keeps every member visible with its
// outcome, groups them under one header with a meter, and shows what each one
// is doing right now instead of the item it was handed at launch.

type swarmSummary struct {
	members []parallelAgentEntry
	types   []string
	running int
	done    int
	failed  int
}

func (m Model) swarmSummary() swarmSummary {
	var s swarmSummary
	seenType := map[string]bool{}
	for _, a := range m.parallelAgents {
		if a.Kind != "swarm" {
			continue
		}
		s.members = append(s.members, a)
		if t := swarmSpecID(a.ID); !seenType[t] {
			seenType[t] = true
			s.types = append(s.types, t)
		}
		switch a.Status {
		case "running":
			s.running++
		case "failed", "error":
			s.failed++
		default:
			s.done++
		}
	}
	return s
}

func (s swarmSummary) headline() string {
	parts := []string{fmt.Sprintf("%d running", s.running), fmt.Sprintf("%d done", s.done)}
	if s.failed > 0 {
		parts = append(parts, fmt.Sprintf("%d failed", s.failed))
	}
	return strings.Join(parts, " · ")
}

// compactHeadline is the glyph form used when the panel is too narrow to spell
// the counts out; see workflowRun.compactHeadline.
func (s swarmSummary) compactHeadline() string {
	out := fmt.Sprintf("%d▶ %d✓", s.running, s.done)
	if s.failed > 0 {
		out += fmt.Sprintf(" %d✗", s.failed)
	}
	return out
}

// swarmTitleLine renders the header, falling back to glyph counts when the
// spelled-out ones will not fit.
func (s swarmSummary) titleLine(budget int) string {
	title := "ultra swarm"
	if len(s.types) == 1 {
		title += " · " + s.types[0]
	}
	prefix := "⚡ " + truncateLabel(title, max(10, budget/2))
	headline := s.headline()
	if lipgloss.Width(prefix)+1+lipgloss.Width(headline) > budget {
		headline = s.compactHeadline()
	}
	return lipgloss.NewStyle().Bold(true).Foreground(theme.Current().Warning).Render(prefix) + " " +
		styleMuted.Render(truncateLabel(headline, max(4, budget-lipgloss.Width(prefix)-1)))
}

// swarmMemberRow renders one member: status glyph, instance name in the
// agent's manifest colour, and its live activity (falling back to the item it
// was assigned when it has not called a tool yet).
func (m Model) swarmMemberRow(a parallelAgentEntry, budget int) string {
	agentColor := modeColor("")
	if spec, ok := m.manifest.AgentByID(swarmSpecID(a.ID)); ok {
		agentColor = modeColor(spec.Color)
	}
	icon, iconStyle := agentStatusGlyph(swarmStatus(a.Status))
	nameStyle := lipgloss.NewStyle().Foreground(agentColor)
	if a.Status != "running" {
		nameStyle = nameStyle.Faint(true)
	}
	name := truncateAgentName(a.ID, max(8, budget/3))
	detail := a.Task
	if a.Status == "running" {
		if live := m.latestAgentActivity(a.ID); live != "" {
			detail = live
		}
	}
	prefix := iconStyle.Render(icon+" ") + nameStyle.Render(name)
	return prefix + " " + styleMuted.Render(
		truncateLabel(strings.ReplaceAll(detail, "\n", " "), max(6, budget-lipgloss.Width(prefix)-1)))
}

// swarmStatus normalises the two spellings a failed member can arrive with.
func swarmStatus(status string) string {
	switch status {
	case "running":
		return "running"
	case "failed", "error":
		return "failed"
	}
	return "done"
}

// swarmSummaryLines is the footer form: how far the fan-out has got and what
// is running, not the whole roster. The full list is in the side panel, which
// has the room; down here the swarm competes with the conversation.
func (m Model) swarmSummaryLines(width, rows int) []string {
	s := m.swarmSummary()
	if len(s.members) == 0 {
		return nil
	}
	budget := max(width, 24)
	title := "ultra swarm"
	if len(s.types) == 1 {
		title += " · " + s.types[0]
	}
	head := lipgloss.NewStyle().Bold(true).Foreground(theme.Current().Warning).
		Render("⚡ " + truncateLabel(title, max(10, budget/3)))

	// A finished swarm collapses to one line: nothing is moving, and the
	// conversation needs the rows back.
	if s.running == 0 {
		return []string{head + " " + styleMuted.Render(truncateLabel(s.headline(), max(6, budget-lipgloss.Width(head)-1)))}
	}

	counts := fmt.Sprintf("%d/%d", s.done+s.failed, len(s.members))
	if s.failed > 0 {
		counts += fmt.Sprintf(" · %d✗", s.failed)
	}
	// Meter and counts share the header's line rather than taking one of
	// their own — a row is a row.
	lines := []string{head + "  " +
		progressBar(min(14, max(6, budget/4)), s.done, s.failed, len(s.members)) + " " +
		styleMuted.Render(counts)}

	var live []parallelAgentEntry
	for _, a := range s.members {
		if a.Status == "running" {
			live = append(live, a)
		}
	}
	shown := min(len(live), liveRowsFor(rows, len(live)))
	for _, a := range live[:shown] {
		lines = append(lines, "  "+m.swarmMemberRow(a, budget-2))
	}
	if hidden := len(live) - shown; hidden > 0 {
		lines = append(lines, styleMuted.Render(fmt.Sprintf("  … %d more running%s", hidden, m.panelKeyHint(" · ", "the whole swarm"))))
	}
	return lines
}

// renderSwarmBlock is the footer form of the swarm view, shown under the
// transcript when the side panel is hidden. rows is the row budget it must
// fit inside, border excluded.
func (m Model) renderSwarmBlock(width, rows int) string {
	lines := m.swarmSummaryLines(width-4, rows)
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
