package acp

// Tool-call presentation: how an agent.ToolTrace becomes the fields of an
// ACP tool_call / tool_call_update notification (kind, title, locations,
// rawInput, content) and how those fields are kept to a size an editor can
// render.
//
// Size bounds. A model can put megabytes into one call (file-write of a
// generated file, a bash heredoc), and the notification that announces the
// call would carry all of it twice (title and rawInput). Editors render these
// cards eagerly, so every field is clipped here:
//
//   - title: one line, at most maxTitleRunes runes;
//   - rawInput: each string value at most maxRawInputString bytes, the whole
//     object at most maxRawInputBytes once encoded, secrets redacted;
//   - text output: at most maxToolTextBytes bytes (the runtime already cuts
//     trace output to a few hundred bytes; this is the backstop);
//   - diffs: the runtime drops the texts of changes to very large files
//     (agent.FileChange.TextOmitted), and one update carries at most
//     maxDiffBytesPerUpdate bytes of diff text; changes past that are named
//     in a note instead.
//
// Clipping is always rune-safe, so no notification carries broken UTF-8.

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	acpsdk "github.com/coder/acp-go-sdk"

	"spettro/internal/agent"
)

const (
	maxTitleRunes         = 120
	maxRawInputString     = 2 << 10
	maxRawInputBytes      = 16 << 10
	maxToolTextBytes      = 16 << 10
	maxDiffBytesPerUpdate = 512 << 10
)

// builtinToolKinds maps every canonical built-in tool to its ACP tool kind,
// which editors use to pick an icon and, for read/edit, to follow the agent
// across files. Retired names (shell-exec, multi-edit, task-*, skill-read,
// diagnostics, ...) are looked up by their canonical tool, see toolKind.
//
// The kinds, per the ACP spec: read (reading files or data), edit
// (modifying files or content), delete, move, search (searching for
// information), execute (running commands or code), think (internal
// reasoning or planning), fetch (retrieving external data), switch_mode, and
// other.
var builtinToolKinds = map[string]acpsdk.ToolKind{
	// Files.
	"file-read":     acpsdk.ToolKindRead,
	"view-image":    acpsdk.ToolKindRead,
	"file-write":    acpsdk.ToolKindEdit,
	"file-edit":     acpsdk.ToolKindEdit,
	"rename-symbol": acpsdk.ToolKindEdit,
	// Search.
	"glob":        acpsdk.ToolKindSearch,
	"grep":        acpsdk.ToolKindSearch,
	"lsp":         acpsdk.ToolKindSearch,
	"tool-search": acpsdk.ToolKindSearch,
	// Commands, background jobs and terminals.
	"bash":        acpsdk.ToolKindExecute,
	"job-kill":    acpsdk.ToolKindExecute,
	"pty-start":   acpsdk.ToolKindExecute,
	"pty-write":   acpsdk.ToolKindExecute,
	"pty-kill":    acpsdk.ToolKindExecute,
	"job-output":  acpsdk.ToolKindRead,
	"tool-output": acpsdk.ToolKindRead,
	// The network.
	"web-fetch":  acpsdk.ToolKindFetch,
	"web-search": acpsdk.ToolKindFetch,
	"download":   acpsdk.ToolKindFetch,
	// Planning, delegation and the agent's own bookkeeping.
	"todo-write":        acpsdk.ToolKindThink,
	"agent":             acpsdk.ToolKindThink,
	"ultra":             acpsdk.ToolKindThink,
	"workflow":          acpsdk.ToolKindThink,
	"workflow-progress": acpsdk.ToolKindThink,
	"goal-complete":     acpsdk.ToolKindThink,
	"skill":             acpsdk.ToolKindRead,
	"enter-plan-mode":   acpsdk.ToolKindSwitchMode,
	"exit-plan-mode":    acpsdk.ToolKindSwitchMode,
	// MCP resources.
	"mcp-list-resources": acpsdk.ToolKindRead,
	"mcp-read-resource":  acpsdk.ToolKindRead,
	// Everything else (ask-user, config, save-memory, send-message,
	// task-stop, mcp-auth, enter-worktree, exit-worktree) is "other".
}

