package tui

import (
	"fmt"
	"strings"

	"spettro/internal/commands"
)

type commandDef struct {
	name string
	desc string
}

var allCommands = []commandDef{
	{"/help", "show help"},
	{"/login", "sign in to your Spettro subscription"},
	{"/logout", "sign out of your Spettro subscription"},
	{"/models", "switch model"},
	{"/connect", "connect a provider"},
	{"/mode", "cycle mode"},
	{"/approve", "execute pending plan"},
	{"/permission", "set permission level"},
	{"/budget", "set token budget per request  usage: /budget <n|0>"},
	{"/init", "analyze codebase and write SPETTRO.md"},
	{"/compact", "summarize conversation (optionally focused)"},
	{"/tasks", "manage session tasks"},
	{"/mcp", "list/read/auth MCP resources"},
	{"/skill", "manage Agent Skills (list/install/info/enable/disable/uninstall/where/reload)"},
	{"/skills", "alias of /skill list"},
	{"/hooks", "list effective runtime hooks"},
	{"/memory", "show/edit/clear persistent cross-session memory"},
	{"/memory mine", "scan saved sessions and draft candidate memories into the review inbox"},
	{"/memory review", "approve or discard drafted memory candidates"},
	{"/memory curate", "LLM pass over saved memory: propose merges, rewrites, deletions for review"},
	{"/plan", "switch plan mode or run plan task"},
	{"/goal", "run autonomously until an objective is met (no step/token limits)"},
	{"/permissions", "show/set permission level"},
	{"/remote", "start loopback HTTP control plane on 127.0.0.1 (optional :PORT)"},
	{"/remote local", "start LAN HTTP control plane on 0.0.0.0 (optional :PORT)"},
	{"/remote stop", "stop the running remote control plane"},
	{"/remote status", "print remote control URL and bearer token"},
	{"/telegram", "Telegram relay: setup, allow, start/stop, status (alias /tg)"},
	{"/tg", "alias of /telegram"},
	{"/think", "set extended-thinking level (alias of /thinking)"},
	{"/ultra", "toggle Ultra: fan hard tasks out across a swarm of parallel sub-agents"},
	{"/jobs", "list background shell jobs"},
	{"/jobs kill", "kill a background job by ID (or all)"},
	{"/stats", "show session token usage and prompt-cache metrics"},
	{"/diff", "show diffs of files modified this session (optional paths)"},
	{"/clear", "clear conversation history"},
	{"/resume", "resume a previous conversation"},
	{"/rewind", "rewind files and/or conversation to a checkpoint (esc esc)"},
	{"/update", "update spettro to the latest release"},
	{"/exit", "exit spettro"},
}

var skillCommands = []commandDef{
	{"/skill list", "list discovered skills and their scope/source"},
	{"/skill install", "install from local path, https git URL, or owner/repo"},
	{"/skill info", "show metadata, resources, and body excerpt for a skill"},
	{"/skill enable", "enable a skill in this project"},
	{"/skill disable", "disable a skill without uninstalling it"},
	{"/skill uninstall", "remove an installed skill"},
	{"/skill where", "list every discovery root and whether it exists"},
	{"/skill reload", "re-scan skill directories"},
}

var permissionCommands = []commandDef{
	{"/permission yolo", "no approval required for any action"},
	{"/permission restricted", "ask once, remember for session"},
	{"/permission ask-first", "always ask before executing"},
}

var thinkingCommands = []commandDef{
	{"/thinking off", "no extended thinking (default)"},
	{"/thinking low", "low reasoning effort (~2k thinking tokens on Anthropic)"},
	{"/thinking medium", "medium reasoning effort (~5k thinking tokens on Anthropic)"},
	{"/thinking high", "high reasoning effort (~16k thinking tokens on Anthropic)"},
	{"/thinking x-high", "extra-high reasoning effort (~32k thinking tokens on Anthropic)"},
	{"/thinking max", "maximum reasoning effort (~100k thinking tokens on Anthropic)"},
}

