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
are skills). Its other tools (`send-message`, `save-memory`, `config`,
`download`, `skill-list`, `task-stop`, the `pty-*` tools, `view-image`,
`rename-symbol`, `web-search`, `mcp-auth`, the worktree tools, ...) are
deferred: the system prompt names them in one line, and a `tool-search` for a
name or keyword returns the matching tools' descriptions and schemas and
advertises them from the next step on. A deferred tool stays callable: a call
by name runs, and advertises it too. The tool list therefore changes only
when a tool is loaded, and a later turn of the conversation keeps what an
earlier one loaded. Deferral never grants anything: `tool-search` only finds
tools on the agent's `allowed_tools`, and an agent without `tool-search` gets
all its tools advertised.

With no language server configured or installed for the workspace (see
[lsp](lsp.md)), `lsp` and `rename-symbol` are neither advertised nor found by
`tool-search`, and the system prompt says there is none. This is decided
once per process: a server installed mid-session is picked up by the next
session.

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
that shares a former name (a `hover` script, say) is yours: calls, hooks and
rules under that name are its, never the `lsp` op's.

`task-update` keeps its old contract: an unknown `id` is an error rather
than a new task, and an empty `dependencies` list leaves the stored ones as
they are.

Manifests are migrated on load. v12 replaces the old names in
`allowed_tools` (only where the agent could actually call the old tool) and
removes the former `grok-image`/`grok-video` generators; v13 does the same
for the language-server tools, and gives an agent that held only some of
them an `lsp-op` rule denying each of the other ops, so it gains none. Other
permission rules are left as written. With a tool of your own called `lsp`,
v13 folds nothing: the built-ins keep their own names. See the v12 and v13 notes in
[AGENTS.md](../AGENTS.md#root-fields).
