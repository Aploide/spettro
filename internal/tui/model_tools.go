package tui

import (
	"encoding/json"
	"fmt"
	"image/color"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"spettro/internal/agent"
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

// pickerOption is one row of an approval picker. The annotations are drawn
// independently of the cursor, so a marker the agent attached to a row (its
// recommended answer) stays readable after the user arrows away from it.
type pickerOption struct {
	Label string
	// Badge is a muted suffix rendered after the label on every row state.
	Badge string
}

// windowPickerRows keeps the cursor's row visible within maxLines of terminal
// height, growing the window outwards from the cursor. When rows are dropped it
// reserves one line for the caller's "… N more" marker and returns how many are
// hidden; the caller must render that marker for the count to add up.
func windowPickerRows(rows []pickerOption, cursor, maxLines int) (visible []pickerOption, newCursor, hidden int) {
	if cursor < 0 || cursor >= len(rows) {
		cursor = 0
	}
	if len(rows) == 0 || len(rows) <= max(maxLines, 1) {
		return rows, cursor, 0
	}
	// One line goes to the caller's marker, so the rows themselves get the
	// rest. A single-line budget cannot show both; the caller reserves for this
	// and gets one row over budget rather than an empty list if it does not.
	budget := max(maxLines-1, 1)

	start, end := cursor, cursor+1
	for end-start < budget {
		grew := false
		if end < len(rows) {
			end++
			grew = true
		}
		if start > 0 && end-start < budget {
			start--
			grew = true
		}
		if !grew {
			break
		}
	}

	visible = rows[start:end]
	return visible, cursor - start, len(rows) - len(visible)
}

func (m Model) renderApprovalPicker(title string, options []string, cursor int, mc color.Color) string {
	rows := make([]pickerOption, 0, len(options))
	for _, opt := range options {
		rows = append(rows, pickerOption{Label: opt})
	}
	return m.renderAnnotatedPicker(title, rows, cursor, mc)
}

func (m Model) renderAnnotatedPicker(title string, options []pickerOption, cursor int, mc color.Color) string {
	var sb strings.Builder
	sb.WriteString(styleMuted.Render("  " + title))
	sb.WriteString("\n")
	for i, opt := range options {
		if i == cursor {
			sb.WriteString(lipgloss.NewStyle().Foreground(mc).Bold(true).Render("  › " + opt.Label))
		} else {
			sb.WriteString(styleMuted.Render("    " + opt.Label))
		}
		if opt.Badge != "" {
			sb.WriteString(styleMuted.Render("  " + opt.Badge))
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
	case "pty-start":
		var args struct {
			Command string `json:"command"`
		}
		if json.Unmarshal([]byte(argsJSON), &args) == nil && args.Command != "" {
			return "Started terminal $ " + truncateLabel(args.Command, 60)
		}
		return "Started terminal session"
	case "pty-write":
		var args struct {
			ID    string `json:"id"`
			Input string `json:"input"`
		}
		if json.Unmarshal([]byte(argsJSON), &args) == nil && args.ID != "" {
			if in := strings.TrimSpace(args.Input); in != "" {
				return fmt.Sprintf("Typed %q into %s", truncateLabel(in, 40), args.ID)
			}
			return "Polled terminal " + args.ID
		}
		return "Typed into terminal"
	case "pty-kill":
		var args struct {
			ID string `json:"id"`
		}
		if json.Unmarshal([]byte(argsJSON), &args) == nil && args.ID != "" {
			return "Closed terminal " + args.ID
		}
		return "Closed terminal session"
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
		// The form shape (questions[]) and the legacy flat question both reach
		// this label; a multi-question form is summarised by its first question.
		var args struct {
			Question  string `json:"question"`
			Questions []struct {
				Question string `json:"question"`
			} `json:"questions"`
		}
		if json.Unmarshal([]byte(argsJSON), &args) != nil {
			return "Asked user"
		}
		first := strings.TrimSpace(args.Question)
		if len(args.Questions) > 0 {
			first = strings.TrimSpace(args.Questions[0].Question)
		}
		if first == "" {
			return "Asked user"
		}
		if n := len(args.Questions); n > 1 {
			return fmt.Sprintf("Asked user %q (+%d more)", truncateLabel(first, 40), n-1)
		}
		return fmt.Sprintf("Asked user %q", truncateLabel(first, 50))
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
	case "pty-start":
		var args struct {
			Command string `json:"command"`
		}
		if json.Unmarshal([]byte(argsJSON), &args) == nil && args.Command != "" {
			return "Starting terminal $ " + truncateLabel(args.Command, 60) + "…"
		}
		return "Starting terminal session…"
	case "pty-write":
		var args struct {
			ID    string `json:"id"`
			Input string `json:"input"`
		}
		if json.Unmarshal([]byte(argsJSON), &args) == nil && args.ID != "" {
			if in := strings.TrimSpace(args.Input); in != "" {
				return fmt.Sprintf("Typing %q into %s…", truncateLabel(in, 40), args.ID)
			}
			return "Waiting on terminal " + args.ID + "…"
		}
		return "Typing into terminal…"
	case "pty-kill":
		var args struct {
			ID string `json:"id"`
		}
		if json.Unmarshal([]byte(argsJSON), &args) == nil && args.ID != "" {
			return "Closing terminal " + args.ID + "…"
		}
		return "Closing terminal session…"
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
	case "pty-start":
		return "Started"
	case "pty-write":
		return "Drove"
	case "pty-kill":
		return "Closed"
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
	case "pty-start", "pty-write", "pty-kill":
		if count == 1 {
			return "1 terminal session"
		}
		return fmt.Sprintf("%d terminal sessions", count)
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
