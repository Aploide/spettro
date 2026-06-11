package agent

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"spettro/internal/skills"
)

func summarizeLoopToolResult(name, args, status, output string) string {
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
		// Preserve newlines so the model sees the structure of files and search
		// results — it needs them to build correct multi-line edits. Only the
		// length is bounded, per tool (the overall rolling history is still
		// capped by maxHistoryBytes).
		parts = append(parts, "output="+truncate(output, toolOutputHistoryLimit(name)))
	}
	return strings.Join(parts, " | ")
}

// toolOutputHistoryLimit returns how many characters of a tool's output are fed
// back to the model in the next-step history. Read/search tools get a generous
// budget because the model acts on their contents; chatty/again-fetchable tools
// get less. Previously every tool was flattened to 240 chars, which made
// informed multi-line edits essentially impossible.
func toolOutputHistoryLimit(name string) int {
	switch name {
	case "file-read":
		return 8000
	case "repo-search", "grep", "glob", "ls":
		return 4000
	case "shell-exec", "bash", "bash-output":
		return 4000
	case "agent":
		return 4000
	default:
		return 1000
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

func buildLoopPrompt(cfg toolLoopConfig, history string, step int) string {
	toolList := strings.Join(cfg.AllowedTools, ", ")
	base := strings.TrimSpace(cfg.SystemPrompt)
	if base == "" {
		base = "You are an assistant."
	}
	if catalog := skills.CatalogPrompt(cfg.SkillsCatalog); catalog != "" {
		base = base + catalog
	}
	schemaSection := buildToolSchemaSection(cfg.AllowedTools)
	commentGuidance := ""
	for _, tool := range cfg.AllowedTools {
		if tool == "comment" {
			commentGuidance = "\n- Use the comment tool to narrate meaningful progress in the chat.\n- Before major operations (file-write, shell/batch commands, sub-agent delegation), emit a short comment about what you are about to do.\n- After major operations, emit a short success/failure comment including what happened.\n- Prefer a small number of useful comments over narrating every single tool call.\n- Do not narrate with plain text when you still plan to continue; use comment for progress updates and FINAL only when actually done."
			break
		}
	}
	requiredReadsSection := ""
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
			requiredReadsSection = "\nRequired first reads (must be done with file-read before anything else):\n- " + strings.Join(paths, "\n- ")
		}
	}
	// Cross-turn conversation history (EFF-2). Empty on a first turn so the
	// rendered prompt is byte-for-byte identical to the pre-history behavior.
	conversationSection := ""
	if h := strings.TrimSpace(cfg.History); h != "" {
		conversationSection = "\nConversation so far (earlier turns, oldest first):\n" + h + "\n"
	}
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
%s
Task:
%s
%s

Working directory:
%s

Current step: %d/%d

Previous tool interaction log:
%s`, base, toolList, schemaSection, commentGuidance, conversationSection, cfg.UserTask, requiredReadsSection, cfg.CWD, step, cfg.MaxSteps, emptyIfBlank(history))
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
