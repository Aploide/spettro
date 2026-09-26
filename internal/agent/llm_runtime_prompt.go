package agent

import (
	"encoding/json"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"spettro/internal/provider"
	"spettro/internal/shell"
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
	case "shell-exec", "bash", "bash-output":
		// Build and test logs are what the model iterates on; the failures
		// usually sit at the end, which spoolResult keeps (head + tail).
		return shellOutputHistoryLimit
	case "job-output", "tool-output", "pty-start", "pty-write":
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
// request. Never embed step counters, timestamps, or other per-call state here;
// the environment and project-instruction sections come from sessionContextFor,
// which freezes them per conversation for exactly this reason.
func buildSystemString(cfg toolLoopConfig) string {
	base := strings.TrimSpace(cfg.SystemPrompt)
	if base == "" {
		base = "You are an assistant."
	}
	if catalog := skills.CatalogPrompt(cfg.SkillsCatalog); catalog != "" {
		base = base + catalog
	}
	return base + sessionContextFor(cfg)
}

// buildInitialUserMessage returns the first user turn: optional prior-conversation
// history, the task and required reads. The working directory and environment
// live in the system prompt (see sessionContextFor).
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
	return sb.String()
}

// environmentBrief tells the model which OS and shell dialect its command
// lines will actually be executed by (rendered inside the environment section). Without it the model defaults to POSIX
// pipelines everywhere, which on a PowerShell host fail in confusing ways
// (`2>/dev/null` redirects to a file named "null", `&&` is a parse error).
func environmentBrief() string {
	lines := []string{
		"- OS: " + runtime.GOOS + "/" + runtime.GOARCH,
		"- Shell for shell-exec/bash tools: " + shell.Describe(),
		"- Path separator: " + string(filepath.Separator),
	}
	if shell.Dialect() == shell.KindPowerShell {
		lines = append(lines,
			"- Write PowerShell, not POSIX sh: no `&&`/`||` chaining in Windows PowerShell 5.1 (use `;` or separate calls),",
			"  redirect with `2>$null` not `2>/dev/null`, and prefer cmdlets (Get-ChildItem, Select-String) over ls/grep.",
			"- Native exit codes are propagated, so a failing command still reports failure.",
		)
	}
	return strings.Join(lines, "\n")
}

// buildTurnUserMessage returns the user turn appended when a structured prior
// conversation (cfg.Messages) is carried in. Unlike buildInitialUserMessage it
// contains only this turn's task and required reads: any earlier context
// already lives in the carried messages, and repeating it here would both
// waste tokens and change the prompt prefix between turns.
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

// shellToolDesc describes shell-exec and bash, which are the same tool.
const shellToolDesc = "Run a command in the working directory with the host shell (see Environment) and return its combined stdout+stderr. Each call is a fresh process: cd, exported variables and shell state do not carry over, so chain dependent steps in one command (e.g. `cd web && npm test`). Use it for builds, tests, linters, git and package managers. Do not use it to read, search or list files (cat/head/tail/grep/rg/find/ls): file-read, grep and glob are faster, cheaper and tracked. A failing command returns its full output followed by `[exit status N]`. Foreground commands run under a timeout: pass timeout (seconds, max 600) for slow builds, installs or test suites; on timeout the whole process group is killed and the partial output returned. Set run_in_background for servers and watchers that never exit; it returns a job ID immediately (read output with job-output, stop with job-kill). Never run interactive commands (editors, pagers, prompts, REPLs): pass non-interactive flags (--yes, -y, --no-pager, CI=1) or use pty-start. Output beyond the history cap (about 30,000 characters) keeps the head and the tail (where errors usually are); the footer names a spool ID to page the rest with tool-output. Quote paths that contain spaces."

