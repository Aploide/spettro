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
  `/thinking`, `/goal`, `/loop`, `/memory`, `/compact`, and `/clear` are advertised to
  the client (`available_commands_update`). Config commands resolve in one
  turn without invoking the model; `/models` with no argument lists the
  connected models, and `/models provider:model [api_key]` switches the
  active one. `/memory show|add|clear` edits the persistent memory store
  (the same one the TUI's `/memory` command uses); the dialog-only `edit`,
  `review`, and `mine` sub-commands remain TUI-only. `/compact [auto
  <status|on|off>]` summarizes older history to free context window space.
  `/goal <objective>` runs the autonomous goal loop inside the prompt turn
  — cancel the turn to stop it. `/loop <time> <prompt>` re-runs the prompt on
  the given interval inside the prompt turn the same way; `/loop stop` or the
  editor's cancel ends it. Anything else needing a TUI dialog
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
  ends with the `cancelled` stop reason. `/goal stop` and `/loop stop` sent
  as new prompts also cancel a running goal/loop turn.
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

The model asks a *form*: up to four related questions put to you together. Core
ACP has no question primitive, so Spettro offers the same payload over several
transports and takes the best one the client supports — the first two carry the
whole form, the rest carry one question at a time.

| Client supports | Transport | What the user gets |
|---|---|---|
| `_spettro/question/ask` (mirrored back at `initialize`) | `CallExtension` with the form payload below | The whole form in one interaction: descriptions, previews, recommended marker, multi-select, free text |
| `elicitation.form` capability (multi-question forms) | `elicitation/create` (form mode) with a schema property per question | Every question in one native form; `enum` picks, `array` multi-select, free text |
| `_meta` on permission requests | `session/request_permission` per question, with the payload in `_meta` and `isRecommended` on the matching option | Native picker plus the recommended marker; free text via the answer `_meta` |
| `elicitation.form` capability (single question) | `elicitation/create` (form mode) | Free-text answers, including option-less questions |
| none of the above | plain `session/request_permission` per question | Working multiple-choice prompt |

Elicitation requests are sent in the spec's own shape: `mode: "form"`, the
`sessionId` scope the request belongs to, `requestedSchema`, and the question
payload below in `_meta`. A client that rejects one — no such method, or a body
it cannot read — is treated as a client without elicitation, and the form is
walked instead of failing the turn.

Each question is one property, `q-0`…, keyed by position: `enum` for a
single-select, `array` of `enum` for a multi-select, a plain string for a
question with no options. Nothing in the elicitation schema is both a picker and
a text box, so a question that has options *and* allows free text is sent as
**two** properties — the picker, plus `q-N-custom` for the user's own words. Fill
in either. If both are filled the answer carries both, since a choice and the
words written beside it are two things the user said; free text that names one of
the options resolves to that option instead. Option descriptions and the
recommended marker are folded into each property's `description`, which is the
only place an `enum` leaves for them.

A form that has to be walked question by question is asked in order, and
declining any question declines the whole form: half a form delivered as if you
had skipped the rest would misreport what you said. Option previews have no
representation outside the extension transport and are dropped there; option
descriptions are folded into the option name (`label — description`) so a bare
picker still shows what separates the choices.

If none of them can reach you — an option-less question against a client with
no elicitation support — the model gets an error telling it to proceed on its
own judgment or offer explicit options. The agent's own `default_option` is
never returned as if a human had chosen it.

### Handshake

Spettro advertises its extension surface in the `initialize` response
`_meta["spettro.app/extensions"]`:

```json
{
  "version": 3,
  "methods": ["_spettro/account/status", "..."],
  "clientMethods": ["_spettro/question/ask"]
}
```

`methods` are served by the agent; `clientMethods` are served by the *client*.
Nothing is called on the client until it mirrors the ones it implements back
in its own `initialize` request `_meta`, using the same key and shape:

```json
{ "_meta": { "spettro.app/extensions": { "version": 3, "methods": ["_spettro/question/ask"] } } }
```

Client capabilities from `initialize` (`elicitation.form` in particular) are
recorded per connection and gate the transports above.

### Question payload

Sent as the `_spettro/question/ask` params, and mirrored into
`_meta["spettro.app/question"]` on the permission request. Version 2 carries the
whole form in `questions[]` and keeps every version 1 field alongside it,
describing the form's **first** question — so a client written against version 1
still renders something answerable:

```json
{
  "version": 2,
  "sessionId": "…",
  "question": "Which database?",
  "context": "both are already provisioned",
  "options": [
    { "id": "opt-0", "label": "Postgres" },
    { "id": "opt-1", "label": "SQLite", "isRecommended": true }
  ],
  "allowCustomInput": true,
  "questions": [
    {
      "id": "q-0",
      "header": "Database",
      "question": "Which database?",
      "options": [
        { "id": "opt-0", "label": "Postgres", "description": "already provisioned" },
        { "id": "opt-1", "label": "SQLite", "isRecommended": true, "preview": "file: ./spettro.db" }
      ],
      "multiSelect": false,
      "allowCustomInput": true
    },
    {
      "id": "q-1",
      "header": "Checks",
      "question": "Which checks run before commits?",
      "options": [{ "id": "opt-0", "label": "go vet" }, { "id": "opt-1", "label": "gofmt" }],
      "multiSelect": true,
      "allowCustomInput": false
    }
  ]
}
```

A per-question walk (`session/request_permission`, or elicitation for a single
question) sends the version 1 shape with no `questions[]`, one question at a
time. The `version` field is what tells the two apart.

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

A form answered through `_spettro/question/ask` comes back as one answer per
question instead, each naming the question it belongs to (`questionId`, or
`header`) and carrying `optionIds` for a multi-select answer:

```json
{
  "answers": [
    { "questionId": "q-0", "kind": "option", "optionId": "opt-1" },
    { "questionId": "q-1", "kind": "option", "optionIds": ["opt-0", "opt-1"], "notes": "vet is the slow one" },
    { "questionId": "q-2", "kind": "custom", "text": "neither — use the existing MySQL box" }
  ]
}
```

A question with no answer in the array — or one answered `declined` — is
reported to the model as unanswered rather than defaulted; `kind` at the top
level of the response (rather than inside `answers`) declines the whole form.
A client that answers the flat question with the bare tagged shape is read as
having answered the form's first question, and the rest come back unanswered.

The elicitation form uses one property per question, keyed by the question id:
`enum` of option labels for a single-select question, `array` with
`items.enum` for a multi-select one, and a plain string where the question
takes free text (including a question that offers options *and* allows free
text, since an `enum` would forbid the text it explicitly allows — a reply
naming an option is resolved back to it). Nothing is marked `required`: a form
you answer in part is delivered in part, and the questions you left alone are
reported as unanswered.

An option resolves to that option's label; custom text reaches the model
verbatim, never as the synthetic option's label. A client that answers a
`_meta`-annotated request without any `_meta` of its own is read from the
selected `optionId` instead; selecting the synthetic `custom` option then
escalates to an elicitation to collect the text, or fails if the client cannot
collect it. `declined`, `cancelled`, and a cancelled permission outcome all
tell the model that nobody answered.
