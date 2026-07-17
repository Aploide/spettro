package tui

import (
	"encoding/json"
	"fmt"
	"image/color"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"spettro/internal/agent"
	"spettro/internal/config"
	"spettro/internal/diff"
)

func stripThinking(content string) (main, thinking string) {
	var sb, tb strings.Builder
	remaining := content
	for {
		start := strings.Index(remaining, "<think>")
		if start == -1 {
			sb.WriteString(remaining)
			break
		}
		sb.WriteString(remaining[:start])
		remaining = remaining[start+len("<think>"):]
		end := strings.Index(remaining, "</think>")
		if end == -1 {
			tb.WriteString(remaining)
			break
		}
		tb.WriteString(remaining[:end])
		remaining = remaining[end+len("</think>"):]
	}
	return strings.TrimSpace(sb.String()), strings.TrimSpace(tb.String())
}

func waitForTool(ch chan agent.ToolTrace) tea.Cmd {
	return func() tea.Msg {
		t, ok := <-ch
		if !ok {
			return nil
		}
		return toolProgressMsg{trace: t}
	}
}

func waitForStream(ch chan agent.StreamChunk) tea.Cmd {
	return func() tea.Msg {
		c, ok := <-ch
		if !ok {
			return nil
		}
		return streamChunkMsg{chunk: c}
	}
}

func waitForUsage(ch chan agent.UsageEvent) tea.Cmd {
	return func() tea.Msg {
		ev, ok := <-ch
		if !ok {
			return nil
		}
		return usageEventMsg{event: ev}
	}
}

func waitForShellApproval(ch chan shellApprovalRequestMsg) tea.Cmd {
	return func() tea.Msg {
		req, ok := <-ch
		if !ok {
			return nil
		}
		return req
	}
}

func waitForAskUser(ch chan askUserRequestMsg) tea.Cmd {
	return func() tea.Msg {
		req, ok := <-ch
		if !ok {
			return nil
		}
		return req
	}
}

func (m Model) renderApprovalPicker(title string, options []string, cursor int, mc color.Color) string {
	var sb strings.Builder
	sb.WriteString(styleMuted.Render("  "+title) + "\n")
	for i, opt := range options {
		if i == cursor {
			sb.WriteString(lipgloss.NewStyle().Foreground(mc).Bold(true).Render("  › " + opt))
		} else {
			sb.WriteString(styleMuted.Render("    " + opt))
		}
		if i < len(options)-1 {
			sb.WriteString("\n")
		}
	}
	return sb.String()
}

