package tui

import (
	"encoding/json"
	"fmt"
	"image/color"
	"strings"

	"charm.land/lipgloss/v2"

	"spettro/internal/agent"
	"spettro/internal/config"
	"spettro/internal/diff"
)

func renderToolGroups(tools []ToolItem, showTools, fullOutput bool, mc color.Color) string {
	// Line caps for tool outputs; lifted when the user toggled full output
	// with ctrl+g (0 = unlimited, scrollback handles the length).
	singleCap, groupCap := 20, 8
	if fullOutput {
		singleCap, groupCap = 0, 0
	}
	if len(tools) == 0 {
		return ""
	}
	bullet := lipgloss.NewStyle().Foreground(mc).Bold(true).Render("  ●")
	errStyle := lipgloss.NewStyle().Foreground(colorError)
	outputStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#4B5563")).Italic(true)
	var lines []string

	i := 0
	for i < len(tools) {
		j := i
		for j < len(tools) && tools[j].Name == tools[i].Name {
			j++
		}
		group := tools[i:j]
		count := len(group)
		name := group[0].Name

		if count == 1 {
			item := group[0]
			label := formatToolLabel(name, item.Args)
			if item.Status == "running" {
				label = formatRunningLabel(name, item.Args)
				label = styleMuted.Render(label)
			} else if item.Status == "error" {
				label = errStyle.Render(label)
			} else {
				label = styleMuted.Render(label)
			}
			lines = append(lines, bullet+" "+label)
			if showTools {
				if p := extractToolPath(name, item.Args); p != "" {
					icon := "✓"
					if item.Status == "running" {
						icon = ""
					} else if item.Status == "error" {
						icon = "✗"
					}
					line := fmt.Sprintf("    ⎿  %s", p)
					if icon != "" {
						line += " " + icon
					}
					lines = append(lines, styleMuted.Render(line))
				}
			}
			if item.Diff != "" && item.Status != "running" {
				if block := renderDiffBlock(item.Diff, showTools); block != "" {
					lines = append(lines, block)
				}
			} else if showTools && item.Status != "running" {
				if out := trimToolOutput(item.Output, singleCap); out != "" {
					for ol := range strings.SplitSeq(out, "\n") {
						lines = append(lines, outputStyle.Render("       "+ol))
					}
				}
			}
		} else {
			label := formatToolGroupLabel(name, group)
			if !showTools {
				label += "  " + styleMuted.Render("(ctrl+o to expand)")
			}
			lines = append(lines, bullet+" "+styleMuted.Render(label))
			if showTools {
				for _, gt := range group {
					var detail string
					if p := extractToolPath(gt.Name, gt.Args); p != "" {
						icon := "✓"
						if gt.Status == "running" {
							icon = ""
						} else if gt.Status == "error" {
							icon = "✗"
						}
						detail = "    ⎿  " + p
						if icon != "" {
							detail += " " + icon
						}
					} else {
						if gt.Status == "running" {
							detail = "    ⎿  " + formatRunningLabel(gt.Name, gt.Args)
						} else {
							detail = "    ⎿  " + formatToolLabel(gt.Name, gt.Args)
						}
					}
					lines = append(lines, styleMuted.Render(detail))
					if gt.Status != "running" {
						if out := trimToolOutput(gt.Output, groupCap); out != "" {
							for ol := range strings.SplitSeq(out, "\n") {
								lines = append(lines, outputStyle.Render("       "+ol))
							}
						}
					}
				}
			}
		}

		i = j
	}
	return strings.Join(lines, "\n")
}

func hasRunningTool(items []ToolItem) bool {
	for _, item := range items {
		if item.Status == "running" {
			return true
		}
	}
	return false
}