// toolKind classifies a tool name into an ACP tool kind. Built-ins, under
// their canonical or a retired name, come from builtinToolKinds; any other
// name (an MCP or manifest tool) is guessed from words in the name.
//
// It knows names only; a tool of the operator's own that takes a built-in's
// name is told apart by turnState.cardKind, which has the session's
// manifest.
func toolKind(name string) acpsdk.ToolKind {
	canonical := agent.CanonicalToolName(strings.ToLower(strings.TrimSpace(name)))
	if kind, ok := builtinToolKinds[canonical]; ok {
		return kind
	}
	if _, ok := builtinToolTitles[canonical]; ok {
		// A built-in with no specific kind.
		return acpsdk.ToolKindOther
	}
	return guessToolKind(canonical)
}

// guessToolKind classifies a tool that is not a built-in by the words in its
// name, most specific first ("read_file" is a read, "write_file" an edit).
func guessToolKind(name string) acpsdk.ToolKind {
	has := func(words ...string) bool {
		for _, w := range words {
			if strings.Contains(name, w) {
				return true
			}
		}
		return false
	}
	switch {
	case has("edit", "write", "patch", "replace", "create", "update"):
		return acpsdk.ToolKindEdit
	case has("delete", "remove"):
		return acpsdk.ToolKindDelete
	case has("move", "rename"):
		return acpsdk.ToolKindMove
	case has("search", "grep", "glob", "find", "query"):
		return acpsdk.ToolKindSearch
	case has("read", "get", "list", "view", "show"):
		return acpsdk.ToolKindRead
	case has("shell", "bash", "exec", "run", "command"):
		return acpsdk.ToolKindExecute
	case has("http", "fetch", "web", "url", "download"):
		return acpsdk.ToolKindFetch
	case has("think", "plan", "todo"):
		return acpsdk.ToolKindThink
	}
	return acpsdk.ToolKindOther
}

// toolArgs is a call's JSON arguments, decoded, with typed accessors that
// return the zero value for a missing or differently typed field.
type toolArgs map[string]any

func decodeToolArgs(raw string) toolArgs {
	var m map[string]any
	if json.Unmarshal([]byte(raw), &m) != nil {
		return nil
	}
	return m
}

func (a toolArgs) str(key string) string {
	s, _ := a[key].(string)
	return strings.TrimSpace(s)
}

func (a toolArgs) flag(key string) bool {
	b, _ := a[key].(bool)
	return b
}

// firstStr returns the first non-empty string among keys.
func (a toolArgs) firstStr(keys ...string) string {
	for _, k := range keys {
		if s := a.str(k); s != "" {
			return s
		}
	}
	return ""
}

// commandArg is the command of a bash or pty-start call. bash accepts
// Codex's "cmd" as an alias for "command" (shellToolArgs in internal/agent),
// and a trace carries the arguments exactly as the model wrote them, so both
// names are read.
func (a toolArgs) commandArg() string {
	return a.firstStr("command", "cmd")
}

// pathArg is the file of a file-read, file-write or file-edit call. The
// runtime accepts Claude Code's "file_path" as an alias for "path" (see
// decodeFileWriteArgs and friends in internal/agent), so both names are read.
func (a toolArgs) pathArg() string {
	return a.firstStr("path", "file_path")
}