func formatToolLabel(name, argsJSON string) string {
	switch name {
	case "file-read":
		var args struct {
			Path string `json:"path"`
		}
		if json.Unmarshal([]byte(argsJSON), &args) == nil && args.Path != "" {
			return "Read " + args.Path
		}
		return "Read file"
	case "file-write":
		var args struct {
			Path string `json:"path"`
		}
		if json.Unmarshal([]byte(argsJSON), &args) == nil && args.Path != "" {
			return "Wrote " + args.Path
		}
		return "Wrote file"
	case "file-edit", "multi-edit":
		var args struct {
			Path string `json:"path"`
		}
		if json.Unmarshal([]byte(argsJSON), &args) == nil && args.Path != "" {
			return "Edited " + args.Path
		}
		return "Edited file"
	case "repo-search":
		var args struct {
			Query string `json:"query"`
		}
		if json.Unmarshal([]byte(argsJSON), &args) == nil && args.Query != "" {
			q := truncateLabel(args.Query, 50)
			return fmt.Sprintf("Searched repo for %q", q)
		}
		return "Searched repository"
	case "tool-search":
		var args struct {
			Query string `json:"query"`
		}
		if json.Unmarshal([]byte(argsJSON), &args) == nil && args.Query != "" {
			q := truncateLabel(args.Query, 50)
			return fmt.Sprintf("Searched tools for %q", q)
		}
		return "Searched tools"
	case "web-search":
		var args struct {
			Query string `json:"query"`
		}
		if json.Unmarshal([]byte(argsJSON), &args) == nil && args.Query != "" {
			q := truncateLabel(args.Query, 50)
			return fmt.Sprintf("Searched web for %q", q)
		}
		return "Searched web"
	case "web-fetch":
		var args struct {
			URL string `json:"url"`
		}
		if json.Unmarshal([]byte(argsJSON), &args) == nil && args.URL != "" {
			u := truncateLabel(args.URL, 60)
			return fmt.Sprintf("Fetched %q", u)
		}
		return "Fetched web page"
	case "download":
		var args struct {
			URL  string `json:"url"`
			Path string `json:"path"`
		}
		if json.Unmarshal([]byte(argsJSON), &args) == nil && args.URL != "" {
			return fmt.Sprintf("Downloaded %q", truncateLabel(args.URL, 60))
		}
		return "Downloaded file"
	case "shell-exec", "bash", "bash-output":
		var args struct {
			Command string `json:"command"`
		}
		if json.Unmarshal([]byte(argsJSON), &args) == nil && args.Command != "" {
			cmd := truncateLabel(args.Command, 60)
			return "Ran $ " + cmd
		}
		return "Ran command"
	case "glob":
		var args struct {
			Pattern string `json:"pattern"`
		}
		if json.Unmarshal([]byte(argsJSON), &args) == nil && args.Pattern != "" {
			p := truncateLabel(args.Pattern, 50)
			return fmt.Sprintf("Matched %q", p)
		}
		return "Matched files"
	case "grep":
		var args struct {
			Pattern string `json:"pattern"`
		}
		if json.Unmarshal([]byte(argsJSON), &args) == nil && args.Pattern != "" {
			p := truncateLabel(args.Pattern, 50)
			return fmt.Sprintf("Grepped %q", p)
		}
		return "Grepped files"
	case "ls":
		var args struct {
			Path string `json:"path"`
		}
		if json.Unmarshal([]byte(argsJSON), &args) == nil && strings.TrimSpace(args.Path) != "" {
			p := truncateLabel(args.Path, 60)
			return fmt.Sprintf("Listed %s", p)
		}
		return "Listed directory"
	case "todo-write":
		var args struct {
			Todos []json.RawMessage `json:"todos"`
		}
		if json.Unmarshal([]byte(argsJSON), &args) == nil && len(args.Todos) > 0 {
			if len(args.Todos) == 1 {
				return "Wrote 1 todo"
			}
			return fmt.Sprintf("Wrote %d todos", len(args.Todos))
		}
		return "Wrote todos"
	case "task-create":
		var args struct {
			ID string `json:"id"`
		}
		if json.Unmarshal([]byte(argsJSON), &args) == nil && strings.TrimSpace(args.ID) != "" {
			return fmt.Sprintf("Created task %s", truncateLabel(args.ID, 40))
		}
		return "Created task"
	case "task-get":
		var args struct {
			ID string `json:"id"`
		}
		if json.Unmarshal([]byte(argsJSON), &args) == nil && strings.TrimSpace(args.ID) != "" {
			return fmt.Sprintf("Read task %s", truncateLabel(args.ID, 40))
		}
		return "Read task"
	case "task-update":
		var args struct {
			ID string `json:"id"`
		}
		if json.Unmarshal([]byte(argsJSON), &args) == nil && strings.TrimSpace(args.ID) != "" {
			return fmt.Sprintf("Updated task %s", truncateLabel(args.ID, 40))
		}
		return "Updated task"
	case "task-list":
		return "Listed tasks"
	case "task-delete":
		var args struct {
			ID string `json:"id"`
		}
		if json.Unmarshal([]byte(argsJSON), &args) == nil && strings.TrimSpace(args.ID) != "" {
			return fmt.Sprintf("Deleted task %s", truncateLabel(args.ID, 40))
		}
		return "Deleted tasks"
	case "ask-user":
		var args struct {
			Question string `json:"question"`
		}
		if json.Unmarshal([]byte(argsJSON), &args) == nil && strings.TrimSpace(args.Question) != "" {
			q := truncateLabel(args.Question, 50)
			return fmt.Sprintf("Asked user %q", q)
		}
		return "Asked user"
	case "enter-plan-mode":
		return "Entered plan mode"
	case "exit-plan-mode":
		return "Exited plan mode"
	case "mcp-list-resources":
		var args struct {
			ServerID string `json:"server_id"`
		}
		if json.Unmarshal([]byte(argsJSON), &args) == nil && strings.TrimSpace(args.ServerID) != "" {
			return fmt.Sprintf("Listed MCP resources for %s", truncateLabel(args.ServerID, 40))
		}
		return "Listed MCP resources"
	case "mcp-read-resource":
		var args struct {
			ResourceID string `json:"resource_id"`
		}
		if json.Unmarshal([]byte(argsJSON), &args) == nil && strings.TrimSpace(args.ResourceID) != "" {
			return fmt.Sprintf("Read MCP resource %s", truncateLabel(args.ResourceID, 40))
		}
		return "Read MCP resource"
	case "mcp-auth":
		var args struct {
			ServerID string `json:"server_id"`
		}
		if json.Unmarshal([]byte(argsJSON), &args) == nil && strings.TrimSpace(args.ServerID) != "" {
			return fmt.Sprintf("Updated MCP auth for %s", truncateLabel(args.ServerID, 40))
		}
		return "Updated MCP auth"
	case "enter-worktree":
		var args struct {
			Path string `json:"path"`
		}
		if json.Unmarshal([]byte(argsJSON), &args) == nil && strings.TrimSpace(args.Path) != "" {
			return fmt.Sprintf("Entered worktree %s", truncateLabel(args.Path, 50))
		}
		return "Entered worktree"
	case "exit-worktree":
		var args struct {
			Path string `json:"path"`
		}
		if json.Unmarshal([]byte(argsJSON), &args) == nil && strings.TrimSpace(args.Path) != "" {
			return fmt.Sprintf("Exited worktree %s", truncateLabel(args.Path, 50))
		}
		return "Exited worktree"
	case "send-message":
		var args struct {
			Target string `json:"target"`
		}
		if json.Unmarshal([]byte(argsJSON), &args) == nil && strings.TrimSpace(args.Target) != "" {
			return fmt.Sprintf("Sent message to %s", truncateLabel(args.Target, 40))
		}
		return "Sent message"
	case "agent":
		var args struct {
			Agent  string `json:"agent"`
			Target string `json:"target"`
			ID     string `json:"id"`
			Task   string `json:"task"`
		}
		if json.Unmarshal([]byte(argsJSON), &args) == nil {
			label := args.Agent
			if label == "" {
				label = args.Target
			}
			if label == "" {
				label = args.ID
			}
			if label == "" {
				label = "sub-agent"
			}
			if args.Task != "" {
				return fmt.Sprintf("Delegated to %s for %s", label, truncateLabel(args.Task, 80))
			}
			return fmt.Sprintf("Delegated to %s", label)
		}
	}
	return humanizeToolID(name)
}

