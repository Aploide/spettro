package agent

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"spettro/internal/provider"
	"spettro/internal/skills"
)

// summarizeLoopToolResult formats a tool result for the text-protocol history
// message. outputLimit caps the output chars fed to the model; callers should
// pass runtime.historyLimit(name) so manifest overrides apply.
func summarizeLoopToolResult(name, args, status, output string, outputLimit int) string {
	var parts []string
	status = strings.TrimSpace(status)
	if status != "" {
		parts = append(parts, "status="+status)
	}
	if summary := summarizeLoopToolArgs(name, args); summary != "" {
		parts = append(parts, summary)
	}
	output = strings.TrimSpace(output)
	if output != "" {
		parts = append(parts, "output="+truncate(output, outputLimit))
	}
	return strings.Join(parts, " | ")
}

// toolOutputHistoryLimit returns the default character cap for a tool's output
// in model history. These defaults intentionally match the source caps in
// execute() so the model always sees what it just read.
func toolOutputHistoryLimit(name string) int {
	switch name {
	case "file-read":
		return 40000
	case "repo-search", "grep", "glob", "ls":
		return 16000
	case "shell-exec", "bash", "bash-output":
		return 8000
	case "agent":
		return 8000
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

// buildSystemString returns the system-role content for the request.
// When nativeTools is true the TOOL_CALL/FINAL text protocol is omitted because
// the model receives tool schemas via the API and uses structured tool calls.
func buildSystemString(cfg toolLoopConfig, step int, nativeTools bool) string {
	base := strings.TrimSpace(cfg.SystemPrompt)
	if base == "" {
		base = "You are an assistant."
	}
	if catalog := skills.CatalogPrompt(cfg.SkillsCatalog); catalog != "" {
		base = base + catalog
	}
	commentGuidance := ""
	for _, tool := range cfg.AllowedTools {
		if tool == "comment" {
			if nativeTools {
				commentGuidance = "\n- Use the comment tool to report meaningful progress steps."
			} else {
				commentGuidance = "\n- Use the comment tool to narrate meaningful progress in the chat.\n- Before major operations (file-write, shell/batch commands, sub-agent delegation), emit a short comment about what you are about to do.\n- After major operations, emit a short success/failure comment including what happened.\n- Prefer a small number of useful comments over narrating every single tool call.\n- Do not narrate with plain text when you still plan to continue; use comment for progress updates and FINAL only when actually done."
			}
			break
		}
	}
	if nativeTools {
		return fmt.Sprintf("%s%s\n\nCurrent step: %d", base, commentGuidance, step)
	}
	toolList := strings.Join(cfg.AllowedTools, ", ")
	schemaSection := buildToolSchemaSection(cfg.AllowedTools)
	return fmt.Sprintf(`%s

You can use tools iteratively.
Allowed tools: %s
%s
Output protocol (strict):
1) To call tools (all executed in parallel), output one TOOL_CALL per line:
TOOL_CALL {"name":"<tool-name>","arguments":{...}}
TOOL_CALL {"name":"<another>","arguments":{...}}
2) When done, output exactly:
FINAL
<your final answer>

Rules:
- Known aliases accepted by runtime: tool/args and function{name,arguments}.
- Use ONLY the field names listed in the tool argument schemas above. Unknown fields will be rejected.
- For the agent tool, arguments must include {"agent":"<handoff-id>","task":"..."}.
- Prefer reading/searching before writing.
- Never edit an existing file unless it has been read first.
- Creating a brand-new file without reading is allowed.
- Keep tool args minimal and valid JSON.
- If a tool fails, adapt and continue.
%s

Current step: %d`, base, toolList, schemaSection, commentGuidance, step)
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

// builtinToolSchemas describes the JSON arguments object accepted by every
// built-in tool dispatched in llm_runtime.go (and friends). The runtime decodes
// each tool's arguments with json.Decoder.DisallowUnknownFields(), so the LLM
// MUST use these exact field names. Optional fields are flagged with a `?`.
//
// When a manifest exposes additional tools (mcp/script/http), they will simply
// be omitted from the rendered schema section; the agent prompt should mention
// their schema separately if needed.
var builtinToolSchemas = map[string]string{
	"comment":            `{"message": string}`,
	"ls":                 `{"path"?: string}`,
	"file-read":          `{"path": string, "start_line"?: int, "end_line"?: int}`,
	"file-write":         `{"path": string, "content": string, "append"?: bool}`,
	"file-edit":          `{"path": string, "old_string"?: string, "new_string"?: string, "replace_all"?: bool, "start_line"?: int, "end_line"?: int, "expected_replacements"?: int, "edits"?: [{"old_string": string, "new_string": string, "replace_all"?: bool}]}`,
	"glob":               `{"pattern": string, "path"?: string}`,
	"grep":               `{"pattern": string, "glob"?: string, "type"?: string, "case_insensitive"?: bool, "context"?: int, "output_mode"?: "content"|"files_with_matches"|"count", "max_results"?: int}`,
	"repo-search":        `{"query": string}`,
	"sandbox":            `{"action": "status"|"request", "add_writable_dir"?: string, "net"?: "all"|"localhost"|"none"|"ports", "ports"?: [int], "reason"?: string}`,
	"shell-exec":         `{"command": string}`,
	"bash":               `{"command": string}`,
	"bash-output":        `{"command": string}`,
	"web-fetch":          `{"url": string}`,
	"web-search":         `{"query": string, "max_results"?: int}`,
	"grok-image":         `{"prompt": string, "path"?: string, "model"?: string, "n"?: int, "aspect_ratio"?: string, "resolution"?: "1k"|"2k", "response_format"?: "url"|"b64_json"}`,
	"grok-video":         `{"prompt": string, "path"?: string, "model"?: string, "duration"?: int, "aspect_ratio"?: string, "resolution"?: string, "image_url"?: string, "reference_image_urls"?: [string]}`,
	"ask-user":           `{"question": string, "options"?: [string], "context"?: string, "default_option"?: string, "allow_free_response"?: bool}`,
	"agent":              `{"agent": string, "task": string, "constraints"?: string, "expected_output"?: string, "parent_agent_id"?: string}`,
	"todo-write":         `{"todos": [{"id"?: string, "content": string, "status"?: "pending"|"in_progress"|"completed", "owner"?: string, "source"?: string, "priority"?: string, "dependencies"?: [string]}]}`,
	"task-create":        `{"id"?: string, "content": string, "status"?: string, "owner"?: string, "source"?: string, "priority"?: string, "dependencies"?: [string]}`,
	"task-get":           `{"id": string}`,
	"task-update":        `{"id": string, "content"?: string, "status"?: string, "owner"?: string, "source"?: string, "priority"?: string, "dependencies"?: [string]}`,
	"task-list":          `{"status"?: string}`,
	"task-stop":          `{"reason"?: string}`,
	"goal-complete":      `{"summary": string, "verified"?: bool}`,
	"tool-search":        `{"query": string}`,
	"skill-list":         `{"query"?: string}`,
	"skill-read":         `{"name"?: string, "skill"?: string, "location"?: string}`,
	"activate-skill":     `{"name"?: string, "skill"?: string, "location"?: string}`,
	"skill-activate":     `{"name"?: string, "skill"?: string, "location"?: string}`,
	"config":             `{"action": "get"|"set", "key"?: string, "value"?: string, "force"?: bool}`,
	"mcp-list-resources": `{"server_id": string}`,
	"mcp-read-resource":  `{"server_id": string, "resource_id": string}`,
	"mcp-auth":           `{"server_id": string, "token"?: string, "scope"?: string, "expires_at"?: string, "description"?: string}`,
	"enter-plan-mode":    `{"reason"?: string}`,
	"exit-plan-mode":     `{"reason"?: string}`,
	"enter-worktree":     `{"path"?: string, "branch"?: string, "allow_dirty"?: bool}`,
	"exit-worktree":      `{"path": string, "force"?: bool}`,
	"send-message":       `{"target"?: string, "message": string}`,
}

// builtinNativeToolDescs and builtinNativeToolSchemas define the description and
// real JSON Schema for each built-in tool on the native tool-calling path.
var builtinNativeToolDescs = map[string]string{
	"comment":            "Emit a progress message visible to the user.",
	"ls":                 "List directory entries.",
	"file-read":          "Read a file, optionally bounded to a line range.",
	"file-write":         "Create or overwrite a file, optionally appending.",
	"file-edit":          "Apply targeted string replacements or line-range edits to a file.",
	"glob":               "Find files matching a glob pattern (** for recursive search).",
	"grep":               "Search files with a regular expression.",
	"repo-search":        "Semantic full-text search across the repository.",
	"shell-exec":         "Execute a shell command.",
	"bash":               "Execute a shell command.",
	"bash-output":        "Execute a shell command.",
	"web-fetch":          "Fetch the content of a URL.",
	"web-search":         "Search the web.",
	"ask-user":           "Ask the user a question and wait for their answer.",
	"agent":              "Delegate a task to a named sub-agent.",
	"todo-write":         "Persist the session todo list.",
	"task-create":        "Create a task.",
	"task-get":           "Get a task by ID.",
	"task-update":        "Update a task.",
	"task-list":          "List tasks, optionally filtered by status.",
	"task-stop":          "Stop the current task.",
	"goal-complete":      "Declare the goal fully achieved and verified; ends the run. Only call after you have confirmed the objective is met (tests pass / build green / change applied).",
	"tool-search":        "Search available tool definitions.",
	"skill-list":         "List available skills.",
	"skill-read":         "Read a skill definition.",
	"activate-skill":     "Activate a skill.",
	"skill-activate":     "Activate a skill.",
	"config":             "Get or set configuration values.",
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
}

var builtinNativeToolSchemas = map[string]json.RawMessage{
	"comment":            json.RawMessage(`{"type":"object","properties":{"message":{"type":"string"}},"required":["message"]}`),
	"ls":                 json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}}}`),
	"file-read":          json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"},"start_line":{"type":"integer"},"end_line":{"type":"integer"}},"required":["path"]}`),
	"file-write":         json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"},"content":{"type":"string"},"append":{"type":"boolean"}},"required":["path","content"]}`),
	"file-edit":          json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"},"old_string":{"type":"string"},"new_string":{"type":"string"},"replace_all":{"type":"boolean"},"start_line":{"type":"integer"},"end_line":{"type":"integer"},"expected_replacements":{"type":"integer"},"edits":{"type":"array","items":{"type":"object","properties":{"old_string":{"type":"string"},"new_string":{"type":"string"},"replace_all":{"type":"boolean"}},"required":["old_string","new_string"]}}},"required":["path"]}`),
	"glob":               json.RawMessage(`{"type":"object","properties":{"pattern":{"type":"string"},"path":{"type":"string"}},"required":["pattern"]}`),
	"grep":               json.RawMessage(`{"type":"object","properties":{"pattern":{"type":"string"},"glob":{"type":"string"},"type":{"type":"string"},"case_insensitive":{"type":"boolean"},"context":{"type":"integer"},"output_mode":{"type":"string","enum":["content","files_with_matches","count"]},"max_results":{"type":"integer"}},"required":["pattern"]}`),
	"repo-search":        json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}},"required":["query"]}`),
	"shell-exec":         json.RawMessage(`{"type":"object","properties":{"command":{"type":"string"}},"required":["command"]}`),
	"bash":               json.RawMessage(`{"type":"object","properties":{"command":{"type":"string"}},"required":["command"]}`),
	"bash-output":        json.RawMessage(`{"type":"object","properties":{"command":{"type":"string"}},"required":["command"]}`),
	"web-fetch":          json.RawMessage(`{"type":"object","properties":{"url":{"type":"string"}},"required":["url"]}`),
	"web-search":         json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"},"max_results":{"type":"integer"}},"required":["query"]}`),
	"ask-user":           json.RawMessage(`{"type":"object","properties":{"question":{"type":"string"},"options":{"type":"array","items":{"type":"string"}},"context":{"type":"string"},"default_option":{"type":"string"},"allow_free_response":{"type":"boolean"}},"required":["question"]}`),
	"agent":              json.RawMessage(`{"type":"object","properties":{"agent":{"type":"string"},"task":{"type":"string"},"constraints":{"type":"string"},"expected_output":{"type":"string"},"parent_agent_id":{"type":"string"}},"required":["agent","task"]}`),
	"todo-write":         json.RawMessage(`{"type":"object","properties":{"todos":{"type":"array","items":{"type":"object","properties":{"id":{"type":"string"},"content":{"type":"string"},"status":{"type":"string"},"owner":{"type":"string"},"source":{"type":"string"},"priority":{"type":"string"},"dependencies":{"type":"array","items":{"type":"string"}}},"required":["content"]}}},"required":["todos"]}`),
	"task-create":        json.RawMessage(`{"type":"object","properties":{"id":{"type":"string"},"content":{"type":"string"},"status":{"type":"string"},"owner":{"type":"string"},"source":{"type":"string"},"priority":{"type":"string"},"dependencies":{"type":"array","items":{"type":"string"}}},"required":["content"]}`),
	"task-get":           json.RawMessage(`{"type":"object","properties":{"id":{"type":"string"}},"required":["id"]}`),
	"task-update":        json.RawMessage(`{"type":"object","properties":{"id":{"type":"string"},"content":{"type":"string"},"status":{"type":"string"},"owner":{"type":"string"},"source":{"type":"string"},"priority":{"type":"string"},"dependencies":{"type":"array","items":{"type":"string"}}},"required":["id"]}`),
	"task-list":          json.RawMessage(`{"type":"object","properties":{"status":{"type":"string"}}}`),
	"task-stop":          json.RawMessage(`{"type":"object","properties":{"reason":{"type":"string"}}}`),
	"goal-complete":      json.RawMessage(`{"type":"object","properties":{"summary":{"type":"string"},"verified":{"type":"boolean"}},"required":["summary"]}`),
	"tool-search":        json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}},"required":["query"]}`),
	"skill-list":         json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}}}`),
	"skill-read":         json.RawMessage(`{"type":"object","properties":{"name":{"type":"string"},"skill":{"type":"string"},"location":{"type":"string"}}}`),
	"activate-skill":     json.RawMessage(`{"type":"object","properties":{"name":{"type":"string"},"skill":{"type":"string"},"location":{"type":"string"}}}`),
	"skill-activate":     json.RawMessage(`{"type":"object","properties":{"name":{"type":"string"},"skill":{"type":"string"},"location":{"type":"string"}}}`),
	"config":             json.RawMessage(`{"type":"object","properties":{"action":{"type":"string","enum":["get","set"]},"key":{"type":"string"},"value":{"type":"string"},"force":{"type":"boolean"}},"required":["action"]}`),
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

// buildToolSchemaSection renders a per-tool argument schema section to inject
// into the system prompt. Tools without a registered built-in schema are
// skipped (e.g. mcp/script/http tools defined by the manifest); duplicate
// entries — for example when both a tool and its alias are listed — are
// rendered once.
func buildToolSchemaSection(allowedTools []string) string {
	if len(allowedTools) == 0 {
		return ""
	}
	seen := map[string]struct{}{}
	var lines []string
	for _, name := range allowedTools {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		schema, ok := builtinToolSchemas[name]
		if !ok {
			continue
		}
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		lines = append(lines, fmt.Sprintf("- %s arguments: %s", name, schema))
	}
	if len(lines) == 0 {
		return ""
	}
	sort.Strings(lines)
	return "\nTool argument schemas (JSON object passed as \"arguments\"; ? marks optional fields):\n" + strings.Join(lines, "\n") + "\n"
}