// builtinToolTitles renders the card title of each built-in from its
// arguments. A function returning "" falls back to the generic
// "<name> <args>" title (see toolCallTitle).
var builtinToolTitles = map[string]func(toolArgs) string{
	"bash": func(a toolArgs) string {
		if a.flag("run_in_background") {
			return "Run in background: " + a.commandArg()
		}
		return "Run " + a.commandArg()
	},
	"file-read":  func(a toolArgs) string { return "Read " + a.pathArg() },
	"view-image": func(a toolArgs) string { return "View image " + a.str("path") },
	"file-write": func(a toolArgs) string {
		if a.flag("append") {
			return "Append to " + a.pathArg()
		}
		return "Write " + a.pathArg()
	},
	"file-edit": func(a toolArgs) string { return "Edit " + a.pathArg() },
	"rename-symbol": func(a toolArgs) string {
		title := "Rename"
		if s := a.str("symbol"); s != "" {
			title += " " + s
		}
		return title + " to " + a.str("new_name") + " in " + a.str("path")
	},
	"glob": func(a toolArgs) string {
		dir := a.str("path")
		if dir == "" {
			dir = "."
		}
		if p := a.str("pattern"); p != "" {
			return "Find " + p + " in " + dir
		}
		return "List " + dir
	},
	"grep": func(a toolArgs) string {
		if s := a.str("symbol"); s != "" {
			return "Find symbol " + s
		}
		title := "Search " + a.str("pattern")
		if dir := a.str("path"); dir != "" {
			title += " in " + dir
		}
		return title
	},
	"lsp": func(a toolArgs) string {
		title := "LSP " + a.str("op")
		if target := a.firstStr("symbol", "path"); target != "" {
			title += " " + target
		}
		return title
	},
	"tool-search": func(a toolArgs) string { return "Search tools: " + a.str("query") },
	"job-output":  func(a toolArgs) string { return "Read output of job " + a.str("job_id") },
	"job-kill":    func(a toolArgs) string { return "Stop job " + a.str("job_id") },
	"tool-output": func(a toolArgs) string { return "Read full output of " + a.str("id") },
	"pty-start":   func(a toolArgs) string { return "Start terminal: " + a.str("command") },
	"pty-write":   func(a toolArgs) string { return "Type into terminal " + a.str("id") },
	"pty-kill":    func(a toolArgs) string { return "Close terminal " + a.str("id") },
	"web-fetch":   func(a toolArgs) string { return "Fetch " + a.str("url") },
	"web-search":  func(a toolArgs) string { return "Search the web: " + a.str("query") },
	"download":    func(a toolArgs) string { return "Download " + a.str("url") + " to " + a.str("path") },
	"todo-write":  func(toolArgs) string { return "Update tasks" },
	"skill": func(a toolArgs) string {
		if name := a.str("name"); name != "" {
			return "Load skill " + name
		}
		return "List skills"
	},
	"ultra": func(a toolArgs) string {
		if d := a.str("description"); d != "" {
			return "ultra: " + d
		}
		return "ultra"
	},
	"goal-complete":      func(toolArgs) string { return "Goal complete" },
	"enter-plan-mode":    func(toolArgs) string { return "Enter plan mode" },
	"exit-plan-mode":     func(toolArgs) string { return "Exit plan mode" },
	"ask-user":           func(toolArgs) string { return "Ask the user" },
	"task-stop":          func(toolArgs) string { return "Stop the run" },
	"save-memory":        func(a toolArgs) string { return "Remember: " + a.str("fact") },
	"send-message":       func(a toolArgs) string { return "Message " + a.firstStr("target") },
	"config":             func(a toolArgs) string { return "Config " + a.str("action") + " " + a.str("key") },
	"enter-worktree":     func(a toolArgs) string { return "Enter worktree " + a.firstStr("path", "branch") },
	"exit-worktree":      func(a toolArgs) string { return "Exit worktree " + a.str("path") },
	"mcp-list-resources": func(a toolArgs) string { return "List MCP resources of " + a.str("server_id") },
	"mcp-read-resource": func(a toolArgs) string {
		return "Read MCP resource " + a.str("resource_id") + " from " + a.str("server_id")
	},
	"mcp-auth": func(a toolArgs) string { return "Authenticate MCP server " + a.str("server_id") },
}

// toolCallTitle is the card title for a trace: a human sentence for the
// built-ins ("Edit main.go", "Run go test ./..."), "<name> <args>" for any
// other tool. Sub-agent and workflow traces get titles naming the member and
// the workflow phase. Titles are one line and at most maxTitleRunes runes.
func toolCallTitle(tr agent.ToolTrace) string {
	args := decodeToolArgs(tr.Args)
	title := ""
	switch tr.Name {
	case "agent":
		// "agent code#3: fix auth tests" tells which swarm or delegation
		// member is doing what, instead of a raw JSON blob. The runtime
		// also accepts "target" and "id" for the agent to run.
		if name := args.firstStr("agent", "target", "id"); name != "" {
			title = "agent " + name
			if task := args.str("task"); task != "" {
				title += ": " + task
			}
		}
	case "workflow", "workflow-progress":
		if wf := args.str("workflow"); wf != "" {
			switch args.str("kind") {
			case "phase":
				title = "workflow " + wf + " ▸ " + args.str("phase")
			case "log":
				title = "workflow " + wf + " · log"
			default:
				title = "workflow " + wf
			}
		}
	default:
		if render, ok := builtinToolTitles[agent.CanonicalToolName(tr.Name)]; ok && args != nil {
			title = strings.TrimSpace(render(args))
		}
	}
	if title == "" {
		title = genericToolTitle(tr.Name, tr.Args)
	}
	return finishTitle(tr, title)
}