func formatRunningLabel(name, argsJSON string) string {
	switch name {
	case "file-read":
		var args struct {
			Path string `json:"path"`
		}
		if json.Unmarshal([]byte(argsJSON), &args) == nil && args.Path != "" {
			return "Reading " + args.Path + "…"
		}
		return "Reading…"
	case "file-write":
		var args struct {
			Path string `json:"path"`
		}
		if json.Unmarshal([]byte(argsJSON), &args) == nil && args.Path != "" {
			return "Writing " + args.Path + "…"
		}
		return "Writing…"
	case "file-edit", "multi-edit":
		var args struct {
			Path string `json:"path"`
		}
		if json.Unmarshal([]byte(argsJSON), &args) == nil && args.Path != "" {
			return "Editing " + args.Path + "…"
		}
		return "Editing…"
	case "repo-search":
		var args struct {
			Query string `json:"query"`
		}
		if json.Unmarshal([]byte(argsJSON), &args) == nil && args.Query != "" {
			q := truncateLabel(args.Query, 50)
			return fmt.Sprintf("Searching repo for %q…", q)
		}
		return "Searching repository…"
	case "tool-search":
		var args struct {
			Query string `json:"query"`
		}
		if json.Unmarshal([]byte(argsJSON), &args) == nil && args.Query != "" {
			q := truncateLabel(args.Query, 50)
			return fmt.Sprintf("Searching tools for %q…", q)
		}
		return "Searching tools…"
	case "web-search":
		var args struct {
			Query string `json:"query"`
		}
		if json.Unmarshal([]byte(argsJSON), &args) == nil && args.Query != "" {
			q := truncateLabel(args.Query, 50)
			return fmt.Sprintf("Searching web for %q…", q)
		}
		return "Searching web…"
	case "web-fetch":
		var args struct {
			URL string `json:"url"`
		}
		if json.Unmarshal([]byte(argsJSON), &args) == nil && args.URL != "" {
			u := truncateLabel(args.URL, 60)
			return fmt.Sprintf("Fetching %q…", u)
		}
		return "Fetching web page…"
	case "download":
		var args struct {
			URL  string `json:"url"`
			Path string `json:"path"`
		}
		if json.Unmarshal([]byte(argsJSON), &args) == nil && args.URL != "" {
			return fmt.Sprintf("Downloading %q…", truncateLabel(args.URL, 60))
		}
		return "Downloading file…"
	case "shell-exec", "bash", "bash-output":
		var args struct {
			Command string `json:"command"`
		}
		if json.Unmarshal([]byte(argsJSON), &args) == nil && args.Command != "" {
			cmd := truncateLabel(args.Command, 60)
			return "Running $ " + cmd + "…"
		}
		return "Running…"
	case "glob":
		var args struct {
			Pattern string `json:"pattern"`
		}
		if json.Unmarshal([]byte(argsJSON), &args) == nil && args.Pattern != "" {
			p := truncateLabel(args.Pattern, 50)
			return fmt.Sprintf("Matching %q…", p)
		}
		return "Matching files…"
	case "grep":
		var args struct {
			Pattern string `json:"pattern"`
		}
		if json.Unmarshal([]byte(argsJSON), &args) == nil && args.Pattern != "" {
			p := truncateLabel(args.Pattern, 50)
			return fmt.Sprintf("Grepping %q…", p)
		}
		return "Grepping…"
	case "ls":
		var args struct {
			Path string `json:"path"`
		}
		if json.Unmarshal([]byte(argsJSON), &args) == nil && strings.TrimSpace(args.Path) != "" {
			return fmt.Sprintf("Listing %s…", truncateLabel(args.Path, 60))
		}
		return "Listing directory…"
	case "todo-write":
		return "Writing todos…"
	case "task-create":
		return "Creating task…"
	case "task-get":
		return "Reading task…"
	case "task-update":
		return "Updating task…"
	case "task-list":
		return "Listing tasks…"
	case "task-delete":
		return "Deleting task…"
	case "ask-user":
		return "Asking user…"
	case "enter-plan-mode":
		return "Entering plan mode…"
	case "exit-plan-mode":
		return "Exiting plan mode…"
	case "mcp-list-resources":
		return "Listing MCP resources…"
	case "mcp-read-resource":
		return "Reading MCP resource…"
	case "mcp-auth":
		return "Updating MCP auth…"
	case "enter-worktree":
		return "Entering worktree…"
	case "exit-worktree":
		return "Exiting worktree…"
	case "send-message":
		return "Sending message…"
	case "agent":
		return "Delegating to sub-agent…"
	}
	return "Using " + humanizeToolID(name) + "…"
}

