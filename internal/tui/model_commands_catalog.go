package tui

import "strings"

type commandDef struct {
	name string
	desc string
}

var allCommands = []commandDef{
	{"/help", "show help"},
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
	{"/skill", "manage Agent Skills (install/list/info/uninstall)"},
	{"/skill install", "install a skill from path, git URL, or owner/repo"},
	{"/skills", "alias of /skill"},
	{"/hooks", "list effective runtime hooks"},
	{"/plan", "switch plan mode or run plan task"},
	{"/permissions", "show/set permission level"},
	{"/remote", "start local HTTP control plane (optional :PORT, /remote stop|status)"},
	{"/clear", "clear conversation history"},
	{"/resume", "resume a previous conversation"},
	{"/exit", "exit spettro"},
}

var permissionCommands = []commandDef{
	{"/permission yolo", "no approval required for any action"},
	{"/permission restricted", "ask once, remember for session"},
	{"/permission ask-first", "always ask before executing"},
}

func filterCommands(query string) []commandDef {
	if query == "" {
		return append([]commandDef(nil), allCommands...)
	}
	q := strings.ToLower(query)
	var out []commandDef
	for _, c := range allCommands {
		if strings.Contains(c.name, q) || strings.Contains(c.desc, q) {
			out = append(out, c)
		}
	}
	return out
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
		"/connect",
		"/models",
		"/skill", "/skills",
		"/hooks",
		"/tasks",
		"/mcp",
		"/mode", "/next",
		"/remote",
		"/exit", "/quit":
		return true
	case "/plan":
		// /plan with no args just switches mode; /plan <task> launches the plan agent.
		return len(fields) == 1
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
  /mode          cycle to next mode  (or shift+tab)
  /models        open model selector (connected providers only)
  /models p:m    set model directly
  /connect       connect a provider or local endpoint
  /permission    set permission: yolo | restricted | ask-first
  /permissions   show/set permission level, debug details
  /approve       approve and execute pending plan (coding mode)
  /plan [prompt] switch to plan mode or run a plan request
  /tasks         manage tasks (list/add/done/set/show)
  /mcp           manage MCP resources (list/read/auth)
  /skill         manage Agent Skills (list/install/uninstall/info/enable/disable)
  /skill install <source>   install from path, https git URL, or owner/repo
  /hooks         list effective runtime hooks (project + global)
  /init          analyze codebase and write SPETTRO.md
  /compact [x]   summarize conversation (optional focus instruction)
  /compact auto  view/set auto-compact (status|on|off)
  /compact policy show compact thresholds and counters
  /remote [:port] start local HTTP/SSE control plane on 127.0.0.1
  /remote stop   stop the running remote control plane
  /remote status print remote control URL and bearer token
  /clear         clear conversation history (auto-saves first)
  /resume        resume a previous saved conversation

keys:
  shift+tab      cycle mode (plan → coding → ask)
  f2             cycle to next favorite model
  shift+f2       cycle to previous favorite model
  ctrl+b         toggle side activity panel
  ctrl+t         toggle text-select mode (release mouse for terminal selection)

in model selector:
  f              toggle favorite (★) for highlighted model
  c              open connect provider dialog`