func formatRunningToolGroupLabel(name string, group []ToolItem) string {
	count := len(group)
	if desc := formatDetailedGroupLabel(name, true, group); desc != "" {
		return desc
	}
	switch name {
	case "file-read":
		if count == 1 {
			return "Reading 1 file…"
		}
		return fmt.Sprintf("Reading %d files…", count)
	case "file-write":
		if count == 1 {
			return "Writing 1 file…"
		}
		return fmt.Sprintf("Writing %d files…", count)
	case "file-edit", "multi-edit":
		if count == 1 {
			return "Editing 1 file…"
		}
		return fmt.Sprintf("Editing %d files…", count)
	case "repo-search":
		if count == 1 {
			return "Searching 1 query…"
		}
		return fmt.Sprintf("Searching %d queries…", count)
	case "tool-search":
		if count == 1 {
			return "Searching 1 tool query…"
		}
		return fmt.Sprintf("Searching %d tool queries…", count)
	case "web-search":
		if count == 1 {
			return "Searching 1 web query…"
		}
		return fmt.Sprintf("Searching %d web queries…", count)
	case "web-fetch":
		if count == 1 {
			return "Fetching 1 page…"
		}
		return fmt.Sprintf("Fetching %d pages…", count)
	case "download":
		if count == 1 {
			return "Downloading 1 file…"
		}
		return fmt.Sprintf("Downloading %d files…", count)
	case "shell-exec", "bash", "bash-output":
		if count == 1 {
			return "Running 1 command…"
		}
		return fmt.Sprintf("Running %d commands…", count)
	case "glob":
		if count == 1 {
			return "Matching 1 pattern…"
		}
		return fmt.Sprintf("Matching %d patterns…", count)
	case "grep":
		if count == 1 {
			return "Grepping 1 pattern…"
		}
		return fmt.Sprintf("Grepping %d patterns…", count)
	case "ls":
		if count == 1 {
			return "Listing 1 directory…"
		}
		return fmt.Sprintf("Listing %d directories…", count)
	case "task-create":
		if count == 1 {
			return "Creating 1 task…"
		}
		return fmt.Sprintf("Creating %d tasks…", count)
	case "task-get":
		if count == 1 {
			return "Reading 1 task…"
		}
		return fmt.Sprintf("Reading %d tasks…", count)
	case "task-update":
		if count == 1 {
			return "Updating 1 task…"
		}
		return fmt.Sprintf("Updating %d tasks…", count)
	case "task-list":
		if count == 1 {
			return "Listing tasks…"
		}
		return fmt.Sprintf("Listing tasks %d times…", count)
	case "task-delete":
		if count == 1 {
			return "Deleting 1 task…"
		}
		return fmt.Sprintf("Deleting %d tasks…", count)
	case "ask-user":
		if count == 1 {
			return "Asking 1 question…"
		}
		return fmt.Sprintf("Asking %d questions…", count)
	case "mcp-list-resources":
		if count == 1 {
			return "Listing MCP resources…"
		}
		return fmt.Sprintf("Listing MCP resources %d times…", count)
	case "mcp-read-resource":
		if count == 1 {
			return "Reading 1 MCP resource…"
		}
		return fmt.Sprintf("Reading %d MCP resources…", count)
	case "mcp-auth":
		if count == 1 {
			return "Updating MCP auth…"
		}
		return fmt.Sprintf("Updating MCP auth %d times…", count)
	case "enter-worktree":
		if count == 1 {
			return "Entering 1 worktree…"
		}
		return fmt.Sprintf("Entering %d worktrees…", count)
	case "exit-worktree":
		if count == 1 {
			return "Exiting 1 worktree…"
		}
		return fmt.Sprintf("Exiting %d worktrees…", count)
	case "send-message":
		if count == 1 {
			return "Sending 1 message…"
		}
		return fmt.Sprintf("Sending %d messages…", count)
	case "agent":
		if count == 1 {
			return "Delegating 1 task…"
		}
		return fmt.Sprintf("Delegating %d tasks…", count)
	}
	return fmt.Sprintf("Using %s %d time(s)…", humanizeToolID(name), count)
}

func formatToolGroupLabel(name string, group []ToolItem) string {
	count := len(group)
	if hasRunningTool(group) {
		return formatRunningToolGroupLabel(name, group)
	}
	if desc := formatDetailedGroupLabel(name, false, group); desc != "" {
		return desc
	}
	return fmt.Sprintf("%s %s", toolActionVerb(name), toolNounCount(name, count))
}

func formatDetailedGroupLabel(name string, running bool, group []ToolItem) string {
	count := len(group)
	if count <= 0 {
		return ""
	}
	verb := toolActionVerb(name)
	if running {
		verb = runningVerb(name)
	}
	maxShown := min(len(group), 3)
	labels := make([]string, 0, maxShown)
	for i := 0; i < maxShown; i++ {
		if d := toolDescriptor(name, group[i].Args); d != "" {
			labels = append(labels, d)
		}
	}
	if len(labels) == 0 {
		return ""
	}
	prefix := ""
	switch name {
	case "repo-search", "tool-search", "web-search", "grep":
		prefix = " for "
	case "agent":
		prefix = " to "
	default:
		prefix = " "
	}
	label := verb + prefix + strings.Join(labels, ", ")
	if count > len(labels) {
		label += fmt.Sprintf(" (+%d more)", count-len(labels))
	}
	if running {
		label += "…"
	}
	return label
}

