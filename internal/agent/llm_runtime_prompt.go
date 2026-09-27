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
		return fileReadDefaultChars
	case "grep", "glob", "lsp":
		return 16000
	case "bash":
		// Build and test logs are what the model iterates on; the failures
		// usually sit at the end, which spoolResult keeps (head + tail).
		return shellOutputHistoryLimit
	case "job-output", "tool-output", "pty-start", "pty-write":
		return 8000
	case "todo-write":
		// Every call returns the whole task list.
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
	case "bash":
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
			Symbol  string `json:"symbol"`
		}
		if json.Unmarshal([]byte(args), &payload) == nil {
			if payload.Symbol != "" {
				return "symbol=" + truncate(payload.Symbol, 120)
			}
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
	return buildSystemStringWith(cfg, sessionContextFor(cfg))
}

// buildSystemStringWith is buildSystemString with the session snapshot
// already taken.
func buildSystemStringWith(cfg toolLoopConfig, sessionCtx string) string {
	base := strings.TrimSpace(cfg.SystemPrompt)
	if base == "" {
		base = "You are an assistant."
	}
	if catalog := skills.CatalogPrompt(cfg.SkillsCatalog); catalog != "" {
		base = base + catalog
	}
	return base + cfg.toolSurfaceNote + sessionCtx
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
		"- Shell for the bash tool: " + shell.Describe(),
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

// shellToolDesc describes the bash tool (shell-exec is a retired alias of
// it). Whatever its name, it runs the host shell, which the description and
// the environment section both state, so a PowerShell host is not mistaken
// for bash.
const shellToolDesc = "Run a command with the host shell (see Environment) in the working directory; returns stdout+stderr, then `[exit status N]` on failure. Each call is a fresh process: cd and variables don't carry over, so chain dependent steps in one command (e.g. `cd web && npm test`). Don't use it to read, search or list files: use file-read, grep and glob. Pass timeout (seconds, max 600) for slow builds and test suites; on timeout the process group is killed and partial output returned. run_in_background (servers, watchers) returns a job ID for job-output/job-kill. No interactive commands: pass non-interactive flags or use pty-start. Output over about 30,000 characters keeps the head and tail; page the rest with tool-output."

// shellToolDescFor is shellToolDesc in the host shell's dialect: a
// PowerShell host gets a chaining example that parses there (Windows
// PowerShell 5.1 has no &&).
func shellToolDescFor(kind shell.Kind) string {
	if kind == shell.KindPowerShell {
		return strings.Replace(shellToolDesc, "(e.g. `cd web && npm test`)", "(e.g. `Set-Location web; npm test` — Windows PowerShell 5.1 has no `&&`)", 1)
	}
	return shellToolDesc
}

// shellReadCommand names a read-only command for reading part of a file
// outside the workspace (where file-read cannot go) in the host dialect.
func shellReadCommand(kind shell.Kind) string {
	switch kind {
	case shell.KindPowerShell:
		return "a read-only shell command such as Get-Content (with -TotalCount, or piped to Select-Object -Skip/-First)"
	case shell.KindCmd:
		return "a read-only shell command such as type or more"
	}
	return "a read-only shell command such as sed -n"
}

// toolDescription is a built-in tool's description for this host.
func toolDescription(name string) (string, bool) {
	if name == "bash" {
		return shellToolDescFor(shell.Dialect()), true
	}
	desc, ok := builtinNativeToolDescs[name]
	return desc, ok
}

// builtinNativeToolDescs and builtinNativeToolSchemas define the description and
// real JSON Schema for each built-in tool on the native tool-calling path.
var builtinNativeToolDescs = map[string]string{
	"comment":            "Optional: show a short progress note to the user. It does not advance the task, so never spend a step on it alone; add it next to real tool calls, and only for long-running work worth announcing.",
	"file-read":          "Read a text file (path relative to the working directory). Each line comes back cat -n style: its 1-based line number, a tab, then the text (`     7\tcode`); the prefix is display only, never copy it into old_string or file content. Returns up to 2000 lines and about 60,000 characters; the footer gives the offset to continue from. For part of a file pass offset/limit or start_line/end_line. Long lines are clipped; binary files are refused. Read a file before editing or overwriting it.",
	"file-write":         "Create a file, or overwrite one with the full new content; parent directories are created, and append=true adds to the end instead. Overwriting an existing file is refused unless you read it with file-read (a grep hit does not count) and it has not changed since. For changes to an existing file prefer file-edit. Don't create files the task doesn't need; never write secrets.",
	"file-edit":          "Replace exact text in an existing file; read the file first (the edit is refused if it changed since your last read, your own shell commands such as a formatter included: re-read and retry). old_string must match byte for byte, indentation included, without the line-number prefix file-read shows, and identify exactly one location: add surrounding lines to make it unique, or set replace_all=true to change every occurrence. new_string replaces it (empty deletes it). For several changes to one file pass edits[] (applied in order, all or nothing). Cannot create files. Returns a short diff.",
	"glob":               "Find files by path pattern, matched against the path relative to the working directory (or to path): * within one segment, ** across directories, {a,b} alternatives, e.g. `**/*.go` or `src/**/*.{ts,tsx}`; `*.go` matches only top-level files. Returns sorted paths, at most 1000; honours .gitignore and skips .git, .spettro, vendor, node_modules, dist and build. Without a pattern it lists the immediate entries of path (directories end in /).",
	"grep":               "Search file contents with a regular expression (Go RE2: no lookarounds or backreferences) in the working directory, or only in path. Filter files with glob (unlike the glob tool, one without / such as `*.go` matches file names at any depth; one with / such as `internal/**/*.go` matches the path) or type. output_mode: content (default, `path:line: text`), files_with_matches or count; results stop at max_results (default 200). Honours .gitignore; skips binary files and .git, .spettro, vendor, node_modules, dist and build unless named as path. For an identifier, pass symbol instead of pattern: a case-insensitive literal lookup over the whole working directory, ranked definitions first, then usages (path, glob, type and max_results don't apply).",
	"bash":               shellToolDesc,
	"job-output":         "Fetch accumulated stdout/stderr of a background job (job-N). Pass the next_offset from the previous call to read incrementally.",
	"job-kill":           "Terminate a background job by ID.",
	"tool-output":        "Re-read the full output of an earlier tool call that was cut or offloaded to disk (truncation footers and stubs name it: [truncated: … use tool-output {\"id\":\"spool:N\",\"offset\":Z} …], [output elided: … tool-output {\"id\":\"spool:N\"}]). Page with offset/limit; pass the next_offset from the previous call to continue.",
	"pty-start":          "Start an interactive terminal session (REPL, debugger, ssh, watch-mode server) under a pseudo-terminal. Returns a session ID plus the initial screen; drive it with pty-write.",
	"pty-write":          "Send input to a pty session and return output produced since the last read. Backslash escapes in input are decoded server-side (\\r \\n \\t \\e \\xHH \\uHHHH; \\\\ for a literal backslash), so {\"input\":\"2+2\",\"submit\":true} runs a REPL line and {\"input\":\"\\x03\"} sends Ctrl-C. submit:true appends \\r. Prefer wait_for (return as soon as this literal string, e.g. the prompt \">>> \", appears in new output) over guessing wait_ms. Empty input just polls.",
	"pty-kill":           "Terminate a pty session (SIGTERM, then SIGKILL) and free it.",
	"web-fetch":          "Fetch a URL and return its content as readable text/markdown (truncated to a size budget). For binary files use the download tool.",
	"download":           "Download a URL to a file inside the workspace, subject to a maximum size limit.",
	"web-search":         "Search the web.",
	"ask-user":           "Ask the user up to 4 related questions as one form and wait for the answers. Use it only when a decision is genuinely the user's and a wrong guess would waste work, never for what the code can tell you; put questions about one decision in one call. Each question has a short header (it keys the answer), the question line and up to 8 options: a label and a one-line description each, is_recommended on the one you would pick, preview for a snippet worth showing beside it. multi_select lets any subset come back, so phrase options so every combination makes sense; allow_custom adds a free-text answer, returned verbatim. Answers come back one line per header. A skipped question is marked unanswered: never read that as agreement (a multi-select answered with none of the options is a real answer). A user note follows its answer as `— note: \"...\"`.",
	"agent":              "Delegate a task to a named sub-agent. Set isolation to \"worktree\" when the sub-agent will edit files and you want it sandboxed from the main checkout: it then runs in its own git worktree on a branch named after it (under .spettro/worktrees/), which is merged back and deleted automatically when it finishes; a merge conflict keeps the branch and worktree for manual resolution. A sub-agent that runs out of time or fails returns status \"timed_out\" or \"failed\" with partial: true, its last message, files_modified and its last tool results; its changes are kept, so continue from them instead of redoing the task.",
	"ultra":              "Fan a task out across many parallel sub-agents (2-32). prompt_template must contain {{item}}; each item fills the template into one self-contained sub-agent task. Sub-agents cannot see your context or each other, so include file paths, constraints, and expected output in the template. Give every item a distinct, non-overlapping scope; never let two agents touch the same file. Results are returned in input order. When sub-agents edit files, set isolation to \"worktree\": each gets its own git worktree and branch under .spettro/worktrees/, and all branches are merged back into the main checkout and deleted after the swarm completes (conflicting branches are kept and reported for manual resolution).",
	"workflow":           "Run a deterministic multi-agent orchestration script you write. The script is JavaScript: `export const meta = {name, description, phases}` followed by a body that uses agent(prompt, opts) to run a sub-agent (returns its final text, or the parsed object when opts.schema is a JSON Schema, or null if it failed), parallel(thunks) to run several concurrently and wait for all of them, pipeline(items, ...stages) to push every item through every stage with no barrier between stages, phase(title) and log(message) for progress, args for the value you passed in, budget.remaining() for the token target, and workflow(name, args) to call a saved workflow. The body runs in an async context, so use await and a top-level return. Prefer pipeline over parallel-then-parallel: only use a barrier when a stage genuinely needs every previous result at once. Pass script for inline source, script_path to re-run an edited script, or name to run a saved workflow from .spettro/workflows. Set save_as to also store the script under that name for reuse (save_scope \"global\" puts it in ~/.spettro/workflows instead of the project). Unless the user wrote \"ultracode\", they are asked to confirm before the run starts and may choose to keep the script without running it. Date.now(), Math.random() and argless new Date() are unavailable (they would break resume).",
	"save-memory":        "Save one short durable fact or user preference to persistent memory; it is loaded into context in future sessions. Use scope \"project\" for facts specific to this repository.",
	"todo-write":         "Read and update the session task list, which the user sees as a live checklist. For work of 3+ steps, write the plan up front and keep it current: in_progress when you start a task, completed as soon as it is done. todos replaces the whole list; with merge=true it inserts or updates only the given tasks by id. delete removes tasks by id; clear_completed prunes completed and cancelled ones; call with no arguments to read the list. dependencies lists task ids that must complete first: cycles are rejected, unknown ids are dropped with a note, and a merged task cannot start or complete before them. Every call returns the full list, each task with blocked_by and ready.",
	"task-stop":          "Stop the current task.",
	"goal-complete":      "Declare the goal fully achieved and verified; ends the run. Only call after you have confirmed the objective is met (tests pass / build green / change applied).",
	"tool-search":        "Find and load tools you hold whose schemas are not loaded yet (the system prompt lists them under More tools). Pass tool names (comma-separated) to load exactly those, or a keyword such as \"image\" or \"memory\": each matching unloaded tool comes back with its description and parameter schema, and you can call it from your next step on. An empty query lists every tool you hold.",
	"skill":              "Load an Agent Skill: returns its instructions and the directory holding its bundled files (read those with file-read only when the instructions point to them). name is a skill from the system prompt's skill list; args, when given, fill the skill's $ARGUMENTS placeholders. Omit name to list every skill you may load (query filters by name or description).",
	"config":             "Get or set configuration values.",
	"lsp":                "Query the language server (read-only; to rename a symbol use rename-symbol). op picks the operation:\n- diagnostics: current diagnostics (errors, warnings, info and hints) for path, or for every file seen so far this session when path is omitted.\n- references: every reference to a symbol (declaration included), as path:line:col.\n- definition: where a symbol is defined, as path:line:col.\n- hover: a symbol's type signature and documentation.\n- restart: restart a wedged language server (server names one; all of them when omitted) and reload .spettro/lsp.json.\nreferences, definition and hover need path plus the position: a symbol name (its first occurrence in the file) or a 1-based line (and optional character).",
	"rename-symbol":      "Language-server rename: rename a symbol across the workspace and apply the edits. Position by symbol name or 1-based line/character; reports the files changed.",
	"enter-plan-mode":    "Enter plan mode.",
	"exit-plan-mode":     "Exit plan mode.",
	"enter-worktree":     "Enter an isolated git worktree.",
	"exit-worktree":      "Exit the current worktree.",
	"send-message":       "Send a message to another agent.",
	"mcp-list-resources": "List resources exposed by an MCP server.",
	"mcp-read-resource":  "Read an MCP resource.",
	"mcp-auth":           "Authenticate with an MCP server.",
	"view-image":         "Attach an image file from the workspace so you can SEE it (vision models). Combine with the shell tools to inspect anything visually: capture a page yourself (e.g. `chromium --headless --screenshot=shot.png <url>` or `npx playwright screenshot <url> shot.png`), then view the file — no need to ask the user for screenshots.",
}

var builtinNativeToolSchemas = map[string]json.RawMessage{
	"comment":            json.RawMessage(`{"type":"object","properties":{"message":{"type":"string","description":"one short line shown to the user"}},"required":["message"]}`),
	"file-read":          json.RawMessage(`{"type":"object","properties":{"path":{"type":"string","description":"file path"},"offset":{"type":"integer","description":"1-based first line"},"limit":{"type":"integer","description":"number of lines (default 2000)"},"start_line":{"type":"integer","description":"first line, 1-based (alternative to offset)"},"end_line":{"type":"integer","description":"last line, inclusive (alternative to limit)"}},"required":["path"]}`),
	"file-write":         json.RawMessage(`{"type":"object","properties":{"path":{"type":"string","description":"file path"},"content":{"type":"string","description":"the complete file content (or the text to add, with append)"},"append":{"type":"boolean","description":"append instead of replacing"}},"required":["path","content"]}`),
	"file-edit":          json.RawMessage(`{"type":"object","properties":{"path":{"type":"string","description":"file path"},"old_string":{"type":"string","description":"exact text to replace; unique unless replace_all"},"new_string":{"type":"string","description":"replacement text (empty to delete)"},"replace_all":{"type":"boolean","description":"replace every occurrence"},"start_line":{"type":"integer","description":"only search from this line (1-based)"},"end_line":{"type":"integer","description":"only search up to this line (inclusive)"},"expected_replacements":{"type":"integer","description":"fail unless exactly this many replacements happen; with old_string alone, above 1 replaces every occurrence"},"edits":{"type":"array","description":"several replacements, applied in order, all or nothing (instead of old_string/new_string)","items":{"type":"object","properties":{"old_string":{"type":"string"},"new_string":{"type":"string"},"replace_all":{"type":"boolean"},"expected_replacements":{"type":"integer","description":"fail unless this edit makes exactly this many replacements"}},"required":["old_string","new_string"]}}},"required":["path"]}`),
	"glob":               json.RawMessage(`{"type":"object","properties":{"pattern":{"type":"string","description":"glob against the path, e.g. **/*.go (any depth; *.go is top level only); omit to list path"},"path":{"type":"string","description":"directory to search or list (default: working directory)"}}}`),
	"grep":               json.RawMessage(`{"type":"object","properties":{"pattern":{"type":"string","description":"RE2 regular expression; required unless symbol is given"},"symbol":{"type":"string","description":"identifier to look up instead of a regex (case-insensitive literal; searches the whole working directory, other options ignored)"},"path":{"type":"string","description":"directory or file to search (default: working directory)"},"glob":{"type":"string","description":"*.go matches file names at any depth; a pattern with / such as internal/**/*.go matches the path"},"type":{"type":"string","description":"file type: go, ts, js, py, rs, md, toml, json, yaml, sh"},"case_insensitive":{"type":"boolean","description":"ignore case"},"context":{"type":"integer","description":"context lines around each match (content mode)"},"output_mode":{"type":"string","enum":["content","files_with_matches","count"],"description":"default content"},"max_results":{"type":"integer","description":"maximum matches (content) or files (default 200)"}}}`),
	"bash":               json.RawMessage(`{"type":"object","properties":{"command":{"type":"string","description":"the command line to run"},"timeout":{"type":"integer","description":"seconds before the command is killed (max 600); ignored with run_in_background"},"run_in_background":{"type":"boolean","description":"start as a background job; returns its ID"},"cwd":{"type":"string","description":"directory to run in (default: working directory)"}},"required":["command"]}`),
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
	"todo-write":         json.RawMessage(`{"type":"object","properties":{"todos":{"type":"array","description":"the whole task list, or with merge only the tasks to add or change","items":{"type":"object","properties":{"id":{"type":"string","description":"stable task id; omit on a new task to get the next task-N"},"content":{"type":"string","description":"what the task is; required for a new task"},"status":{"type":"string","enum":["pending","in_progress","completed","blocked","cancelled"]},"owner":{"type":"string"},"source":{"type":"string"},"priority":{"type":"string"},"dependencies":{"type":"array","items":{"type":"string"},"description":"ids of tasks that must complete first"}}}},"merge":{"type":"boolean","description":"insert or update the given tasks by id instead of replacing the whole list"},"delete":{"type":"array","items":{"type":"string"},"description":"ids of tasks to remove"},"clear_completed":{"type":"boolean","description":"remove every completed and cancelled task"}}}`),
	"task-stop":          json.RawMessage(`{"type":"object","properties":{"reason":{"type":"string"}}}`),
	"goal-complete":      json.RawMessage(`{"type":"object","properties":{"summary":{"type":"string"},"verified":{"type":"boolean"}},"required":["summary"]}`),
	"tool-search":        json.RawMessage(`{"type":"object","properties":{"query":{"type":"string","description":"tool names separated by commas (e.g. \"view-image, web-search\"), or a keyword; empty lists every tool"}},"required":["query"]}`),
	"skill":              json.RawMessage(`{"type":"object","properties":{"name":{"type":"string","description":"the skill to load; omit to list skills"},"args":{"type":"string","description":"optional arguments for the skill, as a user would type them after /name"},"query":{"type":"string","description":"with no name: only list skills whose name or description contains this"}}}`),
	"config":             json.RawMessage(`{"type":"object","properties":{"action":{"type":"string","enum":["get","set"]},"key":{"type":"string"},"value":{"type":"string"},"force":{"type":"boolean"}},"required":["action"]}`),
	"lsp":                json.RawMessage(`{"type":"object","properties":{"op":{"type":"string","enum":["diagnostics","references","definition","hover","restart"],"description":"the operation"},"path":{"type":"string","description":"file path, relative to the working directory; required for references, definition and hover, optional for diagnostics (omit for every file seen so far)"},"symbol":{"type":"string","description":"references/definition/hover: identifier whose first occurrence in path is the position (instead of line)"},"line":{"type":"integer","description":"references/definition/hover: 1-based line of the position"},"character":{"type":"integer","description":"references/definition/hover: 1-based column on line"},"server":{"type":"string","description":"restart: the server to restart (omit for all)"}},"required":["op"]}`),
	"rename-symbol":      json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"},"new_name":{"type":"string"},"symbol":{"type":"string"},"line":{"type":"integer"},"character":{"type":"integer"}},"required":["path","new_name"]}`),
	"enter-plan-mode":    json.RawMessage(`{"type":"object","properties":{"reason":{"type":"string"}}}`),
	"exit-plan-mode":     json.RawMessage(`{"type":"object","properties":{"reason":{"type":"string"}}}`),
	"enter-worktree":     json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"},"branch":{"type":"string"},"allow_dirty":{"type":"boolean"}}}`),
	"exit-worktree":      json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"},"force":{"type":"boolean"}},"required":["path"]}`),
	"send-message":       json.RawMessage(`{"type":"object","properties":{"target":{"type":"string"},"message":{"type":"string"}},"required":["message"]}`),
	"mcp-list-resources": json.RawMessage(`{"type":"object","properties":{"server_id":{"type":"string"}},"required":["server_id"]}`),
	"mcp-read-resource":  json.RawMessage(`{"type":"object","properties":{"server_id":{"type":"string"},"resource_id":{"type":"string"}},"required":["server_id","resource_id"]}`),
	"mcp-auth":           json.RawMessage(`{"type":"object","properties":{"server_id":{"type":"string"},"token":{"type":"string"},"scope":{"type":"string"},"expires_at":{"type":"string"},"description":{"type":"string"}},"required":["server_id"]}`),
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
		desc, hasDesc := toolDescription(name)
		schema, hasSchema := builtinNativeToolSchemas[name]
		if !hasDesc || !hasSchema {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, provider.ToolSpec{Name: name, Description: desc, Schema: schema})
	}
	return out
}
