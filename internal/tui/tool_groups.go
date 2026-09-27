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
	"spettro/internal/pty"
	"spettro/internal/termtext"
	"spettro/internal/theme"
)

// Tool output line caps: a lone tool call shows more of its output than each
// member of a group of same-named calls. ctrl+g (fullOutput) lifts both.
const (
	toolOutputCapSingle = 20
	toolOutputCapGroup  = 8
)

// toolOutputIndent is the gutter in front of every tool output row, so the
// output reads as belonging to the "●" header above it.
const toolOutputIndent = "       "

// toolDetailIndent prefixes the per-call rows of an expanded tool group and
// the path row of a single file tool.
const toolDetailIndent = "    ⎿  "

// renderToolGroups renders an assistant turn's tool calls as the transcript
// shows them: one "●" header row per call, or per run of consecutive calls of
// the same tool, followed (when showTools is on) by paths, diffs and output.
//
// width is the number of cells the block may occupy. Every row is fitted to
// it here, because the viewport that displays the transcript silently cuts
// anything wider: a 50k-character heredoc or a 10k-character minified line
// would otherwise lose its tail with no sign anything was hidden. Labels are
// folded onto one line and cut with "…"; output rows are sanitized (escape
// sequences, tabs, carriage returns) and either cut with "…" or, in the
// ctrl+g full-output view, wrapped so nothing is lost.
//
// userTool reports whether a name belongs to a tool of the operator's own
// (Model.isUserTool; nil means none does). Such a call gets the generic
// label, and none of the extras a built-in of the same name would get (a
// path row, a live terminal tail): see tool_labels.go.
func renderToolGroups(tools []ToolItem, width int, showTools, fullOutput bool, mc color.Color, userTool func(name string) bool) string {
	if len(tools) == 0 {
		return ""
	}
	width = max(width, 20)
	singleCap, groupCap := toolOutputCapSingle, toolOutputCapGroup
	if fullOutput {
		singleCap, groupCap = 0, 0 // 0 = unlimited, the scrollback handles the length
	}
	bullet := lipgloss.NewStyle().Foreground(mc).Bold(true).Render("  ●")
	bulletW := lipgloss.Width(bullet) + 1 // the bullet and the space after it
	errStyle := lipgloss.NewStyle().Foreground(theme.Current().Error)
	outputStyle := lipgloss.NewStyle().Foreground(theme.Current().TextFaint).Italic(true)
	var lines []string

	// header fits a label into the row after the bullet.
	header := func(label string, style lipgloss.Style) string {
		return bullet + " " + style.Render(termtext.Fit(termtext.SingleLine(label), width-bulletW))
	}
	// detail fits an indented sub-row (a path or a group member's label).
	detail := func(text string) string {
		return styleMuted.Render(termtext.Fit(toolDetailIndent+termtext.SingleLine(text), width))
	}
	// output appends styled output rows.
	output := func(rows []string) {
		for _, row := range rows {
			lines = append(lines, outputStyle.Render(row))
		}
	}

	i := 0
	for i < len(tools) {
		j := i
		for j < len(tools) && tools[j].Name == tools[i].Name {
			j++
		}
		group := tools[i:j]
		name := group[0].Name
		ownTool := userTool != nil && userTool(name)
		// label is the one-row label of one call in this run.
		label := func(item ToolItem) string {
			running := item.Status == "running"
			switch {
			case ownTool:
				return userToolLabel(name, running)
			case running:
				return formatRunningLabel(name, item.Args)
			default:
				return formatToolLabel(name, item.Args)
			}
		}
		// path is the file path a built-in file tool shows under its label.
		path := func(item ToolItem) string {
			if ownTool {
				return ""
			}
			return extractToolPath(name, item.Args)
		}
		// liveTail is the live terminal tail of a running pty tool.
		liveTail := func(item ToolItem) []string {
			if ownTool {
				return nil
			}
			return liveTailRows(renderPtyLiveTail(name, item.Args, fullOutput), width)
		}

		if len(group) == 1 {
			item := group[0]
			switch item.Status {
			case "running":
				lines = append(lines, header(label(item), styleMuted))
				output(liveTail(item))
			case "error":
				lines = append(lines, header(label(item), errStyle))
			default:
				lines = append(lines, header(label(item), styleMuted))
			}
			if showTools {
				if p := path(item); p != "" {
					lines = append(lines, detail(p+toolStatusIcon(item.Status)))
				}
			}
			if item.Diff != "" && item.Status != "running" {
				if block := renderDiffBlock(item.Diff, width, showTools); block != "" {
					lines = append(lines, block)
				}
			} else if showTools && item.Status != "running" {
				output(toolOutputRows(item.Output, singleCap, width, fullOutput))
			}
		} else {
			groupLabel := formatToolGroupLabel(name, group)
			if ownTool {
				groupLabel = userToolGroupLabel(name, len(group), hasRunningTool(group))
			}
			if !showTools {
				groupLabel += "  (ctrl+o to expand)"
			}
			lines = append(lines, header(groupLabel, styleMuted))
			if showTools {
				for _, gt := range group {
					// Arguments can be a whole file; parse them once per row.
					if p := path(gt); p != "" {
						lines = append(lines, detail(p+toolStatusIcon(gt.Status)))
					} else {
						lines = append(lines, detail(label(gt)))
					}
					if gt.Status == "running" {
						output(liveTail(gt))
					} else {
						output(toolOutputRows(gt.Output, groupCap, width, fullOutput))
					}
				}
			}
		}

		i = j
	}
	return strings.Join(lines, "\n")
}