func toolDescriptor(name, argsJSON string) string {
	switch name {
	case "file-read", "file-write", "file-edit", "multi-edit", "enter-worktree", "exit-worktree", "ls":
		var args struct {
			Path string `json:"path"`
		}
		if json.Unmarshal([]byte(argsJSON), &args) == nil && strings.TrimSpace(args.Path) != "" {
			return truncateLabel(args.Path, 36)
		}
	case "repo-search", "tool-search", "web-search", "grep":
		var args struct {
			Query   string `json:"query"`
			Pattern string `json:"pattern"`
		}
		if json.Unmarshal([]byte(argsJSON), &args) == nil {
			q := strings.TrimSpace(args.Query)
			if q == "" {
				q = strings.TrimSpace(args.Pattern)
			}
			if q != "" {
				return fmt.Sprintf("%q", truncateLabel(q, 36))
			}
		}
	case "web-fetch":
		var args struct {
			URL string `json:"url"`
		}
		if json.Unmarshal([]byte(argsJSON), &args) == nil && strings.TrimSpace(args.URL) != "" {
			return fmt.Sprintf("%q", truncateLabel(args.URL, 36))
		}
	case "download":
		var args struct {
			URL string `json:"url"`
		}
		if json.Unmarshal([]byte(argsJSON), &args) == nil && strings.TrimSpace(args.URL) != "" {
			return fmt.Sprintf("%q", truncateLabel(args.URL, 36))
		}
	case "shell-exec", "bash", "bash-output":
		var args struct {
			Command string `json:"command"`
		}
		if json.Unmarshal([]byte(argsJSON), &args) == nil && strings.TrimSpace(args.Command) != "" {
			return "$ " + truncateLabel(args.Command, 36)
		}
	case "mcp-read-resource":
		var args struct {
			ResourceID string `json:"resource_id"`
		}
		if json.Unmarshal([]byte(argsJSON), &args) == nil && strings.TrimSpace(args.ResourceID) != "" {
			return truncateLabel(args.ResourceID, 36)
		}
	case "agent":
		var args struct {
			Agent  string `json:"agent"`
			Target string `json:"target"`
			ID     string `json:"id"`
		}
		if json.Unmarshal([]byte(argsJSON), &args) == nil {
			target := strings.TrimSpace(args.Agent)
			if target == "" {
				target = strings.TrimSpace(args.Target)
			}
			if target == "" {
				target = strings.TrimSpace(args.ID)
			}
			if target != "" {
				return truncateLabel(target, 24)
			}
		}
	}
	return ""
}

func runningVerb(name string) string {
	switch name {
	case "file-read":
		return "Reading"
	case "file-write":
		return "Writing"
	case "file-edit", "multi-edit":
		return "Editing"
	case "repo-search", "tool-search", "web-search":
		return "Searching"
	case "web-fetch":
		return "Fetching"
	case "download":
		return "Downloading"
	case "shell-exec", "bash", "bash-output":
		return "Running"
	case "glob":
		return "Matching"
	case "grep":
		return "Grepping"
	case "ls":
		return "Listing"
	case "todo-write":
		return "Writing"
	case "task-create":
		return "Creating"
	case "task-get":
		return "Reading"
	case "task-update":
		return "Updating"
	case "task-list":
		return "Listing"
	case "task-delete":
		return "Deleting"
	case "ask-user":
		return "Asking"
	case "enter-plan-mode":
		return "Entering"
	case "exit-plan-mode":
		return "Exiting"
	case "mcp-list-resources":
		return "Listing"
	case "mcp-read-resource":
		return "Reading"
	case "mcp-auth":
		return "Updating"
	case "enter-worktree":
		return "Entering"
	case "exit-worktree":
		return "Exiting"
	case "send-message":
		return "Sending"
	case "agent":
		return "Delegating"
	}
	return "Using"
}