// finishTitle attributes a card title to its swarm member and bounds it.
// Swarm members carry instance names like "code#3"; prefixing them keeps
// every tool call attributable when dozens of agents interleave.
func finishTitle(tr agent.ToolTrace, title string) string {
	if strings.ContainsRune(tr.AgentID, '#') && tr.Name != "agent" {
		title = "[" + tr.AgentID + "] " + title
	}
	return clipLine(title, maxTitleRunes)
}

// isUserTool reports whether name belongs to a tool of the operator's own
// (kind mcp, script or http) in the session's manifest. Such a tool owns
// its name even when it is a built-in's: a call under it never reaches the
// built-in (see agent/tool_names.go).
func (t *turnState) isUserTool(name string) bool {
	_, ok := t.manifest.UserTool(name)
	return ok
}

// cardTitle is toolCallTitle for this session: a tool of the operator's own
// gets the generic "<name> <args>" title instead of the wording of the
// built-in whose name it may share ("List ." for an "ls" script that lists
// a bucket would misreport what ran).
func (t *turnState) cardTitle(tr agent.ToolTrace) string {
	if t.isUserTool(tr.Name) {
		return finishTitle(tr, genericToolTitle(tr.Name, tr.Args))
	}
	return toolCallTitle(tr)
}

// cardKind is toolKind for this session: a tool of the operator's own is
// classified by the words in its name, like any tool that is not a
// built-in, never as the built-in of that name.
func (t *turnState) cardKind(name string) acpsdk.ToolKind {
	if t.isUserTool(name) {
		return guessToolKind(strings.ToLower(strings.TrimSpace(name)))
	}
	return toolKind(name)
}

// genericToolTitle is the title of a call no renderer covers (an MCP or
// manifest tool, or arguments that are not JSON): the tool's name followed
// by its arguments. JSON arguments are shown after the same clipping and
// secret redaction as rawInput (boundValue), because the editor displays
// the title and keeps it in its thread history; a token passed to an MCP
// tool must not end up there.
func genericToolTitle(name, rawArgs string) string {
	if rawArgs == "" {
		return name
	}
	var v any
	if err := json.Unmarshal([]byte(rawArgs), &v); err != nil {
		// Not JSON, so there are no field names to redact by; the runtime
		// rejects such a call before it runs.
		return name + " " + rawArgs
	}
	var buf strings.Builder
	enc := json.NewEncoder(&buf)
	// Show "<" and "&" as written, not as \u003c escapes.
	enc.SetEscapeHTML(false)
	if err := enc.Encode(boundValue(v)); err != nil {
		return name
	}
	return name + " " + strings.TrimSpace(buf.String())
}

// clipLine collapses s to a single line (runs of whitespace, newlines
// included, become one space) and cuts it to at most max runes, marking a
// cut with an ellipsis.
func clipLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	runes := []rune(s)
	return string(runes[:max-1]) + "…"
}

// clipBytes cuts s to at most max bytes on a rune boundary, and says how
// much was cut. It returns s unchanged when it fits.
func clipBytes(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + fmt.Sprintf("… [%d more bytes not shown]", len(s)-cut)
}

// toolLocations returns the file a call touches, taken from its arguments,
// so "follow the agent" editors can jump to it. ACP requires absolute paths:
// a relative one is resolved against the session's working directory. (A
// sub-agent in its own worktree names paths relative to that worktree; the
// authoritative absolute paths of changed files arrive with the completion,
// see fileChangeLocations.)
func toolLocations(args, cwd string) []acpsdk.ToolCallLocation {
	a := decodeToolArgs(args)
	if a == nil {
		return nil
	}
	p := a.firstStr("path", "file", "file_path", "filename")
	if p == "" {
		return nil
	}
	if !filepath.IsAbs(p) && cwd != "" {
		p = filepath.Join(cwd, p)
	}
	loc := acpsdk.ToolCallLocation{Path: p}
	for _, k := range []string{"start_line", "offset", "line"} {
		if n, ok := a[k].(float64); ok && n >= 1 {
			loc.Line = acpsdk.Ptr(int(n))
			break
		}
	}
	return []acpsdk.ToolCallLocation{loc}
}