// toolStatusIcon is the suffix of a file tool's path row: a check once it
// succeeded, a cross when it failed, nothing while it runs.
func toolStatusIcon(status string) string {
	switch status {
	case "running":
		return ""
	case "error":
		return " ✗"
	default:
		return " ✓"
	}
}

// toolOutputRows turns a tool's raw output into indented display rows no
// wider than width cells, keeping at most maxLines source lines (0 = all).
//
// Tool output is untrusted text: every line is sanitized first (see
// termtext.SanitizeLine). A line still wider than the row is wrapped when
// wrap is set (the ctrl+g full-output view, which promises everything) and
// cut with "…" otherwise. Whatever was left out is owned up to in one footer
// row, together with the key that brings it back, so a capped view never
// reads as the whole output.
func toolOutputRows(outputText string, maxLines, width int, wrap bool) []string {
	outputText = strings.TrimSpace(outputText)
	if outputText == "" {
		return nil
	}
	textW := max(width-len(toolOutputIndent), 8)
	source := strings.Split(outputText, "\n")
	hidden := 0
	if maxLines > 0 && len(source) > maxLines {
		hidden = len(source) - maxLines
		source = source[:maxLines]
	}
	rows := make([]string, 0, len(source)+1)
	cut := false
	for _, line := range source {
		line = termtext.SanitizeLine(line)
		if wrap {
			for _, part := range termtext.Wrap(line, textW) {
				rows = append(rows, toolOutputIndent+part)
			}
			continue
		}
		if fitted := termtext.Fit(line, textW); fitted != line {
			line, cut = fitted, true
		}
		rows = append(rows, toolOutputIndent+line)
	}
	var notes []string
	if hidden > 0 {
		notes = append(notes, fmt.Sprintf("… %d more lines", hidden))
	}
	if cut {
		notes = append(notes, "long lines cut")
	}
	if len(notes) > 0 {
		footer := strings.Join(notes, " · ") + " · ctrl+g for full output"
		rows = append(rows, termtext.Fit(toolOutputIndent+footer, width))
	}
	return rows
}

// liveTailRows fits the live scrollback tail of a running pty tool to the
// row width. A terminal session's screen is full of escape sequences and
// carriage returns, so each line is sanitized before it is measured.
func liveTailRows(tail []string, width int) []string {
	if len(tail) == 0 {
		return nil
	}
	textW := max(width-len(toolOutputIndent), 8)
	rows := make([]string, 0, len(tail))
	for _, line := range tail {
		rows = append(rows, toolOutputIndent+termtext.Fit(termtext.SanitizeLine(line), textW))
	}
	return rows
}

