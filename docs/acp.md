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
- **Modes** — the enabled agents from the [manifest](../AGENTS.md) (`plan`,
  `coding`, `ask`, `explore`, ...) are advertised as ACP session modes, so
  the editor's mode picker switches Spettro agents.
- **Streaming** — the model's reasoning streams live as thought chunks and
  every tool call is reported with kind, status, file locations, and output,
  so the editor can render progress and follow the agent across files. The
  final answer is sent when the turn completes.
- **Permissions** — shell command approvals and agent questions are routed
  through `session/request_permission`, so the editor shows its native
  approval prompt. With `/permission yolo` set in Spettro's config, shell
  commands run without asking.
- **Commands** — `/help`, `/mode`, `/permission`, `/budget`, `/thinking`, and
  `/clear` are advertised to the client (`available_commands_update`) and
  resolve in one turn without invoking the model. Anything else needing a
  TUI dialog (`/models`, `/skill`, `/mcp`, ...) is not available over ACP yet.
- **Prompt content** — text, `@`-mentioned files (resource links), embedded
  context, and images are accepted in prompts.
- **Cancellation** — `session/cancel` interrupts the running turn; the turn
  ends with the `cancelled` stop reason.

Session persistence (`session/load`) and ACP-provided MCP servers are not
supported yet; Spettro's own MCP configuration applies as usual.
