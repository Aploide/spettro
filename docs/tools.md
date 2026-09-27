# Built-in tools

These are the tools Spettro's agents call. Which ones an agent gets is set by
its `allowed_tools` in [`spettro.agents.toml`](../AGENTS.md); each request
only carries the schemas of the tools that agent may use.

| Area | Tools |
|---|---|
| Files | `file-read`, `file-write`, `file-edit` (one `old_string`/`new_string`, or several `edits[]` applied atomically), `view-image` |
| Search | `grep` (a regex `pattern`, or a `symbol` for ranked definitions then usages; see [symbol index](symbol-index.md)), `glob` (a path `pattern`, or no pattern to list one directory) |
| Shell | `bash` (the host shell: bash, or PowerShell on Windows; `run_in_background` starts a job), `job-output`, `job-kill`, `pty-start`, `pty-write`, `pty-kill` ([pty](pty.md)) |
| Output | `tool-output` (pages a spooled result; see [session](session.md#tool-output-spooling)) |
| Language server | `lsp` (`op`: `diagnostics`, `references`, `definition`, `hover` or `restart`), `rename-symbol` ([lsp](lsp.md)) |
| Tasks | `todo-write` (read, replace, merge into or prune the session task list; see [session](session.md#task-graph)), `task-stop`, `goal-complete` |
| Web | `web-search`, `web-fetch`, `download` ([web tools](web-tools.md)) |
| Delegation | `agent`, `ultra` ([ultra](ultra.md)), `workflow` ([workflows](workflows.md)), `send-message` |
| User and session | `ask-user`, `comment`, `save-memory`, `config`, `enter-plan-mode`, `exit-plan-mode`, `enter-worktree`, `exit-worktree` |
| Skills and tools | `skill-list`, `skill-read`, `tool-search` |
| MCP | `mcp-list-resources`, `mcp-read-resource`, `mcp-auth` |

## Deferred tools

An agent holding `tool-search` gets only its core tools advertised up front:
`agent`, `glob`, `grep`, `file-read`, `file-write`, `file-edit`, `bash`,
`job-output`, `job-kill`, `tool-output`, `todo-write`, `web-fetch`, `lsp`,
`ask-user`, `comment`, `tool-search`, `goal-complete`, the plan-mode tools,
`ultra`, `workflow` and the MCP resource tools (plus `skill-read` when there
are skills), and any tool it holds that its prompt names in backticks (the
coding agent's prompt names `view-image`, the ask agent's `web-search`).
Its other tools (`send-message`, `save-memory`, `config`, `download`,
`skill-list`, `task-stop`, the `pty-*` tools, `rename-symbol`, `mcp-auth`,
the worktree tools, ...) are deferred: the system prompt names them in one
line, and a `tool-search` for a name or keyword returns the matching tools'
descriptions and schemas and advertises them from the next step on. A
deferred tool stays callable: a call by name runs, and advertises it too.
Loaded tools follow the core ones in `allowed_tools` order, and the
conversation records which are loaded, even through compaction. The tool
list therefore changes only when a tool is loaded, and a later turn of the
conversation advertises exactly the list the last request did. Deferral
never grants anything: `tool-search` only finds tools on the agent's
`allowed_tools`, and an agent without `tool-search` gets all its tools
advertised.

With no language server configured or installed for the workspace (see
[lsp](lsp.md)), `lsp` and `rename-symbol` are neither advertised nor found by
`tool-search`, and the system prompt says there is none. This is decided
once per process, for the life of that process: a server installed while
spettro runs is picked up only after spettro restarts (a new session in the
same TUI or ACP process does not re-check).

## Retired names

Several tools used to exist twice under different names, and the read-only
language-server tools were four tools for what is one tool with an operation.
Each group now has one canonical tool, and the old names are hidden aliases:
a call under an old name still runs, as the canonical tool, but only the
canonical tool is advertised to the model or listed by `tool-search`.

| Canonical | Retired names | How a retired call maps |
|---|---|---|
| `bash` | `shell-exec`, `bash-output` | Same arguments. |
| `file-edit` | `multi-edit` | Same arguments (`edits[]`). |
| `grep` | `repo-search` | `{"query": q}` becomes `{"symbol": q}`. |
| `glob` | `ls` | `{"path": p}` becomes a pattern-less `glob`, which lists `p`. |
| `todo-write` | `task-create`, `task-update` | One task, merged by `id`. |
| | `task-delete` | `delete: [id]`, or `clear_completed`. |
| | `task-get`, `task-list` | A read; returns the whole list. |
| `skill-read` | `activate-skill`, `skill-activate` | Same arguments. |
| `lsp` | `diagnostics` | Same arguments, with `op: "diagnostics"`. |
| | `references` | `op: "references"`, or `op: "definition"` for `kind: "definition"`. |
| | `hover` | Same arguments, with `op: "hover"`. |
| | `lsp-restart` | Same arguments, with `op: "restart"`. |

The canonical tool must be allowed: an old name never grants access the agent
does not already have. Permission rules, the tool trace and the TUI and ACP
clients see the canonical name. Hooks match the canonical name and the name
the model called; a hook for `shell-exec` or `bash-output`, which were the
very same tool as `bash`, also keeps firing on `bash`, and a hook for a
former language-server tool fires on the `lsp` op that replaced it however
the model calls it (see [hooks](hooks.md#matcher-syntax)). Which `lsp` ops an
agent may call is set by `lsp-op` permission rules, with the op as the
pattern (`{ permission = "lsp-op", pattern = "restart", action = "deny" }`);
rules naming the former tools no longer decide anything. A tool of your own
that shares a canonical or retired name is not an alias of anything: see
[below](#tools-of-your-own-with-a-built-ins-name).

`task-update` keeps its old contract: an unknown `id` is an error rather
than a new task, and an empty `dependencies` list leaves the stored ones as
they are.

Manifests are migrated on load. v12 replaces the old names in
`allowed_tools` (only where the agent could actually call the old tool) and
removes the former `grok-image`/`grok-video` generators; v13 does the same
for the language-server tools, and gives an agent that held only some of
them an `lsp-op` rule denying each of the other ops, so it gains none. Other
permission rules are left as written. Neither migration folds anything into
a canonical name a tool of your own holds (see below). See the v12 and v13
notes in [AGENTS.md](../AGENTS.md#root-fields).

## Tools of your own with a built-in's name

A tool you define in the manifest (`kind` `mcp`, `script` or `http`) may use
any `id` or alias, a built-in's included: a canonical name (`bash`,
`file-edit`, `grep`, `glob`, `todo-write`, `skill-read`, `lsp`) or a retired
one (`shell-exec`, `bash-output`, `multi-edit`, `repo-search`, `ls`,
`task-*`, `activate-skill`, `skill-activate`, `diagnostics`, `references`,
`hover`, `lsp-restart`). One rule covers every such name.

**Your tool wins every call made by its name.** A call under that name is
never turned into a call of a built-in and never runs a built-in's code, and
your tool is never advertised with a built-in's description or schema.
Permission rules and hooks written for the name apply to your tool's calls.
Spettro does not run `mcp`, `script` or `http` tools yet: a call of one is
checked against the allow-list, permission rules and hooks like any other
call, then fails with an error saying nothing was run.

**The built-ins your tool shadows stay reachable under their own names.**

- Your tool takes a retired name (an `ls` script): the canonical built-in
  (`glob`) is unaffected. Hooks and rules written for `ls` are your tool's
  and never apply to `glob`, not even for `shell-exec` and `bash-output`,
  whose hooks otherwise also fire on every `bash` call.
- Your tool takes a canonical name (a `bash` script): that tool's retired
  names stop being aliases and stand *unfolded*. An agent that holds a
  built-in under a retired name (a `shell-exec` tool of kind `builtin`)
  calls it by that name: the built-in's code runs (here, the shell), and the
  allow-list, permission rules, approval (the `shell-exec` entry's
  `requires_approval` and rules), hooks, traces and loop detection all see
  `shell-exec`. It is advertised under its own name, with its old
  description and schema, and is deferred or core as its canonical tool is.
  A retired name the agent does not hold is refused as not allowed: it never
  becomes a call of your tool. Hooks and rules written for `bash` are your
  tool's and do not apply to `shell-exec`.

In a manifest written from today's defaults, the retired names live only as
aliases on the canonical built-in's definition, and taking a canonical name
means replacing that definition (tool IDs are unique). Its retired names
then answer to nothing. Unfolded built-ins matter for manifests migrated
from before v12 or v13 that already had such a tool.

**Migrations never hand your tool out in a built-in's place.** v12 and v13
fold nothing into a canonical name your tool holds (as its `id` or an
alias): that group's built-ins keep their own definitions and allow-list
entries. Your tool is never folded into a built-in, never given a
built-in's aliases, never added to a built-in's aliases, and no allow-list
entry is ever rewritten to point at it. The earlier retrofits follow the same
rule: the v11 `general-purpose` agent is granted built-ins only (a built-in
under a retired name when your tool holds the canonical one), and the v6-v8
retrofits read only built-ins as trust (holding a `grep` script of yours does
not earn `repo-search`) and grant only built-ins. The built-in `bash` gets
its long-standing `bash-output` alias only while no other tool uses that
name.
