# Spettro agent manifest

Spettro supports a project-level agent manifest at:

- `spettro.agents.toml`

If the file is missing, Spettro uses an internal default manifest.

## Goals

This file lets you define, in one place:

- which agents exist
- what each agent is good at
- which tools each agent can use
- what actions each tool and agent is allowed to perform
- handoff relationships between agents
- runtime safety defaults

## Schema

### Root fields

- `version` (int, required): schema version, currently `13`. Older manifests
  are migrated on load (with a `.bak` backup): v3 rewrites the previously
  inert `sandbox_mode = "workspace-write"` default to `full-access` (the
  field is now enforced — re-set it explicitly if you want the OS sandbox);
  later versions retrofit new built-in tools (v5 `view-image`, v6
  `hover`/`rename-symbol` for agents holding `references` (or, since v13,
  `lsp`), v7 `repo-search`, v8 the
  `pty-start`/`pty-write`/`pty-kill` interactive terminal tools, granted to
  agents that already hold a shell tool, v9 `tool-output` for agents that
  already hold `file-read`, v10 `ask-user`, v11 the `general-purpose`
  subagent). Each retrofit widens only allow-lists that already show the
  same level of trust, so a deliberately restricted agent is never opened
  up.
- v12 folds duplicate built-ins into one canonical tool each and removes the
  `grok-image`/`grok-video` generators:

  | Canonical | Retired (hidden aliases) |
  |---|---|
  | `bash` | `shell-exec`, `bash-output` |
  | `file-edit` | `multi-edit` |
  | `grep` | `repo-search` (now `grep`'s `symbol` argument) |
  | `glob` | `ls` (`glob` without a pattern lists one directory) |
  | `todo-write` | `task-create`, `task-update`, `task-delete`, `task-get`, `task-list` |

  No agent gains access. For each agent, the migration first works out
  which retired tools it could actually call under the v11 manifest
  (enabled, an action the agent may take, no permission rule denying it);
  only those become the canonical ID in its `allowed_tools`, and one it
  could not call is dropped. A retired definition's settings merge into the
  canonical tool toward the stricter side (approval if either required it,
  the longer timeout, the higher risk, its command/path rules that deny or
  ask), or it becomes the canonical tool in place when that is missing. The
  canonical tool keeps its own `enabled` flag and `permitted_actions`, so a
  canonical tool you switched off stays off. The retired tool's allow rules,
  and the rules that switched it off, are not merged. Permission rules that
  name a retired ID are left as written: they only ever decided whether that
  tool could be called, which the allow-lists now carry. An agent that could
  only read tasks (`task-get`/`task-list`) loses them instead of gaining
  `todo-write`; an agent left with no tools keeps `comment` and is disabled. Tools of another
  kind that share a retired name are left alone, and a tool of your own that
  holds a canonical name (a `bash` script) folds nothing of its group: those
  built-ins keep their own names (see [Built-in tools](docs/tools.md#tools-of-your-own-with-a-built-ins-name)). Folded retired names stay
  callable, but are never advertised to the model and cannot be listed in
  `allowed_tools`. A built-in left under its own name because a tool of your
  own holds its canonical name is the exception: it stays in `allowed_tools`
  and is advertised under that name (it stands unfolded; see the same
  section of docs/tools.md).
- v13 folds the read-only language-server tools into one `lsp` tool whose
  `op` argument picks the operation:

  | `lsp` op | Retired tool (hidden alias) |
  |---|---|
  | `diagnostics` | `diagnostics` |
  | `references`, `definition` | `references` (`kind: "definition"` is op `definition`) |
  | `hover` | `hover` |
  | `restart` | `lsp-restart` |

  `rename-symbol` writes files and needs approval, so it stays a tool of its
  own. The migration works like v12's: an agent gets `lsp` only if it could
  actually call at least one of the four tools, the first enabled of
  `diagnostics`/`references`/`hover`/`lsp-restart` becomes `lsp` in place
  when there is no `lsp` definition, the others merge into it toward the
  stricter side, and the old names become its aliases. Because the old tools
  are now operations of one tool, an agent that could call only some of them
  gets an agent-level rule `{ permission = "lsp-op", pattern = "<op>",
  action = "deny" }` for each op of the others (`references` is ops
  `references` and `definition`): an agent that held `hover` but not
  `lsp-restart` still cannot restart a server. A tool of your own that shares
  an old name (a `hover` script) is not the built-in, so holding it grants
  no op. An agent whose own rules would deny `lsp` (a `"*"` deny with an
  allow per tool) gets a rule allowing `lsp`, so it keeps the ops it had.
  Rules naming the old tools are left as written and no longer decide
  anything. If you have a tool of your own called `lsp`, nothing is folded:
  the four built-ins stay tools of their own, under their own names. The
  stock agents held all four, so they get no rules. `lsp` is low-risk and
  needs no approval, as the four tools were.
- `default_agent` (string, required): agent ID to start from.
- `[metadata]` (table, optional): human-facing metadata.
- `[runtime]` (table, required): global execution defaults.
- `[[tools]]` (array of tables, required): tool registry.
- `[[agents]]` (array of tables, required): callable agents.

### `[runtime]`

- `default_permission`: one of `ask-first`, `restricted`, `yolo`.
- `default_timeout_sec`: positive integer.
- `sandbox_mode`: `off`/`full-access` (no OS sandbox, default), `workspace-write`
  (writes confined to workspace + temp, reads confined to system + workspace),
  or `read-only` (also blocks workspace writes). Enforced via Seatbelt (macOS) /
  Landlock (Linux) for shell commands AND in-process for the `file-write`/
  `file-edit` tools, and the spettro process is write-confined as a backstop.
  The boundary is invisible to the model (no tool, no prompt hint); overridable
  with the `--sandbox` CLI flag. See `docs/configuration.md`.
- `sandbox_net`: optional network policy for sandboxed commands: `all`
  (default), `localhost`, `none`, or `ports:443,8080`. CLI: `--sandbox-net`.
- `sandbox_allow_dirs`: optional extra writable roots inside the sandbox.
  CLI: `--sandbox-allow-dir` (repeatable).
- `sandbox_allow_read_dirs`: optional extra readable-only roots (e.g. a
  toolchain cache outside the workspace when reads are confined).
  CLI: `--sandbox-allow-read-dir` (repeatable).
- `log_tool_calls`: boolean.
- `permission_rules`: optional layered policy rules (`permission`, `pattern`, `action`).
  The permission `lsp-op` takes an `lsp` op as its pattern and decides which
  ops an agent may call: `{ permission = "lsp-op", pattern = "restart",
  action = "deny" }` keeps the lookups but not restarts. Only rules naming
  `lsp-op` itself apply to ops, so a `"*"` permission or pattern that the
  `lsp` tool is allowed around does not take its ops away.
- `[runtime.delegation]`: defaults for `max_parallel_workers` and `max_depth`.

### `[[tools]]`

- `id` (required, unique). A tool of kind `mcp`, `script` or `http` may take
  a built-in's name (`bash`, `ls`, `hover`, ...); it then owns every call made
  by that name, and the built-ins it shadows stay reachable under their own
  names (see [Built-in tools](docs/tools.md#tools-of-your-own-with-a-built-ins-name)).
  Spettro does not run tools of those kinds yet: a call of one fails without
  running anything.
- `name` (required)
- `description`
- `kind`: `builtin`, `mcp`, `script`, `http`
- `enabled`: boolean
- `entry_point`: required when `kind` is `mcp`, `script`, or `http`
- `timeout_sec`: positive integer
- `requires_approval`: boolean
- `permitted_actions`: non-empty string list, e.g. `read`, `write`, `search`, `execute`, `git`, `chat`, `network`
- `aliases`: optional alternate tool IDs (unique across the manifest: an alias may not repeat a tool `id` or another tool's alias)
- `input_schema`: optional JSON-like schema metadata
- `risk_level`: optional `low|medium|high`
- `primary_only`: optional boolean (only primary/orchestrator agents can use)
- `permission_rules`: optional tool-scoped policy rules

### `[[agents]]`

- `id` (required, unique)
- `name` (required)
- `description`
- `skill` (short capability keyword)
- `mode` (e.g. `planning`, `coding`, `chat`, `custom`)
- `role`: `primary`, `subagent`, `orchestrator`, or `worker`
- `model_provider` / `model` (optional override; fallback is active UI model)
- `system_prompt` or `prompt_file`
- `allowed_tools`: non-empty tool ID list
- `permitted_actions`: action list for high-level policy
- `permission`: `ask-first`, `restricted`, or `yolo`
- `temperature`, `max_tokens`
- `permission_rules`: optional agent-scoped policy rules
- `handoffs`: list of target agent IDs
- `enabled`: boolean

`allowed_tools` and `permitted_actions` are both filters: a tool is callable
only when its ID is allow-listed *and* the agent holds at least one of the
tool's `permitted_actions`. An allow-listed tool whose action family the agent
lacks is silently unavailable to the model.

### Asking the user a question

`ask-user` is the only way an agent can put a decision back to the person
driving it. It is granted to the agents a human converses with directly — the
`primary` and `orchestrator` roles, i.e. `plan`, `coding`, and `ask` in the
default manifest — and withheld from workers and subagents (`code` included):
their runs inherit the parent's callback, so a nested worker's question would
interrupt the user mid-orchestration with no context about who is asking.

Granting it to another agent takes both halves: `"ask-user"` in
`allowed_tools` and `"ask"` in `permitted_actions`.

One call carries a **form**, not a single question: an ordered list of up to 4
questions, each with a short `header` (the tab label, unique within the form and
the key the answer comes back under), the question line, up to 8 options
(`label`, optional `description` and `preview`, `is_recommended`), a
`multi_select` flag and an `allow_custom` free-text entry. Both caps are hard —
an over-cap form is rejected with an error the model can correct, never
truncated. The older flat payload (`question` + `options: [...]` +
`default_option` + `allow_free_response`) is still accepted and normalised into
a one-question form, so custom agent files that use it keep working.

Answers come back one line per question, `<header>: <answer>`: multi-select
answers comma-joined, the user's own words quoted verbatim, and a question the
user skipped explicitly marked as unanswered so the agent cannot read silence as
agreement with its recommendation. The TUI shows the whole form at once, in the
input box so the conversation stays readable behind it: one tab per question,
`tab` / `←→` between them, `enter` to record an answer and a trailing
`✓ Submit` tab that sends them together (`esc` declines the form). Surfaces that
can only put one question at a time to the user (the ACP transports,
remote/Telegram) walk the form through one shared adapter rather than each
reimplementing the loop.

## Validation rules

Spettro validates at startup:

- unknown TOML fields are rejected
- tool IDs and agent IDs must be unique
- `default_agent` must exist
- all `allowed_tools` and `handoffs` must reference existing IDs
- all permissions and timeouts must be valid

## Writing tips

- Start from the included `spettro.agents.toml` template.
- Keep IDs stable; rename labels, not IDs, to avoid breaking references.
- Use narrow `allowed_tools` and `permitted_actions` by default.
- Keep one responsibility per agent (`planning`, `coding`, `chat`, etc.).

## Prompt file folder

This repository ships a ready-to-edit prompt folder:

- `agents/`

The default pack includes specialized roles for day-to-day CLI/TUI work:

- planning
- coding
- chat
- explore
- git
- reviewer
- tester
- docs-writer
- general-purpose
