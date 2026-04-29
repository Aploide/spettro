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
		output = strings.Join(strings.Fields(output), " ")
		limit := 240
		if name == "agent" {
			limit = 4000
		}
		parts = append(parts, "output="+truncate(output, limit))
	}
	return strings.Join(parts, " | ")
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
	return fmt.Sprintf(`%s

You can use tools iteratively.
Allowed tools: %s

Output protocol (strict):
1) To call tools (all executed in parallel), output one TOOL_CALL per line:
TOOL_CALL {"name":"<tool-name>","arguments":{...}}
TOOL_CALL {"name":"<another>","arguments":{...}}
2) When done, output exactly:
FINAL
<your final answer>

Rules:
- Known aliases accepted by runtime: tool/args and function{name,arguments}.
- For the agent tool, arguments must include {"agent":"<handoff-id>","task":"..."}.
- Prefer reading/searching before writing.
- Never edit an existing file unless it has been read first.
- Creating a brand-new file without reading is allowed.
- Keep tool args minimal and valid JSON.
- If a tool fails, adapt and continue.
%s

Task:
%s
%s

Working directory:
%s

Current step: %d/%d

Previous tool interaction log:
%s`, base, toolList, commentGuidance, cfg.UserTask, requiredReadsSection, cfg.CWD, step, cfg.MaxSteps, emptyIfBlank(history))
}