// renderPtyLiveTail returns the last few settled scrollback lines of the pty
// session a running pty tool call is driving, so the user watches the
// terminal move while the model waits on it (e.g. a wait_for poll). The
// ctrl+g full-output toggle lifts the line cap to the whole scrollback.
func renderPtyLiveTail(name, argsJSON string, fullOutput bool) []string {
	var sess *pty.Session
	switch name {
	case "pty-write", "pty-kill":
		var args struct {
			ID string `json:"id"`
		}
		if json.Unmarshal([]byte(argsJSON), &args) == nil && strings.TrimSpace(args.ID) != "" {
			sess, _ = pty.Default().Get(args.ID)
		}
	case "pty-start":
		// The session ID does not exist until the tool returns; tail the
		// newest session, which is the one this call just spawned.
		if list := pty.Default().List(); len(list) > 0 {
			sess = list[len(list)-1]
		}
	default:
		return nil
	}
	if sess == nil {
		return nil
	}
	n := 6
	if fullOutput {
		n = 0
	}
	tail := sess.Tail(n)
	for len(tail) > 0 && strings.TrimSpace(tail[len(tail)-1]) == "" {
		tail = tail[:len(tail)-1]
	}
	return tail
}

func hasRunningTool(items []ToolItem) bool {
	for _, item := range items {
		if item.Status == "running" {
			return true
		}
	}
	return false
}

// formatRunningToolGroupLabel is the header of a group of consecutive calls
// of one tool while one of them still runs: the calls' descriptors when
// their arguments give some ("Editing a.go, b.go…"), else the count in the
// tool's wording ("Running 2 commands…").
func formatRunningToolGroupLabel(name string, group []ToolItem) string {
	if desc := formatDetailedGroupLabel(name, true, group); desc != "" {
		return desc
	}
	return runningVerb(name) + " " + toolNounCount(name, len(group)) + "…"
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
	case "file-read", "file-write", "file-edit", "multi-edit", "view-image", "enter-worktree", "exit-worktree", "ls":
		if p := strings.TrimSpace(filePathArg(argsJSON)); p != "" {
			return termtext.FitLeft(termtext.SingleLine(p), 36)
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
	case "shell-exec", "bash", "bash-output", "pty-start":
		var args struct {
			Command string `json:"command"`
		}
		if json.Unmarshal([]byte(argsJSON), &args) == nil && strings.TrimSpace(args.Command) != "" {
			return "$ " + labelCommand(args.Command, 36)
		}
	case "pty-write", "pty-kill":
		var args struct {
			ID string `json:"id"`
		}
		if json.Unmarshal([]byte(argsJSON), &args) == nil && strings.TrimSpace(args.ID) != "" {
			return args.ID
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

// formatApprovalCommandLabel is the text of the approval dialog's summary
// row for an approval request's Command: "$ <command>" for a command, or a
// sentence naming the target of a network call ("network <tool> <target>").
//
// The target is never shortened here. The dialog cuts the row to the
// terminal and, when anything is cut, shows the whole label in its preview
// (buildApprovalPreview): the end of a URL (the real domain after a long
// host name, a query string carrying data out) is what the user has to see
// before allowing the call.
func formatApprovalCommandLabel(command string) string {
	command = trimShellBlanks(command)
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
			return fmt.Sprintf("Searching web for %q", target)
		case "web-fetch":
			return fmt.Sprintf("Fetching %s", target)
		case "download":
			return fmt.Sprintf("Downloading %s", target)
		case "mcp-list-resources":
			return fmt.Sprintf("Listing MCP resources for %s", target)
		case "mcp-read-resource":
			return fmt.Sprintf("Reading MCP resource %s", target)
		case "mcp-auth":
			return fmt.Sprintf("Updating MCP auth for %s", target)
		default:
			return fmt.Sprintf("Using network tool %s on %s", humanizeToolID(toolID), target)
		}
	}
	return "$ " + command
}

// renderDiffBlock renders the diff a file tool produced, under its "●" row.
// Collapsed it shows the first 20 lines; expanded (ctrl+o) all of them.
//
// Every row is cut to width. The diff stays in the unified layout even on a
// wide terminal: a transcript block switching to two columns at some width
// would make the same edit look different after a resize, so the width
// handed to diff.Render is kept below diff.SideBySideMinWidth.
func renderDiffBlock(diffText string, width int, expanded bool) string {
	maxLines := 20
	if expanded {
		maxLines = 0
	}
	return diff.Render(diffText, diff.Options{
		Width:      min(max(width, 20), diff.SideBySideMinWidth-1),
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
