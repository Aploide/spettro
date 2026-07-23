package agent

import (
	"encoding/json"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"spettro/internal/provider"
	"spettro/internal/skills"
)

// toolOutputHistoryLimit returns the default character cap for a tool's output
// in model history. These defaults intentionally match the source caps in
// execute() so the model always sees what it just read.
func toolOutputHistoryLimit(name string) int {
	switch name {
	case "file-read":
		return 40000
	case "repo-search", "grep", "glob", "ls", "diagnostics", "references", "hover":
		return 16000
	case "shell-exec", "bash", "bash-output", "job-output", "tool-output", "pty-start", "pty-write":
		return 8000
	case "web-fetch":
		return webFetchDefaultBudget
	case "agent":
		return 8000
	case "ultra":
		return 32000
	default:
		return 2000
	}
}

func summarizeLoopToolArgs(name, args string) string {
	switch name {
	case "file-read", "file-write":
		var payload struct {
			Path string `json:"path"`
		}
		if json.Unmarshal([]byte(args), &payload) == nil && payload.Path != "" {
			return "path=" + payload.Path
		}
	case "repo-search":
		var payload struct {
			Query string `json:"query"`
		}
		if json.Unmarshal([]byte(args), &payload) == nil && payload.Query != "" {
			return "query=" + truncate(payload.Query, 120)
		}
	case "shell-exec", "bash":
		var payload struct {
			Command string `json:"command"`
		}
		if json.Unmarshal([]byte(args), &payload) == nil && payload.Command != "" {
			return "command=" + truncate(payload.Command, 120)
		}
	case "glob":
		var payload struct {
			Pattern string `json:"pattern"`
		}
		if json.Unmarshal([]byte(args), &payload) == nil && payload.Pattern != "" {
			return "pattern=" + truncate(payload.Pattern, 120)
		}
	case "grep":
		var payload struct {
			Pattern string `json:"pattern"`
			Path    string `json:"path"`
		}
		if json.Unmarshal([]byte(args), &payload) == nil {
			if payload.Path != "" {
				return "path=" + payload.Path + " pattern=" + truncate(payload.Pattern, 120)
			}
			if payload.Pattern != "" {
				return "pattern=" + truncate(payload.Pattern, 120)
			}
		}
	case "view-image":
		var payload struct {
			Path string `json:"path"`
		}
		if json.Unmarshal([]byte(args), &payload) == nil && payload.Path != "" {
			return "path=" + truncate(payload.Path, 120)
		}
	case "grok-image", "grok-video":
		var payload struct {
			Prompt string `json:"prompt"`
			Path   string `json:"path"`
		}
		if json.Unmarshal([]byte(args), &payload) == nil {
			parts := []string{}
			if payload.Prompt != "" {
				parts = append(parts, "prompt="+truncate(payload.Prompt, 80))
			}
			if payload.Path != "" {
				parts = append(parts, "path="+payload.Path)
			}
			if len(parts) > 0 {
				return strings.Join(parts, " ")
			}
		}
	}
	return truncate(strings.TrimSpace(args), 120)
}

// buildSystemString returns the system-role content for the request. The model
// always receives tool schemas via the API and uses structured tool calls, so
// no text protocol is rendered here.
//
// The result MUST be byte-for-byte identical for every step of a run (and every
// turn of a session): the system prompt is the first segment of the provider
// cache prefix, so any variation invalidates prompt caching for the entire
// request. Never embed step counters, timestamps, or other per-call state here.
func buildSystemString(cfg toolLoopConfig) string {
	base := strings.TrimSpace(cfg.SystemPrompt)
	if base == "" {
		base = "You are an assistant."
	}
	if catalog := skills.CatalogPrompt(cfg.SkillsCatalog); catalog != "" {
		base = base + catalog
	}
	if slices.Contains(cfg.AllowedTools, "comment") {
		base += "\n- Use the comment tool to report meaningful progress steps."
	}
	return base
}

