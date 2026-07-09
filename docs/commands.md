# Commands and Keybindings

## Slash commands

| Command | Description |
| --- | --- |
| `/help` | Show in-app help text. |
| `/exit`, `/quit` | Quit Spettro. |
| `/mode`, `/next` | Cycle active manifest agent/mode. |
| `/connect` | Open provider/local-endpoint connect dialog. |
| `/login` | Sign in to a Spettro subscription (device flow). See [Subscription](subscription.md). |
| `/logout` | Sign out and remove the saved Spettro subscription key. See [Subscription](subscription.md). |
| `/models` | Open model selector dialog (connected providers). |
| `/models <provider:model> [api_key]` | Set model directly; optional API key saves for provider. |
| `/goal <objective>` | Start an autonomous goal-mode run. See [Goal Mode](goal.md). |
| `/goal stop` | Abandon the active goal and cancel any in-flight run. |
| `/goal status` | Show the current goal's iteration count, no-progress counter, and elapsed time. |
| `/goal resume` | Resume an unfinished goal from a loaded session. |
| `/permission <ask-first\|restricted\|yolo>` | Set execution policy. |
| `/permissions [ask-first\|restricted\|yolo]` | Show or set policy alias. |
| `/permissions debug <on\|off>` | Toggle permission diagnostics in UI. |
| `/budget <n\|0>` | Set request token budget (`0` = unlimited). |
| `/thinking <off\|low\|medium\|high\|x-high\|max>` | Set extended-thinking compute budget for the active model. Honoured by Anthropic Claude Opus / Sonnet; ignored by providers that don't expose a thinking parameter. |
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
| `/remote local` | Start the LAN HTTP/SSE control plane on `0.0.0.0` (default port `7878`). |
| `/remote local :PORT` | Start the LAN control plane on a specific port; falls back to a free port if it is busy. |
| `/remote stop` | Stop the running control plane. |
| `/remote status` | Print the current URL and bearer token. |
| `/telegram setup <token>` | Save a Telegram BotFather token (encrypted) and validate it via `getMe`. Alias: `/tg`. |
| `/telegram allow <@u\|id>` | Add a Telegram username or chat ID to the allowlist. |
| `/telegram start` / `/telegram stop` | Start or stop the relay's long-poll worker. Autostarted on next launch when previously running. |
| `/telegram status` / `/telegram list` | Print runtime state, bound chats and allowlist. |
| `/telegram deny <@u\|id>` / `/telegram reset` | Remove an allowlist entry or wipe the entire relay configuration. |
| `/<custom> [args]` | Run a user-defined command from `~/.spettro/commands/` or `<root>/.spettro/commands/`. See [Custom Slash Commands](custom-commands.md). |

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
| `Ctrl+V` | Paste image from clipboard (vision-capable models only). |
| `Ctrl+F` | Attach a workspace file path to your next prompt. |
| `Ctrl+R` | Remove the most recent attachment. |
| `Ctrl+Y` | Copy the last assistant response to clipboard. |
| `Ctrl+T` | Toggle text-select mode (mouse capture on/off). |
| `Up` / `Down` | Navigate command suggestions and dialogs. |
| `Tab` | Move selection in dialogs/palettes. |
| `Esc` | Interrupt the current agent run (stops and abandons goals). |

## Notes

- `/approve` requires a pending plan (typically produced in `plan` mode).
- In `ask-first`, coding prompts are gated by approval flow.
- Shell approval options: allow once, allow always, deny, or provide an alternative instruction.
- "Allow always" persists normalized command approvals in `.spettro/allowed_commands.json`.
- `/connect` includes `Local endpoint (LM Studio/Ollama)` and probes `/v1/models`.
- In `/models`, press `f` to toggle favorites for highlighted model.
- Pressing `Enter` on a highlighted command suggestion inserts it first; pressing `Enter` again executes it.
- `/goal` runs the **coding** orchestrator autonomously. Interrupt with `Esc` or `/goal stop`. Permission `yolo` is required for fully unattended operation; otherwise approval prompts pause the loop. See [Goal Mode](goal.md).
- `/clear` **saves** the session first, then starts fresh. The saved session is available via `/resume`. See [Session Lifecycle](session.md).
- `/compact` replaces the transcript with a summary. Auto-compact triggers at 85 % context window by default. See [Session Lifecycle](session.md).
- `/login` and `/logout` manage your Spettro Subscription. See [Subscription](subscription.md).
- Clipboard pasting (`Ctrl+V`), file attachments (`Ctrl+F`), and text-select mode (`Ctrl+T`) are described in [Clipboard and Attachments](clipboard.md).
- The first-launch onboarding wizard is documented in [Onboarding](onboarding.md).
- Runtime hooks (`/hooks`) are documented in [Runtime Hooks](hooks.md).
- User-defined slash commands (reusable prompt files with `{{args}}` and shell interpolation) are documented in [Custom Slash Commands](custom-commands.md).
