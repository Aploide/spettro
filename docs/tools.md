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
the model calls it (see [hooks](hooks.md#matcher-syntax)). Likewise a
permission rule naming a former language-server tool (`lsp-restart`, say)
still denies that `lsp` op.

`task-update` keeps its old contract: an unknown `id` is an error rather
than a new task, and an empty `dependencies` list leaves the stored ones as
they are.

Manifests are migrated on load. v12 replaces the old names in
`allowed_tools` (only where the agent could actually call the old tool) and
removes the former `grok-image`/`grok-video` generators; v13 does the same
for the language-server tools, and gives an agent that held only some of
them a rule denying the other ops, so it gains none. Permission rules are
left as written. See the v12 and v13 notes in
[AGENTS.md](../AGENTS.md#root-fields).