// builtinNativeToolDescs and builtinNativeToolSchemas define the description and
// real JSON Schema for each built-in tool on the native tool-calling path.
var builtinNativeToolDescs = map[string]string{
	"comment":            "Optional: show a short progress note to the user. It does not advance the task, so never spend a step on it alone; add it next to real tool calls, and only for long-running work worth announcing.",
	"ls":                 "List the entries of one directory (default: the working directory), directories marked with a trailing slash. Not recursive; use glob to find files across the tree.",
	"file-read":          "Read a text file. path is relative to the working directory (absolute paths inside the workspace also work). Every line comes back prefixed with its 1-based line number and a tab, cat -n style (`     7\tcode`); that prefix is display only, so never copy it into old_string or file content. A call returns up to 2000 lines (and at most about 40,000 characters); when there is more, the footer gives the offset to continue from. To read part of a file pass offset (first line) and limit (line count), or start_line/end_line (inclusive). Read the whole file, or a generous range, rather than many tiny slices. Lines over 2000 characters are clipped and binary files are refused. Read several files in parallel in one step when you need them. You must read an existing file before editing or overwriting it.",
	"file-write":         "Create a file, or overwrite an existing one with the full new content. Parent directories are created. Overwriting an existing file is refused unless you have read it with file-read in this session (a grep or search hit does not count) and it has not changed since. For changes to an existing file prefer file-edit, which sends only the changed text; rewrite a whole file only when most of it changes. append=true adds content to the end instead of replacing it. Do not create files the task doesn't need (notes, summaries, README/docs, scratch scripts) and never write secrets.",
	"file-edit":          "Replace exact text in an existing file; read the file first. old_string must match the file byte for byte, including indentation, whitespace and line breaks, and without the line-number prefixes file-read shows. It must identify exactly one location: if it appears more than once, include more surrounding lines to make it unique, or set replace_all=true to change every occurrence (e.g. renaming a variable). new_string replaces it (empty deletes it) and must differ from old_string. Keep edits minimal and preserve the file's existing style. start_line/end_line limit the search to that line range; expected_replacements makes the call fail unless exactly that many replacements happen. For several changes to one file pass edits[] (applied in order, each seeing the previous result; if any fails, nothing is written) instead of several calls. Cannot create files: use file-write. The edit is refused if the file changed since you last read it (by you through the shell, or by someone else): re-read it and retry. When a match fails, re-read the file and copy the text exactly. On success the result shows a short diff of the change.",
	"multi-edit":         "Apply an ordered list of find/replace edits to one file atomically: each edit sees the result of the previous one, and if any edit fails to match uniquely the whole call fails and the file is untouched. The file-edit rules apply to every edit: read the file first, copy old_string exactly (no line-number prefixes), and make each one unique or set replace_all.",
	"glob":               "Find files by path pattern; returns matching paths relative to the working directory, sorted. Patterns match the path relative to the working directory (or to path), with * inside one path segment, ** across directories and {a,b} alternatives: `**/*.go` finds Go files anywhere, `internal/**/*_test.go` narrows to a subtree, `**/*.{ts,tsx}` matches either extension, and `*.md` matches only top-level files. path optionally limits the walk to a subdirectory. Honours .gitignore; .git, .spettro, vendor, node_modules, dist and build are skipped. Lists at most 1000 files. Use it to locate files by name; use grep to search their contents.",
	"grep":               "Search file contents with a regular expression (Go RE2 syntax: no lookarounds or backreferences; escape literal metacharacters, e.g. `\\(` or `\\.`). Searches the working directory recursively, or only path (a directory or file relative to the working directory) when given. Filter files with glob (`*.go` or `*_test.go` match the file name; a pattern containing / such as `internal/**/*.go` matches the path) or type (go, ts, js, py, rs, md, toml, json, yaml, sh). output_mode: content (default) returns `path:line: text` lines, with context lines around each match when context is set; files_with_matches returns only paths; count returns per-file counts. content stops after max_results matches (default 200); the other modes list at most max_results files, with full counts. Narrow the pattern or path when results are truncated. An unknown type is an error; use glob for other extensions. Honours .gitignore (nested ones too), skips binary and very large files unless named as path, and clips very long lines; .git, .spettro, vendor, node_modules, dist and build are skipped unless you name one as path. Use it instead of running grep/rg through the shell.",
	"repo-search":        "Case-insensitive literal (not regex) search across every file in the repository, returning `path:line: text` matches. For an identifier (function, type, method, class, const) it first lists ranked definitions, then usages: the fastest way to find where a symbol is defined and used. An empty query lists all files. Use grep when you need a regex, a file filter or context lines.",
	"shell-exec":         shellToolDesc,
	"bash":               shellToolDesc,
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
	"ask-user":           "Ask the user up to 4 related questions as one form and wait for their answers. Available in every interactive mode (not just planning): use it when a decision is genuinely the user's to make and proceeding on a guess would waste work — never for something you can determine yourself by reading the code. Batch questions that belong to the same decision into one call rather than interrupting repeatedly. Each question takes a short header (the tab label the UI shows), the question line, and up to 8 options; give every option a label plus a one-line description of what choosing it means, mark the one you would pick with is_recommended (the UI highlights and pre-selects it), and set preview when there is concrete content — a snippet, a layout, a config — worth showing beside the option. Set multi_select when several answers can hold at once: there is no exclusivity flag, so phrase those options such that any subset of them reads sensibly — an option that rules the others out has to say so in its own words. Set allow_custom when written input is useful: the user gets a free-text entry and their words come back verbatim, quoted. Answers return one line per question, keyed by header; a question the user skipped is marked as unanswered, so never read silence as agreement with your recommendation, and a multi-select question answered with none of the options is marked as such — that is a decision about them, not silence. The user may also attach a note to any question; it follows the answer as `— note: \"...\"` and can appear on an unanswered question too, where it is context, not a choice.",
	"agent":              "Delegate a task to a named sub-agent. Set isolation to \"worktree\" when the sub-agent will edit files and you want it sandboxed from the main checkout: it then runs in its own git worktree on a branch named after it (under .spettro/worktrees/), which is merged back and deleted automatically when it finishes; a merge conflict keeps the branch and worktree for manual resolution.",
	"ultra":              "Fan a task out across many parallel sub-agents (2-32). prompt_template must contain {{item}}; each item fills the template into one self-contained sub-agent task. Sub-agents cannot see your context or each other, so include file paths, constraints, and expected output in the template. Give every item a distinct, non-overlapping scope; never let two agents touch the same file. Results are returned in input order. When sub-agents edit files, set isolation to \"worktree\": each gets its own git worktree and branch under .spettro/worktrees/, and all branches are merged back into the main checkout and deleted after the swarm completes (conflicting branches are kept and reported for manual resolution).",
	"workflow":           "Run a deterministic multi-agent orchestration script you write. The script is JavaScript: `export const meta = {name, description, phases}` followed by a body that uses agent(prompt, opts) to run a sub-agent (returns its final text, or the parsed object when opts.schema is a JSON Schema, or null if it failed), parallel(thunks) to run several concurrently and wait for all of them, pipeline(items, ...stages) to push every item through every stage with no barrier between stages, phase(title) and log(message) for progress, args for the value you passed in, budget.remaining() for the token target, and workflow(name, args) to call a saved workflow. The body runs in an async context, so use await and a top-level return. Prefer pipeline over parallel-then-parallel: only use a barrier when a stage genuinely needs every previous result at once. Pass script for inline source, script_path to re-run an edited script, or name to run a saved workflow from .spettro/workflows. Set save_as to also store the script under that name for reuse (save_scope \"global\" puts it in ~/.spettro/workflows instead of the project). Unless the user wrote \"ultracode\", they are asked to confirm before the run starts and may choose to keep the script without running it. Date.now(), Math.random() and argless new Date() are unavailable (they would break resume).",
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
	"comment":            json.RawMessage(`{"type":"object","properties":{"message":{"type":"string","description":"one short line shown to the user"}},"required":["message"]}`),
	"ls":                 json.RawMessage(`{"type":"object","properties":{"path":{"type":"string","description":"directory relative to the working directory (default: the working directory)"}}}`),
	"file-read":          json.RawMessage(`{"type":"object","properties":{"path":{"type":"string","description":"file path, relative to the working directory"},"offset":{"type":"integer","description":"1-based line to start reading from (omit to read from the start)"},"limit":{"type":"integer","description":"number of lines to read (default 2000)"},"start_line":{"type":"integer","description":"first line to return, 1-based (alternative to offset)"},"end_line":{"type":"integer","description":"last line to return, inclusive (alternative to limit)"}},"required":["path"]}`),
	"file-write":         json.RawMessage(`{"type":"object","properties":{"path":{"type":"string","description":"file path, relative to the working directory"},"content":{"type":"string","description":"the complete file content (or the text to add, with append)"},"append":{"type":"boolean","description":"append content to the end of the file instead of replacing it"}},"required":["path","content"]}`),
	"file-edit":          json.RawMessage(`{"type":"object","properties":{"path":{"type":"string","description":"file path, relative to the working directory"},"old_string":{"type":"string","description":"exact text to replace, copied verbatim from the file (no line-number prefixes); must be unique unless replace_all"},"new_string":{"type":"string","description":"replacement text (empty to delete)"},"replace_all":{"type":"boolean","description":"replace every occurrence of old_string"},"start_line":{"type":"integer","description":"only search from this line (1-based)"},"end_line":{"type":"integer","description":"only search up to this line (inclusive)"},"expected_replacements":{"type":"integer","description":"fail unless exactly this many replacements are made"},"edits":{"type":"array","description":"several replacements applied in order, all or nothing (instead of old_string/new_string)","items":{"type":"object","properties":{"old_string":{"type":"string"},"new_string":{"type":"string"},"replace_all":{"type":"boolean"}},"required":["old_string","new_string"]}}},"required":["path"]}`),
	"multi-edit":         json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"},"edits":{"type":"array","minItems":1,"items":{"type":"object","properties":{"old_string":{"type":"string"},"new_string":{"type":"string"},"replace_all":{"type":"boolean"}},"required":["old_string","new_string"]}}},"required":["path","edits"]}`),
	"glob":               json.RawMessage(`{"type":"object","properties":{"pattern":{"type":"string","description":"glob matched against paths relative to the working directory (or to path), e.g. **/*.go or src/**/*.{ts,tsx}"},"path":{"type":"string","description":"subdirectory to limit the walk to (default: the working directory)"}},"required":["pattern"]}`),
	"grep":               json.RawMessage(`{"type":"object","properties":{"pattern":{"type":"string","description":"regular expression (Go RE2 syntax)"},"path":{"type":"string","description":"directory or file to search, relative to the working directory (default: the whole working directory)"},"glob":{"type":"string","description":"only search matching files: a file-name pattern such as *.go, or a path pattern containing / such as internal/**/*.go"},"type":{"type":"string","description":"only search files of this type: go, ts, js, py, rs, md, toml, json, yaml, sh"},"case_insensitive":{"type":"boolean","description":"match case-insensitively"},"context":{"type":"integer","description":"lines of context to show before and after each match (content mode)"},"output_mode":{"type":"string","enum":["content","files_with_matches","count"],"description":"content (default): matching lines; files_with_matches: paths only; count: matches per file"},"max_results":{"type":"integer","description":"content: stop after this many matches; files_with_matches/count: list at most this many files (default 200)"}},"required":["pattern"]}`),
	"repo-search":        json.RawMessage(`{"type":"object","properties":{"query":{"type":"string","description":"identifier or literal text to find (case-insensitive); empty lists all files"}},"required":["query"]}`),
	"shell-exec":         json.RawMessage(`{"type":"object","properties":{"command":{"type":"string","description":"the command line to run"},"timeout":{"type":"integer","description":"seconds before a foreground command's process group is killed (default as configured, max 600); raise it for slow builds, installs and test suites (ignored with run_in_background)"},"run_in_background":{"type":"boolean","description":"start as a background job and return its ID immediately; for servers and watchers"}},"required":["command"]}`),
	"bash":               json.RawMessage(`{"type":"object","properties":{"command":{"type":"string","description":"the command line to run"},"timeout":{"type":"integer","description":"seconds before a foreground command's process group is killed (default as configured, max 600); raise it for slow builds, installs and test suites (ignored with run_in_background)"},"run_in_background":{"type":"boolean","description":"start as a background job and return its ID immediately; for servers and watchers"}},"required":["command"]}`),
	"bash-output":        json.RawMessage(`{"type":"object","properties":{"command":{"type":"string"},"timeout":{"type":"integer","description":"seconds before the command is killed (default as configured, max 600)"},"run_in_background":{"type":"boolean"},"job_id":{"type":"string"},"offset":{"type":"number"}}}`),
	"job-output":         json.RawMessage(`{"type":"object","properties":{"job_id":{"type":"string"},"offset":{"type":"integer"}},"required":["job_id"]}`),
	"job-kill":           json.RawMessage(`{"type":"object","properties":{"job_id":{"type":"string"}},"required":["job_id"]}`),
	"tool-output":        json.RawMessage(`{"type":"object","properties":{"id":{"type":"string"},"offset":{"type":"integer"},"limit":{"type":"integer"}},"required":["id"]}`),
	"pty-start":          json.RawMessage(`{"type":"object","properties":{"command":{"type":"string"},"cols":{"type":"integer"},"rows":{"type":"integer"}},"required":["command"]}`),
	"pty-write":          json.RawMessage(`{"type":"object","properties":{"id":{"type":"string"},"input":{"type":"string"},"submit":{"type":"boolean","description":"append \\r to submit the input as a line"},"wait_for":{"type":"string","description":"return as soon as this literal string appears in new output (default timeout 10s)"},"wait_ms":{"type":"integer"}},"required":["id"]}`),
	"pty-kill":           json.RawMessage(`{"type":"object","properties":{"id":{"type":"string"}},"required":["id"]}`),
	"web-fetch":          json.RawMessage(`{"type":"object","properties":{"url":{"type":"string"},"max_length":{"type":"integer"}},"required":["url"]}`),
	"download":           json.RawMessage(`{"type":"object","properties":{"url":{"type":"string"},"path":{"type":"string"},"max_bytes":{"type":"integer"}},"required":["url","path"]}`),
	"web-search":         json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"},"max_results":{"type":"integer"}},"required":["query"]}`),
	"ask-user":           json.RawMessage(`{"type":"object","properties":{"questions":{"type":"array","maxItems":4,"description":"the form: up to 4 questions answered in one interaction","items":{"type":"object","properties":{"header":{"type":"string","description":"short tab label, e.g. \"Focus area\"; must be unique within the form and keys the answer"},"question":{"type":"string","description":"the full question line"},"options":{"type":"array","maxItems":8,"description":"selectable answers; prefer these over an open question","items":{"type":"object","properties":{"label":{"type":"string","description":"the answer as the user reads it"},"description":{"type":"string","description":"one muted line under the label saying what choosing it means"},"preview":{"type":"string","description":"preformatted content (snippet, layout, config) shown beside the option list; kept verbatim, so long lines are clipped rather than wrapped — keep it narrow"},"is_recommended":{"type":"boolean","description":"the answer you would pick; highlighted and pre-selected"}},"required":["label"]}},"multi_select":{"type":"boolean","description":"several answers may be chosen at once; any subset can come back, so phrase the options so every combination of them means something"},"allow_custom":{"type":"boolean","description":"also offer a free-text entry; the typed answer is returned verbatim"}},"required":["question"]}},"context":{"type":"string","description":"one line of background applying to the whole form"},"question":{"type":"string","description":"legacy single-question form; use questions[] instead"},"options":{"type":"array","items":{"type":"string"},"description":"legacy: option labels for the single question"},"default_option":{"type":"string","description":"legacy: the recommended option, matched by label"},"allow_free_response":{"type":"boolean","description":"legacy: allow_custom for the single question"}}}`),
	"agent":              json.RawMessage(`{"type":"object","properties":{"agent":{"type":"string"},"task":{"type":"string"},"constraints":{"type":"string"},"expected_output":{"type":"string"},"parent_agent_id":{"type":"string"},"isolation":{"type":"string","enum":["worktree"],"description":"run the sub-agent in its own git worktree/branch, auto-merged back when it finishes"}},"required":["agent","task"]}`),
	"ultra":              json.RawMessage(`{"type":"object","properties":{"description":{"type":"string","description":"short summary of the overall fan-out"},"prompt_template":{"type":"string","description":"task template containing the {{item}} placeholder"},"items":{"type":"array","minItems":2,"maxItems":32,"items":{"type":"string"}},"subagent_type":{"type":"string","description":"worker agent id to run (default: code)"},"isolation":{"type":"string","enum":["worktree"],"description":"give every sub-agent its own git worktree/branch, auto-merged back after the swarm completes; use when sub-agents edit files"}},"required":["description","prompt_template","items"]}`),
	"workflow":           json.RawMessage(`{"type":"object","properties":{"script":{"type":"string","description":"inline workflow source, starting with the export const meta header"},"script_path":{"type":"string","description":"path to a workflow script to run instead of inline source (use to re-run an edited script)"},"name":{"type":"string","description":"name of a saved workflow in .spettro/workflows or ~/.spettro/workflows"},"args":{"description":"any JSON value, exposed to the script as the args global"},"resume_from_run_id":{"type":"string","description":"run id of a prior workflow run; unchanged agent calls replay from its journal instead of re-running"},"max_concurrency":{"type":"integer","description":"cap on simultaneous agents (default: one less than the machine allows, at most 16)"},"budget_tokens":{"type":"integer","description":"token target exposed to the script as budget.total; agent() throws once it is reached"},"save_as":{"type":"string","description":"also save this script as a reusable workflow under this name"},"save_scope":{"type":"string","enum":["project","global"],"description":"where save_as writes: the project's .spettro/workflows (default) or ~/.spettro/workflows"}}}`),
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