func extractToolPath(name, argsJSON string) string {
	switch name {
	case "file-read", "file-write":
		var args struct {
			Path string `json:"path"`
		}
		if json.Unmarshal([]byte(argsJSON), &args) == nil {
			return args.Path
		}
	}
	return ""
}

func toolActionVerb(name string) string {
	switch name {
	case "file-read":
		return "Read"
	case "file-write":
		return "Wrote"
	case "file-edit", "multi-edit":
		return "Edited"
	case "repo-search", "tool-search", "web-search":
		return "Searched"
	case "web-fetch":
		return "Fetched"
	case "download":
		return "Downloaded"
	case "shell-exec", "bash", "bash-output":
		return "Ran"
	case "glob":
		return "Matched"
	case "grep":
		return "Grepped"
	case "ls":
		return "Listed"
	case "todo-write":
		return "Wrote"
	case "task-create":
		return "Created"
	case "task-get":
		return "Read"
	case "task-update":
		return "Updated"
	case "task-list":
		return "Listed"
	case "task-delete":
		return "Deleted"
	case "ask-user":
		return "Asked"
	case "enter-plan-mode":
		return "Entered"
	case "exit-plan-mode":
		return "Exited"
	case "mcp-list-resources":
		return "Listed"
	case "mcp-read-resource":
		return "Read"
	case "mcp-auth":
		return "Updated"
	case "enter-worktree":
		return "Entered"
	case "exit-worktree":
		return "Exited"
	case "send-message":
		return "Sent"
	case "agent":
		return "Delegated"
	}
	return "Used"
}

