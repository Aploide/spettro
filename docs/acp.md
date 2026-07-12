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
  - **Thinking** — the extended-thinking level, shown only for
    reasoning-capable models.

  Changing a selector calls `session/set_config_option`; the equivalent slash
  commands (`/mode`, `/models`, `/permission`, `/thinking`) push a
  `config_option_update` back so the selectors stay in sync. This supersedes
  the deprecated `session/set_mode` "modes" mechanism, which current clients
  no longer render.
- **Streaming** — the model's reasoning streams live as thought chunks and
  every tool call is reported with kind, status, file locations, and output,
  so the editor can render progress and follow the agent across files. The
  final answer is sent when the turn completes.
- **Plan** — whenever the agent updates its session task graph (`task-create`,
  `task-update`, `task-delete`, or the legacy `todo-write`), the full task list is mirrored
  to the client as an ACP `plan` update in dependency order, so editors with
  plan support render the agent's live todo list; tasks gated by incomplete
  dependencies are suffixed with "(blocked)".
- **Permissions** — shell command approvals and agent questions are routed
  through `session/request_permission`, so the editor shows its native
  approval prompt. With `/permission yolo` set in Spettro's config, shell
  commands run without asking.
- **Commands** — `/help`, `/mode`, `/models`, `/permission`, `/budget`,
  `/thinking`, `/goal`, and `/clear` are advertised to the client
  (`available_commands_update`). Config commands resolve in one turn without
  invoking the model; `/models` with no argument lists the connected models,
  and `/models provider:model [api_key]` switches the active one. `/goal
  <objective>` runs the autonomous goal loop inside the prompt turn — cancel
  the turn to stop it. Anything needing a TUI dialog (`/skill`, `/mcp`,
  `/resume`, ...) is not available over ACP yet.
- **Prompt content** — text, `@`-mentioned files (resource links), embedded
  context, and images are accepted in prompts.
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

Session persistence (`session/load`) and ACP-provided MCP servers are not
supported yet; Spettro's own MCP configuration applies as usual.