func humanizeToolID(name string) string {
	if strings.TrimSpace(name) == "" {
		return "Tool"
	}
	name = strings.ReplaceAll(name, "-", " ")
	name = strings.ReplaceAll(name, "_", " ")
	parts := strings.Fields(name)
	for i, p := range parts {
		if p == "" {
			continue
		}
		parts[i] = strings.ToUpper(p[:1]) + strings.ToLower(p[1:])
	}
	if len(parts) == 0 {
		return "Tool"
	}
	return strings.Join(parts, " ")
}

func formatApprovalCommandLabel(command string) string {
	command = strings.TrimSpace(command)
	if command == "" {
		return ""
	}
	parts := strings.Fields(command)
	if len(parts) >= 2 && parts[0] == "network" {
		toolID := parts[1]
		target := strings.TrimSpace(strings.Join(parts[2:], " "))
		if target == "" {
			target = "network target"
		}
		switch toolID {
		case "web-search":
			return fmt.Sprintf("Searching web for %q", truncateLabel(target, 60))
		case "web-fetch":
			return fmt.Sprintf("Fetching %s", truncateLabel(target, 60))
		case "download":
			return fmt.Sprintf("Downloading %s", truncateLabel(target, 60))
		case "mcp-list-resources":
			return fmt.Sprintf("Listing MCP resources for %s", truncateLabel(target, 40))
		case "mcp-read-resource":
			return fmt.Sprintf("Reading MCP resource %s", truncateLabel(target, 50))
		case "mcp-auth":
			return fmt.Sprintf("Updating MCP auth for %s", truncateLabel(target, 40))
		default:
			return fmt.Sprintf("Using network tool %s on %s", humanizeToolID(toolID), truncateLabel(target, 50))
		}
	}
	return "$ " + command
}

func renderDiffBlock(diffText string, expanded bool) string {
	maxLines := 20
	if expanded {
		maxLines = 0
	}
	return diff.Render(diffText, diff.Options{
		MaxLines:   maxLines,
		ExpandHint: "(ctrl+o to expand)",
		Indent:     "       ",
	})
}

// trimToolOutput caps output at maxLines with a "… N more lines" footer;
// maxLines <= 0 means no cap.
func trimToolOutput(output string, maxLines int) string {
	output = strings.TrimSpace(output)
	if output == "" {
		return ""
	}
	lines := strings.Split(output, "\n")
	if maxLines <= 0 || len(lines) <= maxLines {
		return output
	}
	remaining := len(lines) - maxLines
	return strings.Join(lines[:maxLines], "\n") + fmt.Sprintf("\n  … %d more lines", remaining)
}

func toToolItems(traces []agent.ToolTrace) []ToolItem {
	if len(traces) == 0 {
		return nil
	}
	out := make([]ToolItem, 0, len(traces))
	for _, t := range traces {
		out = append(out, ToolItem{
			Name:   t.Name,
			Status: t.Status,
			Args:   t.Args,
			Output: t.Output,
		})
	}
	return out
}

func indent(s, prefix string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = prefix + l
	}
	return strings.Join(lines, "\n")
}

func truncateLabel(s string, max int) string {
	if max <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	if max <= 1 {
		return string(r[:max])
	}
	return string(r[:max-1]) + "…"
}

func nextAgent(manifest config.AgentManifest, current string) string {
	primary := primaryAgentIDs(manifest)
	if len(primary) == 0 {
		primary = []string{"plan", "coding", "ask"}
	}
	for i, id := range primary {
		if id == current {
			return primary[(i+1)%len(primary)]
		}
	}
	return primary[0]
}

func primaryAgentIDs(manifest config.AgentManifest) []string {
	preferred := []string{"plan", "coding", "ask"}
	seen := map[string]struct{}{}
	ids := make([]string, 0, len(manifest.Agents))
	for _, id := range preferred {
		if spec, ok := manifest.AgentByID(id); ok && spec.Enabled && spec.IsPrimaryRole() {
			ids = append(ids, id)
			seen[id] = struct{}{}
		}
	}
	for _, spec := range manifest.Agents {
		if !spec.Enabled || !spec.IsPrimaryRole() {
			continue
		}
		if _, ok := seen[spec.ID]; ok {
			continue
		}
		ids = append(ids, spec.ID)
		seen[spec.ID] = struct{}{}
	}
	return ids
}

func nextMode(mode string) string {
	switch mode {
	case "plan":
		return "coding"
	case "coding":
		return "ask"
	default:
		return "plan"
	}
}

func prevMode(mode string) string {
	switch mode {
	case "plan":
		return "ask"
	case "coding":
		return "plan"
	default:
		return "coding"
	}
}