func toolNounCount(name string, count int) string {
	switch name {
	case "file-read", "file-write", "file-edit", "multi-edit":
		if count == 1 {
			return "1 file"
		}
		return fmt.Sprintf("%d files", count)
	case "repo-search", "tool-search", "web-search", "grep":
		if count == 1 {
			return "1 query"
		}
		return fmt.Sprintf("%d queries", count)
	case "shell-exec", "bash", "bash-output":
		if count == 1 {
			return "1 command"
		}
		return fmt.Sprintf("%d commands", count)
	case "glob":
		if count == 1 {
			return "1 pattern"
		}
		return fmt.Sprintf("%d patterns", count)
	case "web-fetch":
		if count == 1 {
			return "1 page"
		}
		return fmt.Sprintf("%d pages", count)
	case "download":
		if count == 1 {
			return "1 download"
		}
		return fmt.Sprintf("%d downloads", count)
	case "ls":
		if count == 1 {
			return "1 listing"
		}
		return fmt.Sprintf("%d listings", count)
	case "todo-write":
		if count == 1 {
			return "1 todo batch"
		}
		return fmt.Sprintf("%d todo batches", count)
	case "task-create", "task-get", "task-update", "task-list", "task-delete":
		if count == 1 {
			return "1 task"
		}
		return fmt.Sprintf("%d tasks", count)
	case "ask-user":
		if count == 1 {
			return "1 prompt"
		}
		return fmt.Sprintf("%d prompts", count)
	case "enter-plan-mode", "exit-plan-mode":
		if count == 1 {
			return "1 mode change"
		}
		return fmt.Sprintf("%d mode changes", count)
	case "mcp-list-resources", "mcp-read-resource":
		if count == 1 {
			return "1 MCP resource"
		}
		return fmt.Sprintf("%d MCP resources", count)
	case "mcp-auth":
		if count == 1 {
			return "1 MCP auth update"
		}
		return fmt.Sprintf("%d MCP auth updates", count)
	case "enter-worktree", "exit-worktree":
		if count == 1 {
			return "1 worktree"
		}
		return fmt.Sprintf("%d worktrees", count)
	case "send-message":
		if count == 1 {
			return "1 message"
		}
		return fmt.Sprintf("%d messages", count)
	case "agent":
		if count == 1 {
			return "1 delegation"
		}
		return fmt.Sprintf("%d delegations", count)
	}
	if count == 1 {
		return "1 call"
	}
	return fmt.Sprintf("%d calls", count)
}