// buildInitialUserMessage returns the first user turn: optional prior-conversation
// history, the task, required reads, and the working directory.
func buildInitialUserMessage(cfg toolLoopConfig) string {
	var sb strings.Builder
	if h := strings.TrimSpace(cfg.History); h != "" {
		sb.WriteString("Conversation so far (earlier turns, oldest first):\n")
		sb.WriteString(h)
		sb.WriteString("\n\n")
	}
	sb.WriteString("Task:\n")
	sb.WriteString(cfg.UserTask)
	if len(cfg.RequiredReads) > 0 {
		paths := make([]string, 0, len(cfg.RequiredReads))
		for _, p := range cfg.RequiredReads {
			p = filepath.ToSlash(strings.TrimSpace(p))
			if p != "" {
				paths = append(paths, p)
			}
		}
		sort.Strings(paths)
		if len(paths) > 0 {
			sb.WriteString("\n\nRequired first reads (must be done with file-read before anything else):\n- ")
			sb.WriteString(strings.Join(paths, "\n- "))
		}
	}
	sb.WriteString("\n\nWorking directory:\n")
	sb.WriteString(cfg.CWD)
	return sb.String()
}

// buildTurnUserMessage returns the user turn appended when a structured prior
// conversation (cfg.Messages) is carried in. Unlike buildInitialUserMessage it
// contains only this turn's task and required reads: the working directory and
// any earlier context already live in the carried messages, and repeating them
// here would both waste tokens and change the prompt prefix between turns.
func buildTurnUserMessage(cfg toolLoopConfig) string {
	var sb strings.Builder
	sb.WriteString("Task:\n")
	sb.WriteString(cfg.UserTask)
	if len(cfg.RequiredReads) > 0 {
		paths := make([]string, 0, len(cfg.RequiredReads))
		for _, p := range cfg.RequiredReads {
			p = filepath.ToSlash(strings.TrimSpace(p))
			if p != "" {
				paths = append(paths, p)
			}
		}
		sort.Strings(paths)
		if len(paths) > 0 {
			sb.WriteString("\n\nRequired first reads (must be done with file-read before anything else):\n- ")
			sb.WriteString(strings.Join(paths, "\n- "))
		}
	}
	return sb.String()
}

