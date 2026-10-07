package tui

import (
	"fmt"

	"spettro/internal/agent"
)

// How a tool's calls are worded when they are counted rather than described
// one by one: the header of a group of consecutive calls ("Read 3 files",
// "Running 2 commands…") and the verb in front of a group's descriptors
// ("Edited a.go, b.go").
//
// One table holds that wording for every built-in, so the finished and the
// running forms of a group can no longer drift apart (they used to live in
// four separate switches, and a tool missing from one of them fell back to
// "Using Todo Write 2 time(s)…" while running and "Wrote 2 todo batches"
// once done). The single-call labels (formatToolLabel, formatRunningLabel)
// describe a call by its arguments and stay separate, but fall back to this
// table for a built-in they have no case for. A test keeps the table
// complete against the default manifest and the retired names.

// toolWording is the counted wording of one tool's calls.
type toolWording struct {
	done    string // verb of finished calls: "Read"
	running string // verb of running calls: "Reading"
	// one and many are the noun after a count: "1 file", "3 files". When
	// many is empty the count is a number of repetitions of one instead:
	// "the language server once", "plan mode 3 times".
	one, many string
}

// phrase is count calls in this wording's noun: "1 file", "3 files",
// "plan mode once", "plan mode 3 times".
func (w toolWording) phrase(count int) string {
	if w.many == "" {
		times := "once"
		if count != 1 {
			times = fmt.Sprintf("%d times", count)
		}
		if w.one == "" {
			return times
		}
		return w.one + " " + times
	}
	if count == 1 {
		return "1 " + w.one
	}
	return fmt.Sprintf("%d %s", count, w.many)
}

// single is the noun of one call when no argument describes it: "1 image"
// for a counted noun, the bare noun for a repeated one ("plan mode").
func (w toolWording) single() string {
	if w.many == "" {
		return w.one
	}
	return w.phrase(1)
}

// toolWordings is keyed by tool name: every built-in, plus the retired names
// whose calls read differently from their canonical tool's (a task-get call
// only read the list todo-write now writes). Any other retired name is
// worded as its canonical tool (see wordingFor).
var toolWordings = map[string]toolWording{
	"file-read":          {"Read", "Reading", "file", "files"},
	"file-write":         {"Wrote", "Writing", "file", "files"},
	"file-edit":          {"Edited", "Editing", "file", "files"},
	"rename-symbol":      {"Renamed", "Renaming", "symbol", "symbols"},
	"glob":               {"Matched", "Matching", "pattern", "patterns"},
	"ls":                 {"Listed", "Listing", "directory", "directories"},
	"grep":               {"Grepped", "Grepping", "pattern", "patterns"},
	"repo-search":        {"Searched", "Searching", "query", "queries"},
	"tool-search":        {"Searched", "Searching", "tool query", "tool queries"},
	"web-search":         {"Searched", "Searching", "web query", "web queries"},
	"web-fetch":          {"Fetched", "Fetching", "page", "pages"},
	"download":           {"Downloaded", "Downloading", "file", "files"},
	"bash":               {"Ran", "Running", "command", "commands"},
	"job-output":         {"Read", "Reading", "job output", "job outputs"},
	"job-kill":           {"Stopped", "Stopping", "job", "jobs"},
	"tool-output":        {"Read", "Reading", "spooled output", "spooled outputs"},
	"pty-start":          {"Started", "Starting", "terminal session", "terminal sessions"},
	"pty-write":          {"Typed into", "Typing into", "a terminal", ""},
	"pty-kill":           {"Closed", "Closing", "terminal session", "terminal sessions"},
	"todo-write":         {"Updated", "Updating", "the todo list", ""},
	"task-create":        {"Created", "Creating", "task", "tasks"},
	"task-update":        {"Updated", "Updating", "task", "tasks"},
	"task-get":           {"Read", "Reading", "task", "tasks"},
	"task-list":          {"Listed", "Listing", "tasks", ""},
	"task-delete":        {"Deleted", "Deleting", "task", "tasks"},
	"task-stop":          {"Stopped", "Stopping", "task", "tasks"},
	"goal-complete":      {"Completed", "Completing", "the goal", ""},
	"config":             {"Configured", "Configuring", "settings", ""},
	"save-memory":        {"Saved", "Saving", "memory", "memories"},
	"comment":            {"Commented", "Commenting", "", ""},
	"ask-user":           {"Asked", "Asking", "question", "questions"},
	"enter-plan-mode":    {"Entered", "Entering", "plan mode", ""},
	"exit-plan-mode":     {"Exited", "Exiting", "plan mode", ""},
	"view-image":         {"Viewed", "Viewing", "image", "images"},
	"mcp-list-resources": {"Listed", "Listing", "MCP resources", ""},
	"mcp-read-resource":  {"Read", "Reading", "MCP resource", "MCP resources"},
	"mcp-auth":           {"Updated", "Updating", "MCP auth", ""},
	"enter-worktree":     {"Entered", "Entering", "worktree", "worktrees"},
	"exit-worktree":      {"Exited", "Exiting", "worktree", "worktrees"},
	"send-message":       {"Sent", "Sending", "message", "messages"},
	"agent":              {"Delegated", "Delegating", "task", "tasks"},
	"workflow":           {"Ran", "Running", "workflow", "workflows"},
	"lsp":                {"Queried", "Querying", "the language server", ""},
	"skill":              {"Loaded", "Loading", "skill", "skills"},
	"skill-list":         {"Listed", "Listing", "skills", ""},
	"approval":           {"Decided", "Deciding", "approval", "approvals"},
}

// wordingFor returns the counted wording of a tool's calls: its own entry,
// else its canonical tool's for a retired name. ok is false for a name no
// built-in answers to (a tool of the operator's own, an MCP tool).
func wordingFor(name string) (toolWording, bool) {
	if w, ok := toolWordings[name]; ok {
		return w, true
	}
	w, ok := toolWordings[agent.CanonicalToolName(name)]
	return w, ok
}

// genericWording words the calls of a tool the table does not know, by its
// name: "Used Deploy Preview 2 times", "Using Deploy Preview…".
func genericWording(name string) toolWording {
	return toolWording{done: "Used", running: "Using", one: humanizeToolID(name)}
}

// wording is wordingFor with the generic wording as the fallback.
func wording(name string) toolWording {
	if w, ok := wordingFor(name); ok {
		return w
	}
	return genericWording(name)
}

// toolActionVerb is the verb of finished calls of name ("Read").
func toolActionVerb(name string) string { return wording(name).done }

// runningVerb is the verb of running calls of name ("Reading").
func runningVerb(name string) string { return wording(name).running }

// toolNounCount is count calls of name in its noun ("3 files").
func toolNounCount(name string, count int) string { return wording(name).phrase(count) }
