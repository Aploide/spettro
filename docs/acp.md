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
  Sessions on one connection run independently and at the same time: each
  has its own run slot, steering queue and environment snapshot (the
  working directory listing, git branch, date and instruction files taken
  when the conversation started, then carried with its history), so a file
  created while session A is open shows up in a session started later but
  never changes A's system prompt. The mode is per session; the model,
  permission, thinking level and Ultra live in your user config and are
  shared, so changing one from any session sends a `config_option_update`
  to every other open session and a run in progress there applies a new
  permission level at its next approval.
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
- **Streaming** — the model's reasoning streams live as
  `agent_thought_chunk`s. Text the model writes in a step that also calls
  tools ("Let me check the tests first.") is sent once as an
  `agent_message_chunk` when the step ends, followed by a blank line, and so
  is a message it sends with the `comment` tool; a sub-agent's prose,
  comment-tool messages and steering notices, and the runtime's own
  progress notes, are not. The final answer is sent as a single
  `agent_message_chunk` when the turn completes (the internal stream has
  draft-reset semantics, so the answer is flushed from the authoritative
  final content rather than chunked). A `/goal` iteration that ends with
  `goal-complete` and no summary returns its last step's prose, which was
  already sent, so it is not sent again.
- **Tool calls** — see [Tool calls](#tool-calls) below: every call is a
  card with a kind, a readable title, absolute file locations, its output,
  and a real diff for file changes.
- **Token usage** — after every LLM request inside a turn (not just at the
  end), Spettro sends a `usage_update` session notification with the current
  context occupancy (`used`) against the model's context window (`size`), so
  editors that support it render a live context gauge while the agent is
  still working. The cumulative turn cost travels in `_meta`
  (`spettro.app/tokensUsed`) on each update, and the completed turn's
  aggregated accounting (input/output plus cache read/write tokens) is
  returned in the `session/prompt` response's `usage` field.
- **Plan** — whenever the agent updates its session task graph (`todo-write`
  replacing the list, merging changes into it, or deleting tasks, or one of
  the retired `task-*` names that route to it), the full task list is
  mirrored to the client as an ACP `plan` update in dependency order (a task
  follows the tasks it waits for), so editors with plan support render the
  agent's live todo list. Status maps to `pending`, `in_progress` or
  `completed` (a cancelled task counts as completed); priority `high` or
  `urgent` is `high`, `low` is `low`, anything else `medium`; a pending task
  gated by incomplete dependencies is suffixed with "(blocked)". An empty
  list is still sent, so deleting the last task clears the editor's plan.
- **Workflows** — a [workflow](workflows.md) run (any message containing
  `ultracode`) opens a single `workflow <name>` tool call whose content is
  rewritten as the run progresses: declared phases appear immediately as
  pending, fill in as agents land under them, and carry per-phase
  done/failed counts plus the script's `log()` lines. Each sub-agent
  additionally gets its own tool call, so "follow the agent" navigation
  still works. Phases are deliberately *not* published as ACP plan
  entries — that channel belongs to the session task graph, and a
  workflow would silently clobber it.
- **Permissions** — every approval the runtime asks for (shell commands,
  file writes and edits, network access) is routed through
  `session/request_permission` on the tool call's own card, so the editor
  shows its native approval prompt there; see [Permissions](#permissions)
  below. With `/permission yolo` nothing is asked.
- **Agent questions** — when the agent calls `ask-user` the question is put to
  the client as a structured payload; see [Agent questions](#agent-questions)
  below for the transports, the payload, and the answer shape.
- **Commands** — `/help`, `/mode`, `/models`, `/permission`, `/budget`,
  `/thinking`, `/goal`, `/loop`, `/memory`, `/compact`, `/workflows`,
  `/skills`, and `/clear` are advertised to the client
  (`available_commands_update`), followed by one command per
  [Agent Skill](skills.md) the user can run in the session's workspace
  (description and argument hint from its `SKILL.md`; a skill named like a
  built-in command is not advertised). Config commands resolve in one
  turn without invoking the model; `/models` with no argument lists the
  connected models, and `/models provider:model [api_key]` switches the
  active one. `/memory show|add|clear` edits the persistent memory store
  (the same one the TUI's `/memory` command uses); the dialog-only `edit`,
  `review`, and `mine` sub-commands remain TUI-only. `/compact [auto
  <status|on|off>]` summarizes older history to free context window space.
  `/goal <objective>` runs the autonomous goal loop inside the prompt turn
  — cancel the turn to stop it. `/loop <time> <prompt>` re-runs the prompt on
  the given interval inside the prompt turn the same way; `/loop stop` or the
  editor's cancel ends it. `/workflows` lists, shows, and locates saved
  [workflow](workflows.md) scripts inline; `/workflows run <name> [json]`
  is rewritten into an ordinary turn that invokes that script.
  `/<skill-name> [args]` runs the turn with that skill's instructions, and
  `$<skill-name>` in a prompt appends the skill's instructions. Both are
  read from the text the user typed only: files the editor attached are
  passed along as context after the instructions, never as the skill's
  arguments, and a `$word` inside them is not a mention. The transcript
  replayed on `session/load` keeps what the user typed, also when the
  prompt arrives during a running turn and becomes steering for it. A
  skill added or changed on disk is picked up on the next prompt, and
  advertised in sessions created after the change. `/skills`
  lists the skills inline. Anything else needing a TUI dialog
  (`/skill install`, `/mcp`, ...) is not available over ACP yet. `/resume` is
  intentionally not advertised: the editor's own session picker drives
  `session/load` instead (see below).
- **Prompt content** — text, `@`-mentioned files (resource links), embedded
  context, and images are accepted in prompts. A `file://` resource link
  (percent-encoded paths included) is a file the agent must read with
  `file-read` before anything else, like an `@` mention in the TUI. A link
  to a file outside the session's project, or to one that does not exist,
  stays in the prompt text but is not required, so it can never hold up the
  turn; a link with another scheme (`https://`) is only text.
- **Tool-call images** — when a tool attaches an image for the model (the
  `view-image` vision tool, see [vision.md](vision.md)), the corresponding
  `tool_call`/`tool_call_update` carries an image content block (base64 +
  mime) next to the text output, so editors render the screenshot inline in
  the tool-call card.
- **Cancellation** — `session/cancel` interrupts the running turn, whatever
  it is waiting on (the model, a tool, or a permission prompt, which is
  withdrawn with `$/cancel_request`); the turn ends with the `cancelled`
  stop reason, never with an error. `/goal stop` and `/loop stop` sent as
  new prompts also cancel a running goal/loop turn.
- **Stop reasons** — a `session/prompt` ends with `end_turn` when the model
  answers (also for slash commands and for a prompt delivered as steering),
  `cancelled` after `session/cancel` or `session/close`, and `refusal` when
  the provider's content filter stopped the reply. Any other failure (the
  provider unreachable after retries, an unknown agent) is a JSON-RPC error
  carrying the runtime's message; the conversation up to the failure is
  kept either way. Requests naming a session this connection does not hold,
  a relative `cwd`, or an unknown mode or option value are rejected as
  invalid params (`-32602`).
- **Mid-run steering** — a `session/prompt` sent while a turn is already
  executing does not kill or replace the run: it is delivered to the running
  agent as steering, injected as a user message at the agent's next step
  boundary (append-only, so the provider prompt cache keeps hitting). The
  steering prompt's own turn ends immediately with a "steering queued" note,
  and a "✔ steering delivered" message streams when the agent actually sees
  it (only for the session's own agent: a sub-agent's steering queue
  carries the runtime's time-limit wrap-up notice, not your messages). This works for normal turns and for `/goal` turns (the queue is shared
  across goal iterations). Clients that want the classic replace behavior
  keep it: sending `session/cancel` first stops the run, and the next prompt
  starts a fresh turn. A steering message the run never reached is held and
  delivered at the start of the session's next turn.
- **Session persistence** — `session/load`, `session/resume`,
  `session/list` and `session/close` are fully supported (the agent
  advertises `LoadSession: true`, plus `SessionCapabilities.List`,
  `SessionCapabilities.Resume` and `SessionCapabilities.Close` at
  `initialize`). They are backed by Spettro's on-disk session store, so
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
  - `session/close` — cancels the session's running turn (which still
    answers its `session/prompt` with `cancelled` and saves what it did) and
    drops the session from the connection, freeing its in-memory history.
    The stored conversation stays on disk: `session/load` or
    `session/resume` brings it back.

  Sessions persist automatically after every prompt turn, so the editor's
  session picker stays current without any explicit save action. MCP
  servers provided by the editor in `session/new` are still ignored;
  Spettro's own MCP configuration applies as usual.

## Tool calls

Each tool call the agent (or one of its sub-agents) makes is one card in the
editor: a `tool_call` notification when it starts (`in_progress`), then one
`tool_call_update` when it finishes (`completed` or `failed`). Parallel calls
get separate cards; identical calls running at once complete in the order
they started. A call rejected before it could run (arguments of a retired
tool name that do not convert) arrives as a single, already finished
`tool_call`.

| Field | What Spettro sends |
|---|---|
| `kind` | From the tool's canonical name, so a retired name gets its canonical tool's kind: `read` for `file-read`, `view-image`, `skill`, `job-output`, `tool-output` and the MCP resource tools; `edit` for `file-write`, `file-edit`, `rename-symbol`; `search` for `grep`, `glob`, `lsp`, `tool-search`; `execute` for `bash`, `job-kill` and the `pty-*` tools; `fetch` for `web-fetch`, `web-search`, `download`; `think` for `todo-write`, `agent`, `ultra`, `workflow`, `goal-complete`; `switch_mode` for `enter-plan-mode` and `exit-plan-mode`; `other` for the rest. A tool that is not a built-in (MCP, or a tool of your own in the manifest, even one with a built-in's name) is classified from the words in its name. |
| `title` | A sentence for the built-ins (`Run go test ./...`, `Edit internal/app.go`, `Search TODO in internal`, `Load skill greet`), read from the argument names the runtime accepts (`command` or `cmd`; `path` or `file_path`), `<name> <arguments>` for other tools (a tool of your own in the manifest included, even when it has a built-in's name), with the arguments clipped and redacted as in `rawInput`, `agent <id>: <task>` for a sub-agent. Swarm members are prefixed with their instance (`[code#3] Read a.go`). One line, at most 120 characters. |
| `locations` | The file named by the call's `path` argument, resolved against the session's working directory (ACP paths are absolute), with the start line when the call gives one. The completion replaces it with the absolute paths of the files the call actually changed. |
| `rawInput` | The call's arguments, with each string cut to 2 KiB, the whole object to 16 KiB, and values named `token`, `api_key`, `password`, `secret` (and similar) redacted. |
| `content` | On completion: a `diff` block (`path`, `oldText`, `newText`; no `oldText` for a created file) for every file `file-write`, `file-edit` or `rename-symbol` changed, then a short excerpt of the text output (the runtime cuts it to a few hundred bytes for every front-end; the model itself sees the full output) and any image the tool attached. |
| `rawOutput` | `{"output": <the text output>}`. |

Size limits keep a card renderable however large the call is. A change to a
file over 256 KiB (before or after), or the part of a multi-file rename past
512 KiB of diff text, is named in a "diff not shown" note instead of being
diffed. Every clipped value ends with a note saying how much was left out.

`comment` calls and the runtime's progress notes do not get cards (see
**Streaming** above), and approval decisions are not reported separately:
the card either waits on the permission prompt or fails with the policy's
reason. Workflow runs are one long-lived card, see **Workflows** above.

## Permissions

Under `ask-first` (and `restricted`, for what it does not allow outright)
the runtime asks before a shell command that is not already allowed, a file
write or edit whose tool requires approval, and access to a network target
not yet allowed. Permission rules, hooks and the saved allow-lists decide
first; an `lsp-op` rule denies an lsp operation outright rather than asking.
Each question becomes a `session/request_permission` whose `toolCall` is the
card already on screen, set to `pending`. The runtime says which agent asks
and in which directory it works, because the main agent and its sub-agents
run tools at the same time: only that agent's cards of the asking tool are
candidates, a card already showing a prompt is skipped (its call is waiting
on that prompt), and among the rest the one naming the approval's command,
file (a relative path resolved against that agent's directory, which is a
worktree for an isolated sub-agent) or network target wins, else the newest.
The request carries:

- for a file change, a `diff` block of the exact change (for a file too
  large to diff structurally, the whole unified diff as text);
- for a command, the whole command as a fenced code block, then the reason
  and the command segments still needing approval; for a network access,
  the whole target the same way. A card's title and `rawInput` are clipped
  (see **Streaming**), so this block is where the editor shows everything
  being approved. It is cut only past 4 MiB, far beyond any command or diff
  a person reads, and then a `[truncated: N of M bytes not shown; this is
  not the whole text]` line follows the block, so a cut text never reads as
  the whole;
- every character that would not show as itself written out, as the TUI
  does: in the command or target block, the text diff and the card title, a
  carriage return is `^M`, an escape `^[`, a tab `⇥`, and a bidi override,
  zero-width character, variation selector, no-break space or other
  invisible character a `\u202e`-style escape (`\U000e0100` past U+FFFF).
  The editor draws the prompt, but it would hide those just as a terminal
  does. A structured `diff` block shows the file's own text and cannot be
  escaped, so when that text holds such a character the prompt adds a line
  saying so and the unified diff with each one written out;
- when "Always allow" would remember more than the command itself (a
  command is remembered as the parts a shell runs separately: `go build &&
  go test` as `go build` and `go test`, a heredoc as every line of its body),
  a line saying so and the list of those commands, one per line;
- options `allow-once` ("Allow once"), `allow-always` and `deny` ("Deny").
  `allow-always` is offered only for commands and network targets, the
  approvals Spettro remembers (in the project's allowed-commands and
  allowed-network lists); a file write is asked about every time, so it is
  not offered there. What is remembered is the exact target, so the label
  names it: "Always allow this command" (or "Always allow the N commands
  listed" when the list above is shown), "Always allow this URL"
  (`web-fetch`, `download`; one URL, not the whole site), "Always allow this
  search" (`web-search`; that query), "Always allow this MCP server"
  (`mcp-list-resources`, `mcp-auth`) or "Always allow this MCP resource"
  (`mcp-read-resource`).

After "Allow" the card goes back to `in_progress` and finishes normally;
after "Deny" (or a `cancelled` outcome) the call fails without running and
the model is told it was denied. A question with no open card (rare) carries
its own title, kind and input, and its card is finished right after the
answer.

A permission prompt is bound to the tool's own time limit (`timeout_sec` in
the manifest: 120 s for `bash`, 60 s for `file-write`/`file-edit`). If it is
still unanswered then, Spettro withdraws it with `$/cancel_request` and the
call fails without running, telling the model nobody approved it in time;
`session/cancel` withdraws it the same way. Nothing waits on the editor
forever.

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

## For maintainers

| Piece | Where |
|---|---|
| Handshake, sessions, prompt turns, stop reasons, `session/close`, shared-settings sync | `internal/acp/bridge.go` |
| `session/load`, `session/resume`, `session/list` | `internal/acp/sessions.go` |
| Tool call cards, comments and narration, plans | `internal/acp/content.go` |
| Kinds, titles, locations, size limits, diffs | `internal/acp/tools.go` |
| Permission requests, which card they attach to, "always allow" labels | `internal/acp/permission.go` (unit tests in `permission_test.go`) |
| Toolbar selectors | `internal/acp/config_options.go` |
| File changes reported by the runtime (`ToolTrace.FileChanges`, `ShellApprovalRequest.Change`) | `internal/agent/file_changes.go` |
| The asking agent and its directory on every approval request (`ShellApprovalRequest.AgentID`, `CWD`) | `toolRuntime.askApproval` in `internal/agent/llm_runtime_ext.go` |

`internal/acp/e2e_test.go` drives the whole protocol the way an editor does:
a client connection from the ACP Go SDK talks to the bridge over in-memory
pipes, the agent runs the default manifest against a scripted
OpenAI-compatible model (`e2e_harness_test.go`), and the tests read the
JSON-RPC traffic as it went over the wire. Run them with

```bash
go test ./internal/acp -run TestACPEndToEnd
```
