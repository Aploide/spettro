# Commands and Keybindings

## Slash commands

| Command | Description |
| --- | --- |
| `/help` | Show in-app help text. |
| `/exit`, `/quit` | Quit Spettro. |
| `/mode`, `/next` | Cycle active manifest agent/mode. |
| `/connect` | Open provider/local-endpoint connect dialog. |
| `/models` | Open model selector dialog (connected providers). |
| `/models <provider:model> [api_key]` | Set model directly; optional API key saves for provider. |
| `/permission <ask-first\|restricted\|yolo>` | Set execution policy. |
| `/permissions [ask-first\|restricted\|yolo]` | Show or set policy alias. |
| `/permissions debug <on\|off>` | Toggle permission diagnostics in UI. |
| `/budget <n\|0>` | Set request token budget (`0` = unlimited). |
| `/thinking <off\|low\|medium\|high\|x-high>` | Set extended-thinking compute budget for the active model. Honoured by Anthropic Claude Opus / Sonnet; ignored by providers that don't expose a thinking parameter. |
| `/plan [prompt]` | Switch to `plan` mode or run a planning request directly. |
| `/approve` | Execute pending plan through `coding` agent. |
| `/tasks [list\|add\|done\|set\|show]` | Manage session tasks. |
| `/mcp <list\|read\|auth>` | Manage MCP resources and auth. |
| `/skill list` | List installed Agent Skills. |
| `/skill install <source>` | Install a skill from a local path, https git URL, or `owner/repo` shorthand. |
| `/skill info <name>` | Show metadata + body excerpt for an installed skill. |
| `/skill enable <name>` / `disable <name>` | Toggle whether a skill is exposed to agents. |
| `/skill uninstall <name>` | Remove a previously installed skill. |
| `/skill where` | Show the discovery roots being scanned. |
| `/skills` | Alias of `/skill`. |
| `/hooks` | Show effective runtime hooks (project + global). |
| `/compact [focus...]` | Summarize the current conversation. |
| `/compact auto <status\|on\|off>` | Show/configure auto-compact. |
| `/compact policy` | Show compact thresholds and failure counters. |
| `/clear` | Save and clear the current conversation. |
| `/resume` | Open saved conversation picker. |
| `/init` | Analyze codebase and create/update `SPETTRO.md`. |
| `/remote` | Start the local HTTP/SSE control plane on `127.0.0.1` (default port `7878`). |
| `/remote :PORT` | Start the control plane on a specific port; falls back to a free port if it is busy. |
| `/remote stop` | Stop the running control plane. |
| `/remote status` | Print the current URL and bearer token. |

## Agent usage

- Type `@` in the input to open repository file suggestions and insert mentions.
- Use `TOOL_CALL` with `{"tool":"agent",...}` to spawn sub-agents; multiple `TOOL_CALL` lines run in parallel.
- `/approve` executes a previously generated pending plan.

## Keyboard shortcuts

| Key | Action |
| --- | --- |
| `Shift+Tab` | Cycle active mode/agent. |
| `F2` | Next favorite model. |
| `Shift+F2` | Previous favorite model. |
| `Ctrl+O` | Toggle expanded context/tool details in side panel. |
| `Ctrl+C` twice | Quit with safety confirmation. |
| `Ctrl+Q` | Quit immediately. |
| `Up` / `Down` | Navigate command suggestions and dialogs. |
| `Tab` | Move selection in dialogs/palettes. |

## Notes

- `/approve` requires a pending plan (typically produced in `plan` mode).
- In `ask-first`, coding prompts are gated by approval flow.
- Shell approval options: allow once, allow always, deny, or provide an alternative instruction.
- "Allow always" persists normalized command approvals in `.spettro/allowed_commands.json`.
- `/connect` includes `Local endpoint (LM Studio/Ollama)` and probes `/v1/models`.
- In `/models`, press `f` to toggle favorites for highlighted model.
- Pressing `Enter` on a highlighted command suggestion inserts it first; pressing `Enter` again executes it.