var thinkCommands = []commandDef{
	{"/think off", "no extended thinking (default)"},
	{"/think low", "low reasoning effort (~2k thinking tokens on Anthropic)"},
	{"/think medium", "medium reasoning effort (~5k thinking tokens on Anthropic)"},
	{"/think high", "high reasoning effort (~16k thinking tokens on Anthropic)"},
	{"/think x-high", "extra-high reasoning effort (~32k thinking tokens on Anthropic)"},
	{"/think max", "maximum reasoning effort (~100k thinking tokens on Anthropic)"},
}

// requiresParam reports whether the slash command must be followed by a
// sub-parameter before it can be executed. Selecting such a command from the
// completion menu always opens the second-level selector instead of running.
func requiresParam(cmd string) bool {
	switch strings.ToLower(strings.TrimSpace(cmd)) {
	case "/think", "/thinking", "/permission", "/permissions":
		return true
	case "/jobs kill":
		// Needs a job ID (or "all"); executing bare would just error.
		return true
	}
	return false
}

// activeModelSupportsReasoning reports whether the active model is flagged
// reasoning-capable in the catalog. Thinking commands and status tags are
// hidden for models that would ignore the setting anyway.
func (m Model) activeModelSupportsReasoning() bool {
	return m.providers.SupportsReasoning(m.cfg.ActiveProvider, m.cfg.ActiveModel)
}

// filterCommands matches query against the built-in catalog plus any
// user-defined custom commands discovered at startup.
func (m Model) filterCommands(query string) []commandDef {
	catalog := make([]commandDef, 0, len(allCommands)+len(m.customCommands))
	for _, c := range allCommands {
		// Thinking levels only apply to reasoning-capable models (per the
		// catalog's `reasoning` flag), so hide the commands entirely otherwise.
		if (c.name == "/thinking" || c.name == "/think") && !m.activeModelSupportsReasoning() {
			continue
		}
		catalog = append(catalog, c)
	}
	for _, c := range m.customCommands {
		desc := c.Description
		if desc == "" {
			desc = "custom command (" + c.Scope + ")"
		}
		catalog = append(catalog, commandDef{"/" + c.Name, desc})
	}
	if query == "" {
		return catalog
	}
	q := strings.ToLower(query)
	var out []commandDef
	for _, c := range catalog {
		if strings.Contains(strings.ToLower(c.name), q) || strings.Contains(strings.ToLower(c.desc), q) {
			out = append(out, c)
		}
	}
	return out
}

// findCustomCommand resolves "/name" (case-insensitive) to a user-defined
// custom command.
func (m Model) findCustomCommand(cmd string) (commands.Command, bool) {
	name := strings.ToLower(strings.TrimPrefix(cmd, "/"))
	for _, c := range m.customCommands {
		if strings.ToLower(c.Name) == name {
			return c, true
		}
	}
	return commands.Command{}, false
}

// customCommandsHelp renders the user-defined commands section appended to
// /help output; empty when no custom commands are defined.
func (m Model) customCommandsHelp() string {
	if len(m.customCommands) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\ncustom commands (~/.spettro/commands, .spettro/commands):\n")
	for _, c := range m.customCommands {
		desc := c.Description
		if desc == "" {
			desc = "custom command"
		}
		fmt.Fprintf(&b, "  /%-13s %s (%s)\n", c.Name, desc, c.Scope)
	}
	b.WriteString("  supports {{args}}; shell interpolation !`cmd` requires yolo permission")
	return b.String()
}

// isInstantCommand reports whether the given slash command can be executed
// immediately, even while an agent run is in progress. Instant commands only
// touch local config, storage, or display state; they never start a new LLM
// run, never destroy the in-flight conversation, and never replace the
// active session. They are intentionally excluded from the input history
// (up/down recall) since they are transient operational toggles rather than
// reusable prompts.
func isInstantCommand(input string) bool {
	input = strings.TrimSpace(input)
	if !strings.HasPrefix(input, "/") {
		return false
	}
	fields := strings.Fields(input)
	if len(fields) == 0 {
		return false
	}
	switch strings.ToLower(fields[0]) {
	case "/help",
		"/permission", "/permissions",
		"/budget",
		"/thinking", "/think",
		"/ultra",
		"/login",
		"/logout",
		"/connect",
		"/models",
		"/skill", "/skills",
		"/hooks",
		"/tasks",
		"/stats",
		"/diff",
		"/mcp",
		"/mode", "/next",
		"/remote",
		"/telegram", "/tg",
		"/update",
		"/exit", "/quit":
		return true
	case "/plan":
		// /plan with no args just switches mode; /plan <task> launches the plan agent.
		return len(fields) == 1
	case "/goal":
		// /goal stop and /goal status are instant; /goal <objective> starts a run.
		if len(fields) >= 2 {
			sub := strings.ToLower(fields[1])
			return sub == "stop" || sub == "status"
		}
		return false
	case "/compact":
		// /compact auto <...> and /compact policy only read or toggle config.
		// /compact (no args) and /compact <focus> trigger an LLM compaction run.
		if len(fields) >= 2 {
			sub := strings.ToLower(fields[1])
			return sub == "auto" || sub == "policy"
		}
		return false
	}
	return false
}