func summarizeToolArgs(name, argsJSON string) string {
	switch name {
	case "file-read":
		var args struct {
			Path      string `json:"path"`
			StartLine int    `json:"start_line"`
			EndLine   int    `json:"end_line"`
		}
		if json.Unmarshal([]byte(argsJSON), &args) == nil {
			if args.Path == "" {
				return "Reads a file from the workspace."
			}
			if args.StartLine > 0 || args.EndLine > 0 {
				return fmt.Sprintf("Reads %s (lines %d-%d).", args.Path, args.StartLine, args.EndLine)
			}
			return fmt.Sprintf("Reads %s.", args.Path)
		}
	case "file-write":
		var args struct {
			Path string `json:"path"`
		}
		if json.Unmarshal([]byte(argsJSON), &args) == nil && args.Path != "" {
			return fmt.Sprintf("Writes %s.", args.Path)
		}
	case "repo-search":
		var args struct {
			Query string `json:"query"`
		}
		if json.Unmarshal([]byte(argsJSON), &args) == nil {
			if args.Query == "" {
				return "Scans the repository structure."
			}
			return fmt.Sprintf("Searches the repository for %q.", truncateLabel(args.Query, 80))
		}
	case "shell-exec", "bash":
		var args struct {
			Command string `json:"command"`
		}
		if json.Unmarshal([]byte(argsJSON), &args) == nil && args.Command != "" {
			return fmt.Sprintf("Runs `%s`.", truncateLabel(args.Command, 120))
		}
	case "glob":
		var args struct {
			Pattern string `json:"pattern"`
		}
		if json.Unmarshal([]byte(argsJSON), &args) == nil && args.Pattern != "" {
			return fmt.Sprintf("Finds files matching %q.", truncateLabel(args.Pattern, 100))
		}
	case "grep":
		var args struct {
			Pattern string `json:"pattern"`
			Path    string `json:"path"`
		}
		if json.Unmarshal([]byte(argsJSON), &args) == nil && args.Pattern != "" {
			if args.Path != "" {
				return fmt.Sprintf("Searches %s for %q.", args.Path, truncateLabel(args.Pattern, 100))
			}
			return fmt.Sprintf("Searches file contents for %q.", truncateLabel(args.Pattern, 100))
		}
	case "agent":
		var args struct {
			Agent  string `json:"agent"`
			Target string `json:"target"`
			ID     string `json:"id"`
			Task   string `json:"task"`
		}
		if json.Unmarshal([]byte(argsJSON), &args) == nil {
			label := args.Agent
			if label == "" {
				label = args.Target
			}
			if label == "" {
				label = args.ID
			}
			if label == "" {
				label = "sub-agent"
			}
			if args.Task != "" {
				return fmt.Sprintf("Delegates to %s for %s.", label, truncateLabel(args.Task, 100))
			}
			return fmt.Sprintf("Delegates to %s.", label)
		}
	}
	if strings.TrimSpace(argsJSON) == "" {
		return ""
	}
	return truncateLabel(argsJSON, 120)
}

func sanitizeToolOutput(output string, maxLines int) string {
	output = stripToolCallLines(output)
	output = strings.TrimSpace(output)
	if output == "" {
		return ""
	}
	if pretty, ok := formatSubagentEnvelope(output); ok {
		return trimToolOutput(pretty, maxLines)
	}
	return trimToolOutput(output, maxLines)
}

func formatSubagentEnvelope(output string) (string, bool) {
	var payload struct {
		Agent          string `json:"agent"`
		Status         string `json:"status"`
		Summary        string `json:"summary"`
		ToolTraceCount int    `json:"tool_trace_count"`
		TokensUsed     int    `json:"tokens_used"`
	}
	if err := json.Unmarshal([]byte(output), &payload); err != nil {
		return "", false
	}
	if strings.TrimSpace(payload.Agent) == "" && strings.TrimSpace(payload.Summary) == "" {
		return "", false
	}
	lines := []string{}
	if payload.Agent != "" {
		lines = append(lines, fmt.Sprintf("sub-agent: %s", payload.Agent))
	}
	if payload.Status != "" {
		lines = append(lines, fmt.Sprintf("status: %s", payload.Status))
	}
	if payload.ToolTraceCount > 0 || payload.TokensUsed > 0 {
		lines = append(lines, fmt.Sprintf("tools: %d  tokens: %d", payload.ToolTraceCount, payload.TokensUsed))
	}
	if strings.TrimSpace(payload.Summary) != "" {
		lines = append(lines, "summary:")
		lines = append(lines, payload.Summary)
	}
	return strings.Join(lines, "\n"), true
}

func stripToolCallLines(content string) string {
	if strings.TrimSpace(content) == "" {
		return ""
	}
	lines := strings.Split(content, "\n")
	filtered := lines[:0]
	for _, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "TOOL_CALL") {
			continue
		}
		filtered = append(filtered, line)
	}
	return strings.TrimSpace(strings.Join(filtered, "\n"))
}

func renderToolGroups(tools []ToolItem, showTools bool, mc color.Color) string {
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
				if out := trimToolOutput(item.Output, 20); out != "" {
					for _, ol := range strings.Split(out, "\n") {
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
						if out := trimToolOutput(gt.Output, 8); out != "" {
							for _, ol := range strings.Split(out, "\n") {
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
	maxShown := len(group)
	if maxShown > 3 {
		maxShown = 3
	}
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

func trimToolOutput(output string, maxLines int) string {
	output = strings.TrimSpace(output)
	if output == "" {
		return ""
	}
	lines := strings.Split(output, "\n")
	if len(lines) <= maxLines {
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