// builtinNativeToolDescs and builtinNativeToolSchemas define the description and
// real JSON Schema for each built-in tool on the native tool-calling path.
var builtinNativeToolDescs = map[string]string{
	"comment":            "Emit a progress message visible to the user.",
	"ls":                 "List directory entries.",
	"file-read":          "Read a file, optionally bounded to a line range.",
	"file-write":         "Create or overwrite a file, optionally appending.",
	"file-edit":          "Apply targeted string replacements or line-range edits to a file.",
	"multi-edit":         "Apply an ordered list of find/replace edits to one file atomically: each edit sees the result of the previous one, and if any edit fails to match uniquely the whole call fails and the file is untouched.",
	"glob":               "Find files matching a glob pattern (** for recursive search).",
	"grep":               "Search files with a regular expression.",
	"repo-search":        "Full-text search across the repository. For a symbol name (function, type, class, const) it lists ranked definitions first, then usages.",
	"shell-exec":         "Execute a shell command. Set run_in_background for long-running commands (servers, watchers); a job ID is returned immediately.",
	"bash":               "Execute a shell command. Set run_in_background for long-running commands (servers, watchers); a job ID is returned immediately.",
	"bash-output":        "Fetch output of a background job or spooled result by job_id (job-N or spool:N), or execute a shell command when given command.",
	"job-output":         "Fetch accumulated stdout/stderr of a background job (job-N) or page through a spooled truncated tool result (spool:N). Pass the next_offset from the previous call to read incrementally.",
	"job-kill":           "Terminate a background job by ID.",
	"tool-output":        "Re-read the full output of an earlier tool call that was offloaded to disk (stubs like [offloaded: … tool-output {\"id\":\"spool:N\"}]). Page with offset/limit; pass the next_offset from the previous call to continue.",
	"pty-start":          "Start an interactive terminal session (REPL, debugger, ssh, watch-mode server) under a pseudo-terminal. Returns a session ID plus the initial screen; drive it with pty-write.",
	"pty-write":          "Send input to a pty session and return output produced since the last read. Backslash escapes in input are decoded server-side (\\r \\n \\t \\e \\xHH \\uHHHH; \\\\ for a literal backslash), so {\"input\":\"2+2\",\"submit\":true} runs a REPL line and {\"input\":\"\\x03\"} sends Ctrl-C. submit:true appends \\r. Prefer wait_for (return as soon as this literal string, e.g. the prompt \">>> \", appears in new output) over guessing wait_ms. Empty input just polls.",
	"pty-kill":           "Terminate a pty session (SIGTERM, then SIGKILL) and free it.",
	"web-fetch":          "Fetch a URL and return its content as readable text/markdown (truncated to a size budget). For binary files use the download tool.",
	"download":           "Download a URL to a file inside the workspace, subject to a maximum size limit.",
	"web-search":         "Search the web.",
	"ask-user":           "Ask the user a question and wait for their answer.",
	"agent":              "Delegate a task to a named sub-agent.",
	"ultra":              "Fan a task out across many parallel sub-agents (2-32). prompt_template must contain {{item}}; each item fills the template into one self-contained sub-agent task. Sub-agents cannot see your context or each other, so include file paths, constraints, and expected output in the template. Give every item a distinct, non-overlapping scope; never let two agents touch the same file. Results are returned in input order.",
	"save-memory":        "Save one short durable fact or user preference to persistent memory; it is loaded into context in future sessions. Use scope \"project\" for facts specific to this repository.",
	"todo-write":         "Persist the session todo list (flat alias of the task tools; prefer task-create/task-update for dependent tasks).",
	"task-create":        "Create a task in the persistent session task graph. dependencies lists task IDs that must be completed first; cycles and unknown IDs are rejected.",
	"task-get":           "Get a task by ID.",
	"task-update":        "Update a task. Setting status to in_progress/completed fails while dependencies are incomplete — finish those first.",
	"task-list":          "List tasks in dependency order with a blocked_by field. Filter by status, or use \"ready\" (pending with all dependencies met) to pick the next task.",
	"task-delete":        "Delete a task by id, or set clear_completed to prune all completed/cancelled tasks. References to deleted tasks are stripped from other tasks' dependencies. Prefer marking tasks completed; delete only to prune finished work or discard an abandoned plan.",
	"task-stop":          "Stop the current task.",
	"goal-complete":      "Declare the goal fully achieved and verified; ends the run. Only call after you have confirmed the objective is met (tests pass / build green / change applied).",
	"tool-search":        "Search available tool definitions.",
	"skill-list":         "List available skills.",
	"skill-read":         "Read a skill definition.",
	"activate-skill":     "Activate a skill.",
	"skill-activate":     "Activate a skill.",
	"config":             "Get or set configuration values.",
	"diagnostics":        "Return current language-server diagnostics for a file (or every file seen so far when path is omitted).",
	"references":         "Language-server lookup: find references to a symbol, or its definition with kind=\"definition\". Position by symbol name or 1-based line/character.",
	"hover":              "Language-server hover: type signature and documentation for a symbol. Position by symbol name or 1-based line/character.",
	"rename-symbol":      "Language-server rename: rename a symbol across the workspace and apply the edits. Position by symbol name or 1-based line/character; reports the files changed.",
	"lsp-restart":        "Restart a wedged language server (all servers when none named).",
	"enter-plan-mode":    "Enter plan mode.",
	"exit-plan-mode":     "Exit plan mode.",
	"enter-worktree":     "Enter an isolated git worktree.",
	"exit-worktree":      "Exit the current worktree.",
	"send-message":       "Send a message to another agent.",
	"sandbox":            "Query or configure OS-level sandbox permissions.",
	"mcp-list-resources": "List resources exposed by an MCP server.",
	"mcp-read-resource":  "Read an MCP resource.",
	"mcp-auth":           "Authenticate with an MCP server.",
	"grok-image":         "Generate an image.",
	"grok-video":         "Generate a video.",
	"view-image":         "Attach an image file from the workspace so you can SEE it (vision models). Combine with the shell tools to inspect anything visually: capture a page yourself (e.g. `chromium --headless --screenshot=shot.png <url>` or `npx playwright screenshot <url> shot.png`), then view the file — no need to ask the user for screenshots.",
}