const helpText = `commands:
  /help          this message
  /exit /quit    quit spettro  (or ctrl+c twice)
  /update        download and install the latest release, then restart
  /login         sign in to your Spettro subscription (opens browser)
  /logout        sign out of your Spettro subscription
  /mode          cycle to next mode  (or shift+tab)
  /models        open model selector (connected providers only)
  /models p:m    set model directly
  /connect       connect a provider or local endpoint
  /permission    set permission: yolo | restricted | ask-first
  /permissions   show/set permission level, debug details
  /budget [n|0]  set token budget per request (0 = unlimited)
  /think <l>     set extended-thinking level (off|low|medium|high|x-high|max)
  /thinking <l>  alias of /think
  /ultra [on|off] toggle Ultra: swarm of parallel sub-agents for hard tasks (any model)
  /approve       approve and execute pending plan (coding mode)
  /plan [prompt] switch to plan mode or run a plan request
  /goal <obj>   run autonomously until the objective is met
  /goal stop    abandon the active goal
  /goal status  show goal iteration / progress / elapsed
  /tasks         manage tasks (list/add/done/set/show)
  /mcp           manage MCP resources (list/read/auth)
  /skill         manage Agent Skills (list/install/uninstall/info/enable/disable)
  /skill install <source>   install from path, https git URL, or owner/repo
  /hooks         list effective runtime hooks (project + global)
  /memory        show persistent memory (user + project)
  /memory edit [user|project]   edit a memory file in $EDITOR
  /memory clear [user|project|all]  erase saved memory
  /memory mine [n]   scan saved sessions, draft memory candidates (background)
  /memory review     approve/discard drafted candidates (nothing saves without approval)
  /memory curate [user|project|all]  propose dedup/merge/expiry edits, apply per-op
  /init          analyze codebase and write SPETTRO.md
  /compact [x]   summarize conversation (optional focus instruction)
  /compact auto  view/set auto-compact (status|on|off)
  /compact policy show compact thresholds and counters
  /remote [:port]       start local HTTP/SSE control plane on 127.0.0.1
  /remote local [:port] start LAN HTTP/SSE control plane on 0.0.0.0
  /remote stop          stop the running remote control plane
  /remote status        print remote control URL and bearer token
  /telegram setup <token>  configure BotFather token (alias /tg)
  /telegram allow <@u|id>  allow a username or chat ID to drive Spettro
  /telegram start|stop|status  control the Telegram relay
  /stats         show session token usage and prompt-cache hit rate
  /diff [path]   show diffs of files modified this session (all, or given paths)
  /jobs          list background shell jobs started by the agent
  /jobs kill <id>|all  terminate a background job (or all of them)
  /clear         clear conversation history (auto-saves first)
  /resume        resume a previous saved conversation
  /rewind        restore files and/or conversation to a pre-edit checkpoint

keys:
  esc esc        open the rewind picker (when idle)
  shift+tab      cycle mode (plan → coding → ask)
  f2             cycle to next favorite model
  shift+f2       cycle to previous favorite model
  ctrl+y         copy last assistant response to clipboard
  ctrl+f         attach a file to the next message
  ctrl+r         remove last file attachment
  ctrl+b         toggle side activity panel
  ctrl+o         toggle expanded tool context in side panel
  ctrl+t         toggle text-select mode (release mouse for terminal selection)

in model selector:
  f              toggle favorite (★) for highlighted model
  c              open connect provider dialog`
