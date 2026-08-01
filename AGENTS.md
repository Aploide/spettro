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

- `version` (int, required): schema version, currently `10`. Older manifests
  are migrated on load (with a `.bak` backup): v3 rewrites the previously
  inert `sandbox_mode = "workspace-write"` default to `full-access` (the
  field is now enforced — re-set it explicitly if you want the OS sandbox);
  later versions retrofit new built-in tools (v5 `view-image`, v6
  `hover`/`rename-symbol`, v7 `repo-search`, v8 the
  `pty-start`/`pty-write`/`pty-kill` interactive terminal tools, granted to
  agents that already hold `shell-exec`, v9 `tool-output` for agents that
  already hold `file-read`, v10 `ask-user`). Each retrofit widens only
  allow-lists that already show the same level of trust, so a deliberately
  restricted agent is never opened up.
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
- `[runtime.delegation]`: defaults for `max_parallel_workers` and `max_depth`.

### `[[tools]]`

- `id` (required, unique)
- `name` (required)
- `description`
- `kind`: `builtin`, `mcp`, `script`, `http`
- `enabled`: boolean
- `entry_point`: required when `kind` is `mcp`, `script`, or `http`
- `timeout_sec`: positive integer
- `requires_approval`: boolean
- `permitted_actions`: non-empty string list, e.g. `read`, `write`, `search`, `execute`, `git`, `chat`, `network`
- `aliases`: optional alternate tool IDs
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