var builtinNativeToolSchemas = map[string]json.RawMessage{
	"comment":            json.RawMessage(`{"type":"object","properties":{"message":{"type":"string"}},"required":["message"]}`),
	"ls":                 json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}}}`),
	"file-read":          json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"},"start_line":{"type":"integer"},"end_line":{"type":"integer"}},"required":["path"]}`),
	"file-write":         json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"},"content":{"type":"string"},"append":{"type":"boolean"}},"required":["path","content"]}`),
	"file-edit":          json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"},"old_string":{"type":"string"},"new_string":{"type":"string"},"replace_all":{"type":"boolean"},"start_line":{"type":"integer"},"end_line":{"type":"integer"},"expected_replacements":{"type":"integer"},"edits":{"type":"array","items":{"type":"object","properties":{"old_string":{"type":"string"},"new_string":{"type":"string"},"replace_all":{"type":"boolean"}},"required":["old_string","new_string"]}}},"required":["path"]}`),
	"multi-edit":         json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"},"edits":{"type":"array","minItems":1,"items":{"type":"object","properties":{"old_string":{"type":"string"},"new_string":{"type":"string"},"replace_all":{"type":"boolean"}},"required":["old_string","new_string"]}}},"required":["path","edits"]}`),
	"glob":               json.RawMessage(`{"type":"object","properties":{"pattern":{"type":"string"},"path":{"type":"string"}},"required":["pattern"]}`),
	"grep":               json.RawMessage(`{"type":"object","properties":{"pattern":{"type":"string"},"glob":{"type":"string"},"type":{"type":"string"},"case_insensitive":{"type":"boolean"},"context":{"type":"integer"},"output_mode":{"type":"string","enum":["content","files_with_matches","count"]},"max_results":{"type":"integer"}},"required":["pattern"]}`),
	"repo-search":        json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}},"required":["query"]}`),
	"shell-exec":         json.RawMessage(`{"type":"object","properties":{"command":{"type":"string"},"run_in_background":{"type":"boolean"}},"required":["command"]}`),
	"bash":               json.RawMessage(`{"type":"object","properties":{"command":{"type":"string"},"run_in_background":{"type":"boolean"}},"required":["command"]}`),
	"bash-output":        json.RawMessage(`{"type":"object","properties":{"command":{"type":"string"},"run_in_background":{"type":"boolean"},"job_id":{"type":"string"},"offset":{"type":"number"}}}`),
	"job-output":         json.RawMessage(`{"type":"object","properties":{"job_id":{"type":"string"},"offset":{"type":"integer"}},"required":["job_id"]}`),
	"job-kill":           json.RawMessage(`{"type":"object","properties":{"job_id":{"type":"string"}},"required":["job_id"]}`),
	"tool-output":        json.RawMessage(`{"type":"object","properties":{"id":{"type":"string"},"offset":{"type":"integer"},"limit":{"type":"integer"}},"required":["id"]}`),
	"pty-start":          json.RawMessage(`{"type":"object","properties":{"command":{"type":"string"},"cols":{"type":"integer"},"rows":{"type":"integer"}},"required":["command"]}`),
	"pty-write":          json.RawMessage(`{"type":"object","properties":{"id":{"type":"string"},"input":{"type":"string"},"submit":{"type":"boolean","description":"append \\r to submit the input as a line"},"wait_for":{"type":"string","description":"return as soon as this literal string appears in new output (default timeout 10s)"},"wait_ms":{"type":"integer"}},"required":["id"]}`),
	"pty-kill":           json.RawMessage(`{"type":"object","properties":{"id":{"type":"string"}},"required":["id"]}`),
	"web-fetch":          json.RawMessage(`{"type":"object","properties":{"url":{"type":"string"},"max_length":{"type":"integer"}},"required":["url"]}`),
	"download":           json.RawMessage(`{"type":"object","properties":{"url":{"type":"string"},"path":{"type":"string"},"max_bytes":{"type":"integer"}},"required":["url","path"]}`),
	"web-search":         json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"},"max_results":{"type":"integer"}},"required":["query"]}`),
	"ask-user":           json.RawMessage(`{"type":"object","properties":{"question":{"type":"string"},"options":{"type":"array","items":{"type":"string"}},"context":{"type":"string"},"default_option":{"type":"string"},"allow_free_response":{"type":"boolean"}},"required":["question"]}`),
	"agent":              json.RawMessage(`{"type":"object","properties":{"agent":{"type":"string"},"task":{"type":"string"},"constraints":{"type":"string"},"expected_output":{"type":"string"},"parent_agent_id":{"type":"string"}},"required":["agent","task"]}`),
	"ultra":              json.RawMessage(`{"type":"object","properties":{"description":{"type":"string","description":"short summary of the overall fan-out"},"prompt_template":{"type":"string","description":"task template containing the {{item}} placeholder"},"items":{"type":"array","minItems":2,"maxItems":32,"items":{"type":"string"}},"subagent_type":{"type":"string","description":"worker agent id to run (default: code)"}},"required":["description","prompt_template","items"]}`),
	"save-memory":        json.RawMessage(`{"type":"object","properties":{"fact":{"type":"string"},"scope":{"type":"string","enum":["user","project"]}},"required":["fact"]}`),
	"todo-write":         json.RawMessage(`{"type":"object","properties":{"todos":{"type":"array","items":{"type":"object","properties":{"id":{"type":"string"},"content":{"type":"string"},"status":{"type":"string"},"owner":{"type":"string"},"source":{"type":"string"},"priority":{"type":"string"},"dependencies":{"type":"array","items":{"type":"string"}}},"required":["content"]}}},"required":["todos"]}`),
	"task-create":        json.RawMessage(`{"type":"object","properties":{"id":{"type":"string"},"content":{"type":"string"},"status":{"type":"string"},"owner":{"type":"string"},"source":{"type":"string"},"priority":{"type":"string"},"dependencies":{"type":"array","items":{"type":"string"}}},"required":["content"]}`),
	"task-get":           json.RawMessage(`{"type":"object","properties":{"id":{"type":"string"}},"required":["id"]}`),
	"task-update":        json.RawMessage(`{"type":"object","properties":{"id":{"type":"string"},"content":{"type":"string"},"status":{"type":"string"},"owner":{"type":"string"},"source":{"type":"string"},"priority":{"type":"string"},"dependencies":{"type":"array","items":{"type":"string"}}},"required":["id"]}`),
	"task-list":          json.RawMessage(`{"type":"object","properties":{"status":{"type":"string"}}}`),
	"task-delete":        json.RawMessage(`{"type":"object","properties":{"id":{"type":"string"},"clear_completed":{"type":"boolean"}}}`),
	"task-stop":          json.RawMessage(`{"type":"object","properties":{"reason":{"type":"string"}}}`),
	"goal-complete":      json.RawMessage(`{"type":"object","properties":{"summary":{"type":"string"},"verified":{"type":"boolean"}},"required":["summary"]}`),
	"tool-search":        json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}},"required":["query"]}`),
	"skill-list":         json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}}}`),
	"skill-read":         json.RawMessage(`{"type":"object","properties":{"name":{"type":"string"},"skill":{"type":"string"},"location":{"type":"string"}}}`),
	"activate-skill":     json.RawMessage(`{"type":"object","properties":{"name":{"type":"string"},"skill":{"type":"string"},"location":{"type":"string"}}}`),
	"skill-activate":     json.RawMessage(`{"type":"object","properties":{"name":{"type":"string"},"skill":{"type":"string"},"location":{"type":"string"}}}`),
	"config":             json.RawMessage(`{"type":"object","properties":{"action":{"type":"string","enum":["get","set"]},"key":{"type":"string"},"value":{"type":"string"},"force":{"type":"boolean"}},"required":["action"]}`),
	"diagnostics":        json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}}}`),
	"references":         json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"},"symbol":{"type":"string"},"kind":{"type":"string","enum":["references","definition"]},"line":{"type":"integer"},"character":{"type":"integer"}},"required":["path"]}`),
	"hover":              json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"},"symbol":{"type":"string"},"line":{"type":"integer"},"character":{"type":"integer"}},"required":["path"]}`),
	"rename-symbol":      json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"},"new_name":{"type":"string"},"symbol":{"type":"string"},"line":{"type":"integer"},"character":{"type":"integer"}},"required":["path","new_name"]}`),
	"lsp-restart":        json.RawMessage(`{"type":"object","properties":{"server":{"type":"string"}}}`),
	"enter-plan-mode":    json.RawMessage(`{"type":"object","properties":{"reason":{"type":"string"}}}`),
	"exit-plan-mode":     json.RawMessage(`{"type":"object","properties":{"reason":{"type":"string"}}}`),
	"enter-worktree":     json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"},"branch":{"type":"string"},"allow_dirty":{"type":"boolean"}}}`),
	"exit-worktree":      json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"},"force":{"type":"boolean"}},"required":["path"]}`),
	"send-message":       json.RawMessage(`{"type":"object","properties":{"target":{"type":"string"},"message":{"type":"string"}},"required":["message"]}`),
	"sandbox":            json.RawMessage(`{"type":"object","properties":{"action":{"type":"string","enum":["status","request"]},"add_writable_dir":{"type":"string"},"net":{"type":"string","enum":["all","localhost","none","ports"]},"ports":{"type":"array","items":{"type":"integer"}},"reason":{"type":"string"}},"required":["action"]}`),
	"mcp-list-resources": json.RawMessage(`{"type":"object","properties":{"server_id":{"type":"string"}},"required":["server_id"]}`),
	"mcp-read-resource":  json.RawMessage(`{"type":"object","properties":{"server_id":{"type":"string"},"resource_id":{"type":"string"}},"required":["server_id","resource_id"]}`),
	"mcp-auth":           json.RawMessage(`{"type":"object","properties":{"server_id":{"type":"string"},"token":{"type":"string"},"scope":{"type":"string"},"expires_at":{"type":"string"},"description":{"type":"string"}},"required":["server_id"]}`),
	"grok-image":         json.RawMessage(`{"type":"object","properties":{"prompt":{"type":"string"},"path":{"type":"string"},"model":{"type":"string"},"n":{"type":"integer"},"aspect_ratio":{"type":"string"},"resolution":{"type":"string","enum":["1k","2k"]},"response_format":{"type":"string","enum":["url","b64_json"]}},"required":["prompt"]}`),
	"grok-video":         json.RawMessage(`{"type":"object","properties":{"prompt":{"type":"string"},"path":{"type":"string"},"model":{"type":"string"},"duration":{"type":"integer"},"aspect_ratio":{"type":"string"},"resolution":{"type":"string"},"image_url":{"type":"string"},"reference_image_urls":{"type":"array","items":{"type":"string"}}},"required":["prompt"]}`),
	"view-image":         json.RawMessage(`{"type":"object","properties":{"path":{"type":"string","description":"image file inside the workspace (png, jpg, webp, gif)"}},"required":["path"]}`),
}

// buildToolSpecs returns provider.ToolSpec entries for each allowed tool that has
// a registered native schema. Tools without a schema entry (e.g. manifest/MCP
// tools) are omitted; the caller decides whether to fall back to text protocol
// when the resulting slice is empty.
func buildToolSpecs(allowedTools []string) []provider.ToolSpec {
	seen := map[string]struct{}{}
	var out []provider.ToolSpec
	for _, name := range allowedTools {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if _, dup := seen[name]; dup {
			continue
		}
		desc, hasDesc := builtinNativeToolDescs[name]
		schema, hasSchema := builtinNativeToolSchemas[name]
		if !hasDesc || !hasSchema {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, provider.ToolSpec{Name: name, Description: desc, Schema: schema})
	}
	return out
}