// fileChangeLocations lists the files a call changed, by absolute path.
func fileChangeLocations(changes []agent.FileChange) []acpsdk.ToolCallLocation {
	out := make([]acpsdk.ToolCallLocation, 0, len(changes))
	for _, c := range changes {
		out = append(out, acpsdk.ToolCallLocation{Path: c.Path})
	}
	return out
}

// secretArgKeys are argument names whose values never leave the process in
// rawInput (mcp-auth takes a token, an MCP tool may take an api key).
var secretArgKeys = map[string]bool{
	"token": true, "api_key": true, "apikey": true, "password": true,
	"secret": true, "authorization": true, "access_token": true,
}

// boundedRawInput is a call's arguments for rawInput: the decoded JSON with
// long strings clipped and secrets redacted, or the clipped raw text when it
// is not JSON. Whatever does not fit in maxRawInputBytes once encoded is
// replaced by a clipped string of the encoding.
func boundedRawInput(args string) any {
	if args == "" {
		return nil
	}
	var v any
	if err := json.Unmarshal([]byte(args), &v); err != nil {
		return clipBytes(args, maxRawInputString)
	}
	v = boundValue(v)
	raw, err := json.Marshal(v)
	if err != nil || len(raw) <= maxRawInputBytes {
		return v
	}
	return clipBytes(string(raw), maxRawInputBytes)
}

// boundValue walks a decoded JSON value, clipping strings and redacting
// secret-named fields.
func boundValue(v any) any {
	switch x := v.(type) {
	case string:
		return clipBytes(x, maxRawInputString)
	case []any:
		for i := range x {
			x[i] = boundValue(x[i])
		}
		return x
	case map[string]any:
		for k, val := range x {
			if secretArgKeys[strings.ToLower(k)] {
				x[k] = "[redacted]"
				continue
			}
			x[k] = boundValue(val)
		}
		return x
	}
	return v
}

// rawJSON parses the single-line args string for rawInput; on failure the
// original string is passed through so nothing is lost. It is used for the
// workflow card, whose arguments the runtime writes itself and bounds.
func rawJSON(args string) any {
	if args == "" {
		return nil
	}
	var v any
	if err := json.Unmarshal([]byte(args), &v); err != nil {
		return args
	}
	return v
}

// toolOutputContent renders a tool's text output plus any images it attached
// (screenshot, view-image) as ACP content blocks, so capable editors show the
// captured image inline with the tool call. Unreadable image files are
// skipped — the text output still names the path.
func toolOutputContent(output string, images []string) []acpsdk.ToolCallContent {
	var out []acpsdk.ToolCallContent
	if output != "" {
		out = append(out, acpsdk.ToolContent(acpsdk.TextBlock(clipBytes(output, maxToolTextBytes))))
	}
	for _, p := range images {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		out = append(out, acpsdk.ToolContent(acpsdk.ImageBlock(base64.StdEncoding.EncodeToString(data), imageMime(p))))
	}
	return out
}

// fileChangeContent renders the files a call changed as ACP diff content
// (path, oldText, newText; oldText omitted for a created file). A change
// whose texts the runtime dropped (a very large file), or that would push
// the update past maxDiffBytesPerUpdate, is named in a text note instead, so
// the editor still learns the file changed.
func fileChangeContent(changes []agent.FileChange) []acpsdk.ToolCallContent {
	var out []acpsdk.ToolCallContent
	var skipped []string
	budget := maxDiffBytesPerUpdate
	for _, c := range changes {
		size := len(c.OldText) + len(c.NewText)
		if c.TextOmitted || size > budget {
			skipped = append(skipped, c.Path)
			continue
		}
		budget -= size
		if c.Created {
			out = append(out, acpsdk.ToolDiffContent(c.Path, c.NewText))
		} else {
			out = append(out, acpsdk.ToolDiffContent(c.Path, c.NewText, c.OldText))
		}
	}
	if len(skipped) > 0 {
		sort.Strings(skipped)
		note := "diff not shown (too large to display): " + strings.Join(skipped, ", ")
		out = append(out, acpsdk.ToolContent(acpsdk.TextBlock(clipBytes(note, maxToolTextBytes))))
	}
	return out
}

// imageMime is the inverse of imageExt: extension → MIME type for tool-attached
// image files.
func imageMime(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	default:
		return "image/png"
	}
}
