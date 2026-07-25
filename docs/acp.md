# Agent Client Protocol (ACP)

Spettro can run as an [Agent Client Protocol](https://agentclientprotocol.com)
agent, so ACP-capable editors (Zed, Neovim plugins, JetBrains, ...) can drive
it as an external coding agent inside their native agent UI.

## Running

```bash
spettro --acp
```

The process speaks JSON-RPC over stdio: stdout carries protocol messages,
stderr carries diagnostics. There is nothing to configure on the Spettro
side — the ACP agent reuses your existing configuration (active
provider/model, API keys, permission level, agent manifest, sandbox
settings).

The sandbox flags work as in the other modes:

```bash
spettro --acp --sandbox workspace-write --sandbox-net localhost
```

## Editor setup

### Zed

Add Spettro as a custom agent in `settings.json`:

```json
{
  "agent_servers": {
    "Spettro": {
      "command": "spettro",
      "args": ["--acp"]
    }
  }
}
```

Then open the Agent Panel and pick *Spettro* as the agent.

## What is exposed

- **Sessions** — each `session/new` gets its own working directory (the
  project the editor has open), conversation history, and agent mode.
- **Toolbar selectors** — Spettro advertises ACP *session config options* so
  the editor draws native selectors in its message toolbar:
  - **Mode** — the orchestrator agents from the [manifest](../AGENTS.md)
    (`plan`, `coding`, `ask`); worker/subagent roles are internal delegation
    targets and stay hidden.
  - **Model** — the connected models, grouped by provider, switch the active
    model for the session (persisted to your config).
  - **Permission** — `ask-first`, `restricted`, or `yolo`.
  - **Thinking** — the reasoning/thinking level. Always shown (as `Off`
    when disabled) so the control never disappears from the toolbar;
    non-reasoning models simply ignore the setting.
  - **Ultra** — On/Off toggle for [Ultra mode](ultra.md) (swarm of
    parallel sub-agents for hard tasks). Turning it on requires the
    Restricted or YOLO permission level; under Ask-first the change is
    rejected, and dropping back to Ask-first suspends Ultra until the
    level is raised again.

  Changing a selector calls `session/set_config_option`; the equivalent slash
  commands (`/mode`, `/models`, `/permission`, `/thinking`) push a
  `config_option_update` back so the selectors stay in sync. This supersedes
  the deprecated `session/set_mode` "modes" mechanism, which current clients
  no longer render.
- **Streaming** — the model's reasoning streams live as thought chunks and
  every tool call is reported with kind, status, file locations, and output,
  so the editor can render progress and follow the agent across files. The
  final answer is sent as a single `agent_message` block when the turn
  completes (the internal stream has draft-reset semantics, so the answer is
  flushed from the authoritative final content rather than chunked).
- **Token usage** — after every LLM request inside a turn (not just at the
  end), Spettro sends a `usage_update` session notification with the current
  context occupancy (`used`) against the model's context window (`size`), so
  editors that support it render a live context gauge while the agent is
  still working. The cumulative turn cost travels in `_meta`
  (`spettro.app/tokensUsed`) on each update, and the completed turn's
  aggregated accounting (input/output plus cache read/write tokens) is
  returned in the `session/prompt` response's `usage` field.
- **Plan** — whenever the agent updates its session task graph (`task-create`,
  `task-update`, `task-delete`, or the legacy `todo-write`), the full task list is mirrored
  to the client as an ACP `plan` update in dependency order, so editors with
  plan support render the agent's live todo list; tasks gated by incomplete
  dependencies are suffixed with "(blocked)".
- **Permissions** — shell command approvals are routed through
  `session/request_permission`, so the editor shows its native approval
  prompt. With `/permission yolo` set in Spettro's config, shell commands run
  without asking.
- **Agent questions** — when the agent calls `ask-user` the question is put to
  the client as a structured payload; see [Agent questions](#agent-questions)
  below for the transports, the payload, and the answer shape.
- **Commands** — `/help`, `/mode`, `/models`, `/permission`, `/budget`,
  `/thinking`, `/goal`, `/memory`, `/compact`, and `/clear` are advertised to
  the client (`available_commands_update`). Config commands resolve in one
  turn without invoking the model; `/models` with no argument lists the
  connected models, and `/models provider:model [api_key]` switches the
  active one. `/memory show|add|clear` edits the persistent memory store
  (the same one the TUI's `/memory` command uses); the dialog-only `edit`,
  `review`, and `mine` sub-commands remain TUI-only. `/compact [auto
  <status|on|off>]` summarizes older history to free context window space.
  `/goal <objective>` runs the autonomous goal loop inside the prompt turn
  — cancel the turn to stop it. Anything else needing a TUI dialog
  (`/skill`, `/mcp`, ...) is not available over ACP yet. `/resume` is
  intentionally not advertised: the editor's own session picker drives
  `session/load` instead (see below).
- **Prompt content** — text, `@`-mentioned files (resource links), embedded
  context, and images are accepted in prompts.
- **Tool-call images** — when a tool attaches an image for the model (the
  `view-image` vision tool, see [vision.md](vision.md)), the corresponding
  `tool_call`/`tool_call_update` carries an image content block (base64 +
  mime) next to the text output, so editors render the screenshot inline in
  the tool-call card.
- **Cancellation** — `session/cancel` interrupts the running turn; the turn
  ends with the `cancelled` stop reason. `/goal stop` sent as a new prompt
  also cancels a running goal turn.
- **Mid-run steering** — a `session/prompt` sent while a turn is already
  executing does not kill or replace the run: it is delivered to the running
  agent as steering, injected as a user message at the agent's next step
  boundary (append-only, so the provider prompt cache keeps hitting). The
  steering prompt's own turn ends immediately with a "steering queued" note,
  and a "✔ steering delivered" message streams when the agent actually sees
  it. This works for normal turns and for `/goal` turns (the queue is shared
  across goal iterations). Clients that want the classic replace behavior
  keep it: sending `session/cancel` first stops the run, and the next prompt
  starts a fresh turn. A steering message the run never reached is held and
  delivered at the start of the session's next turn.
- **Session persistence** — `session/load`, `session/resume`, and
  `session/list` are fully supported (the agent advertises `LoadSession:
  true`, plus `SessionCapabilities.List` and `SessionCapabilities.Resume` at
  `initialize`). All three are backed by Spettro's on-disk session store, so
  conversations started in either the TUI or the ACP client are visible to
  both:
  - `session/load` — restores the stored session under its original ID and
    **replays** the transcript to the client as `user_message`,
    `agent_thought`, and `agent_message` session updates in order, so the
    editor rebuilds its conversation view from scratch. The first prompt
    after a load also gets a flattened copy of the transcript as bounded
    `role: line` history (capped at 32 KiB) so the model has the prior
    context before any new messages are added.
  - `session/resume` — restores the session under its original ID and
    re-announces config options, but skips the replay (the client already
    holds the transcript).
  - `session/list` — enumerates the on-disk store, optionally filtered to
    the request's `cwd`, newest first. Each entry carries the session id,
    project path, title (first user prompt preview), and `updatedAt`.

  Sessions persist automatically after every prompt turn, so the editor's
  session picker stays current without any explicit save action. MCP
  servers provided by the editor in `session/new` are still ignored;
  Spettro's own MCP configuration applies as usual.

## Agent questions

The `ask-user` tool lets the model put a decision back to you: a question,
selectable options, one of them marked as *recommended*, and optionally a
free-text answer. It is available to the agents you converse with directly
(`plan`, `coding`, `ask`); worker and sub-agent runs cannot interrupt you with
a question.

Core ACP has no question primitive, so Spettro offers the same payload over
three transports and takes the best one the client supports.

| Client supports | Transport | What the user gets |
|---|---|---|
| `_spettro/question/ask` (mirrored back at `initialize`) | `CallExtension` with the payload below | Full fidelity: descriptions, recommended marker, free text |
| `_meta` on permission requests | `session/request_permission` with the payload in `_meta` and `isRecommended` on the matching option | Native picker plus the recommended marker; free text via the answer `_meta` |
| `elicitation.form` capability | `session/elicitation/create` (form mode) | Free-text answers, including option-less questions |
| none of the above | plain `session/request_permission` | Working multiple-choice prompt |

If none of them can reach you — an option-less question against a client with
no elicitation support — the model gets an error telling it to proceed on its
own judgment or offer explicit options. The agent's own `default_option` is
never returned as if a human had chosen it.

### Handshake

Spettro advertises its extension surface in the `initialize` response
`_meta["spettro.app/extensions"]`:

```json
{
  "version": 2,
  "methods": ["_spettro/account/status", "..."],
  "clientMethods": ["_spettro/question/ask"]
}
```

`methods` are served by the agent; `clientMethods` are served by the *client*.
Nothing is called on the client until it mirrors the ones it implements back
in its own `initialize` request `_meta`, using the same key and shape:

```json
{ "_meta": { "spettro.app/extensions": { "version": 2, "methods": ["_spettro/question/ask"] } } }
```

Client capabilities from `initialize` (`elicitation.form` in particular) are
recorded per connection and gate the transports above.

### Question payload

Sent as the `_spettro/question/ask` params, and mirrored into
`_meta["spettro.app/question"]` on the permission request:

```json
{
  "version": 1,
  "sessionId": "…",
  "question": "Which database?",
  "context": "both are already provisioned",
  "options": [
    { "id": "opt-0", "label": "Postgres" },
    { "id": "opt-1", "label": "SQLite", "isRecommended": true }
  ],
  "allowCustomInput": true
}
```

On the permission transport each `PermissionOption` carries
`_meta["spettro.app/isRecommended"]` on the recommended answer, and when
`allowCustomInput` is set a synthetic final option (`optionId: "custom"`,
flagged with `_meta["spettro.app/isCustomInput"]`) offers free text.

### Answer

Every transport resolves to the same tagged shape — as the
`_spettro/question/ask` result, or in
`_meta["spettro.app/questionAnswer"]` on the permission response:

```json
{ "kind": "option", "optionId": "opt-1" }
{ "kind": "custom", "text": "neither — use the existing MySQL box" }
{ "kind": "declined" }
{ "kind": "cancelled" }
```

An option resolves to that option's label; custom text reaches the model
verbatim, never as the synthetic option's label. A client that answers a
`_meta`-annotated request without any `_meta` of its own is read from the
selected `optionId` instead; selecting the synthetic `custom` option then
escalates to an elicitation to collect the text, or fails if the client cannot
collect it. `declined`, `cancelled`, and a cancelled permission outcome all
tell the model that nobody answered.
