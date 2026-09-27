# Commands and Keybindings

## Slash commands

| Command | Description |
| --- | --- |
| `/help` | Show in-app help text. |
| `/exit`, `/quit` | Quit Spettro. |
| `/mode`, `/next` | Cycle active manifest agent/mode. |
| `/theme` | Open the [theme](theme.md) picker: pick dark, light or auto from a list with a live preview panel. Also reports the selection, the palette it resolved to, and which source decided (env / config / detection / default). |
| `/theme <dark\|light\|auto>` | Switch palette immediately and persist it to `~/.spettro/config.json`. `auto` detects the terminal background and falls back to dark. `SPETTRO_THEME` picks the palette at startup; `/theme` still overrides it for the rest of the session. |
| `/connect` | Open provider/local-endpoint connect dialog. |
| `/login` | Sign in to a Spettro subscription (device flow). See [Subscription](subscription.md). |
| `/logout` | Sign out and remove the saved Spettro subscription key. See [Subscription](subscription.md). |
| `/models` | Open model selector dialog (connected providers). |
| `/models <provider:model> [api_key]` | Set model directly; optional API key saves for provider. |
| `/goal <objective>` | Start an autonomous goal-mode run. See [Goal Mode](goal.md). |
| `/goal stop` | Abandon the active goal and cancel any in-flight run. |
| `/goal status` | Show the current goal's iteration count, no-progress counter, and elapsed time. |
| `/goal resume` | Resume an unfinished goal from a loaded session. |
| `/loop <time> <prompt>` | Run a prompt (or slash command) on a recurring interval, e.g. `/loop 5m check CI status` or `/loop 30m /compact`. Intervals use duration syntax (`30s`, `5m`, `1h30m`, minimum `10s`); the first run fires immediately. A firing that lands while a run is still in progress is skipped, not queued. |
| `/loop stop` | Stop the recurring loop (an in-flight iteration keeps running; `Esc` interrupts it). |
| `/loop status` | Show the active loop's prompt, interval, iteration count, and next firing. |
| `/permission <ask-first\|restricted\|yolo>` | Set execution policy. |
| `/permissions [ask-first\|restricted\|yolo]` | Show or set policy alias. |
| `/permissions debug <on\|off>` | Toggle permission diagnostics in UI. |
| `/budget <n\|0>` | Set request token budget (`0` = unlimited). |
| `/thinking <off\|low\|medium\|high\|x-high\|max>` | Set the reasoning/thinking level for the active model. Maps to Anthropic's thinking token budget and to `reasoning_effort` on OpenAI and OpenAI-compatible backends. Hidden for models the catalog does not flag as reasoning-capable; if a model rejects the chosen level, Spettro silently retries at a lower one. |
| `/ultra [on\|off]` | Toggle [Ultra mode](ultra.md): the top-level agent fans hard tasks out across a swarm of parallel sub-agents (works with any model; sub-agents inherit the active model). Requires the `restricted` or `yolo` permission level — refused under `ask-first`. |
| `/workflows` | List saved [workflow](workflows.md) scripts from `.spettro/workflows` and `~/.spettro/workflows`, with their descriptions and phases. |
| `/workflows show <name>` | Print a saved workflow's header and source. |
| `/workflows run <name> [json]` | Run a saved workflow, optionally with a JSON `args` value. Dispatches a turn instructing the agent to invoke it, so the model reviews and acts on the result. |
| `/workflows where` | Show the directories scanned for saved workflows. |
| `/plan [prompt]` | Switch to `plan` mode or run a planning request directly. |
| `/approve` | Execute pending plan through `coding` agent. |
| `/tasks [list\|add\|done\|set\|show\|rm\|clear]` | Manage the session task graph. `list` prints tasks in dependency order with `deps:` and `[blocked]` markers; `set` accepts `pending`, `in_progress`, `completed`, `blocked` or `cancelled`; `rm <id>` deletes a task (stripping references to it from other tasks' dependencies); `clear` prunes all completed/cancelled tasks. |
| `/mcp <list\|read\|auth>` | Manage MCP resources and auth. |
| `/skills` (or `/skill list`) | List [Agent Skills](skills.md): who can run each, its source folder and `SKILL.md` path, shadowed skills and warnings. |
| `/<skill-name> [args]` | Run a skill: its instructions, with the arguments substituted, become the prompt. Listed in the `/` menu with the skill's description; built-in and custom commands win a name clash. `$<skill-name>` in any prompt pulls the skill in too. |
| `/skill install <source>` | Install a skill from a local path, https git URL, or `owner/repo` shorthand into `~/.spettro/skills` (`--project`: `.spettro/skills`). |
| `/skill info <name>` | Show metadata, bundled files and the start of the instructions. |
| `/skill enable <name>` / `disable <name>` | Show or hide a skill for the agent and the `/` menu (saved as `disabled_skills` in `config.json`). |
| `/skill uninstall <name>` | Remove a skill installed in `.spettro/skills` (Claude Code / Codex folders are never written to). |
| `/skill where` | Show the discovery folders, in priority order. |
| `/skill reload` | Force a re-scan of the skill folders (changes on disk are otherwise picked up on the next use). |
| `/stats` | Show session token usage and prompt-cache metrics. |
| `/hooks` | Show effective runtime hooks (project + global). |
| `/memory [show]` | Show persistent cross-session memory (user + project). See [Persistent Memory](memory.md). |
| `/memory edit [user\|project]` | Edit a memory file in `$EDITOR`. |
| `/memory clear [user\|project\|all]` | Erase saved memory. |
| `/memory mine [n]` | Scan recent saved sessions in the background and draft candidate memories into the review inbox. |
| `/memory review` | Approve or discard drafted memory candidates (nothing saves without approval). |
| `/compact [focus...]` | Compact the current conversation (two-stage: offload oversized tool results to re-readable references, then summarize). |
| `/compact auto <status\|on\|off>` | Show/configure auto-compact. |
| `/compact policy` | Show compact thresholds and failure counters. |
| `/clear` | Save and clear the current conversation. |
| `/diff [path...]` | Show diffs of files modified this session (all, or given paths). |
| `/resume` | Open saved conversation picker. |
| `/rewind` | Restore files and/or conversation to a pre-edit checkpoint. See [Checkpointing](checkpointing.md). |
| `/checkpoints` | Show checkpoint count and shadow-store disk usage (this project and all projects). |
| `/storage` | Report what Spettro stores on disk, per artifact class. See [Storage](storage.md). |
| `/storage clean` | Interactive multi-select cleanup with safe defaults preselected; also available headless as `spettro clean`. |
| `/init` | Analyze codebase and create/update `SPETTRO.md`. |
| `/jobs [list]` | List background shell jobs started by the agent. |
| `/jobs kill <id\|all>` | Terminate a background job (or all of them). |
| `job-output` | Tool-only (not a slash command): fetches accumulated output of a background shell job (`job-N`), or pages through a **spooled** truncated tool result (`spool:N`). See [Session Lifecycle](session.md#tool-output-spooling). |
| `tool-output` | Tool-only: re-reads the full output of an earlier tool call that was offloaded to the session spool (by truncation or by compaction), with `id`/`offset`/`limit` paging. See [Session Lifecycle](session.md#tool-output-spooling). |
| `pty-start` / `pty-write` / `pty-kill` | Tool-only: interactive terminal sessions (REPLs, debuggers, ssh, watch-mode servers) the agent can type into. See [Interactive PTY sessions](pty.md). |
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
- Use the native `agent` tool to spawn sub-agents; multiple calls in one response run in parallel.
- `/approve` executes a previously generated pending plan.

## Keyboard shortcuts

| Key | Action |
| --- | --- |
| `Shift+Tab` | Cycle active mode/agent. |
| `F2` | Next favorite model. |
| `Shift+F2` | Previous favorite model. |
| `Ctrl+O` | Toggle transcript tool details (trimmed outputs). In an approval dialog: expand or collapse the preview. |
| `Ctrl+G` | Toggle full (untrimmed) tool outputs; implies details visible. |
| `PgUp` / `PgDn` | Scroll the transcript a page at a time (scrolling back to the bottom resumes following new output). In an approval dialog: scroll the preview (a file change's diff, or a long command) instead. |
| `Ctrl+C` twice | Quit with safety confirmation. |
| `Ctrl+Q` | Quit immediately. |
| `Ctrl+V` | Paste image from clipboard (vision-capable models only). |
| `Ctrl+F` | Attach a workspace file path to your next prompt. |
| `Ctrl+R` | Remove the most recent attachment. |
| `Ctrl+Y` | Copy the last assistant response to clipboard. |
| `Ctrl+T` | Toggle text-select mode (mouse capture on/off). |
| `Up` / `Down` | Navigate command suggestions and dialogs. |
| `Tab` | Move selection in dialogs/palettes. |
| `Esc` | Interrupt the current agent run (stops and abandons goals and loops). |
| `Esc Esc` | Open the rewind checkpoint picker (when idle). See [Checkpointing](checkpointing.md). |

## Notes

- While a run streams, the status bar shows a live ticker: elapsed time and tokens streamed.
- Diffs highlight the changed words *within* modified lines, in both unified and side-by-side layouts.
- `/approve` requires a pending plan (typically produced in `plan` mode).
- In `ask-first`, coding prompts are gated by approval flow.
- Shell approval options: allow once, allow always, deny, or provide an alternative instruction.
- The approval dialog always fits the terminal with every option visible, down to 40x15. It shows the call on one summary row (for a `file-write`/`file-edit` to a long path, the middle of the path is cut, so the file name stays on the row even when the terminal has no room for the diff), then a preview when there is more to see: the diff of a `file-write`/`file-edit`, or the full text of anything too long for the summary row: a command (a heredoc, say), or the whole target of a network call (the URL of a `web-fetch` or `download`, query string included). The preview is capped (16 diff lines, 8 command lines) until `Ctrl+O` expands it to every row the terminal can spare, and `PgUp`/`PgDn` scroll it; a footer row says which lines are on screen (`lines 1-8 of 3002`), or that the preview is hidden when the terminal is too short for it. Control characters in a command or a diff are shown rather than interpreted (`^M` for a carriage return, `^[` for an escape), and so are characters that print nothing but can reorder or hide text (bidi overrides, zero-width characters, the byte-order mark, shown as `\u202e`-style escapes) and bytes that are not valid UTF-8 (`\x9b`), so nothing the agent sends can hide part of what you approve.
- Tool calls and outputs of any size stay inside the terminal. A tool's header row is one line (a multi-line command is folded onto it, a long path is cut from the left so the file name stays visible); output lines wider than the transcript are cut with `…` and a `long lines cut · ctrl+g for full output` note, and `Ctrl+G` shows them whole, wrapped. Escape sequences in output (and in model replies and the activity panel's details) are dropped, bytes that are not valid UTF-8 show as `�`, and progress meters that redraw with carriage returns show their final state. After a resize the transcript is redrawn at the new width. A call of a tool you defined yourself (see [Tools of your own with a built-in's name](tools.md#tools-of-your-own-with-a-built-ins-name)) is labelled with its own name, never as the built-in it shares a name with.
- With the activity panel open (`Ctrl+B`), the task list moves into the panel (the footer under the transcript is not drawn then); it shows live tasks first and counts completed ones. The panel needs a terminal at least 110 columns wide and 15 rows tall; on a smaller one it is not drawn until the terminal grows again. On a short terminal the footer shrinks, down to nothing, so the transcript keeps at least a row (three on a normal terminal) and whatever is in the input box keeps the rows it needs: the text input during a run, the steer or plan picker, an approval or a question.
- Markdown tables wider than the transcript have their widest columns narrowed (cells cut with `…`) instead of wrapping, and code-block lines are cut with `…` rather than clipped silently. Wrapped list items and quotes keep their indent (a list item's text lines up under its first word, nested items stay nested, a quote keeps its `│` bar on every row), and an underscore inside a word is literal, so `snake_case` names and paths like `file_name.go` are not turned into italics.
- The full-screen pickers (`/models`, `/connect`, `/resume`, `/theme`, the onboarding model list) fit the terminal whole, borders and key hints included, down to 40x15: rows too long for the dialog are cut with `…`, the key hints wrap onto a second row when they do not fit one, and a list that scrolls says how much is above and below it (`↑ 3 more`, `↓ 12 more`).
- Command output in the transcript (`/help`, `/skills`, `/stats`, ...) that is too long for the pane wraps under its second column, so a wrapped description never reads as a new entry.
- "Allow always" persists normalized command approvals in `.spettro/allowed_commands.json`.
- `/connect` includes `Local endpoints (LM Studio/Ollama/llama.cpp/…)` and probes `/v1/models`. Multiple local endpoints can be connected side by side; each can optionally carry an API key (for servers started with authentication, e.g. `llama-server --api-key`), and existing endpoints can be managed (edit key / remove) from the same dialog.
- In `/models`, press `f` to toggle favorites for highlighted model.
- Pressing `Enter` on a highlighted command suggestion inserts it first; pressing `Enter` again executes it. When what you typed is a whole command name (`/clear`, `/skills`) it is the highlighted suggestion and `Enter` runs it at once: suggestions are ordered by how well their name matches (exact name, then names starting with what you typed, then names containing it, then commands matched only by their description), and every change to the input moves the highlight back to the top.
- `/goal` runs the **coding** orchestrator autonomously. Interrupt with `Esc` or `/goal stop`. Permission `yolo` is required for fully unattended operation; otherwise approval prompts pause the loop. See [Goal Mode](goal.md).
- A command's output (`/skills`, `/jobs`, `/tasks`, ...) shows in the transcript as soon as the command runs.
- `/clear` **saves** the session first, then starts fresh. The saved session is available via `/resume`. See [Session Lifecycle](session.md).
- `/compact` compacts the conversation in two stages: old oversized tool outputs are first replaced with `[output elided: …]` stubs the agent can re-read via `tool-output`, then the older turns are replaced by a structured summary. The original task, the latest user messages and the most recent tool calls are always kept verbatim. Auto-compact triggers at 85 % context window by default and skips the summary entirely when pruning alone frees enough space. See [Session Lifecycle](session.md).
- `/login` and `/logout` manage your Spettro Subscription. See [Subscription](subscription.md).
- Clipboard pasting (`Ctrl+V`), file attachments (`Ctrl+F`), and text-select mode (`Ctrl+T`) are described in [Clipboard and Attachments](clipboard.md).
- The first-launch onboarding wizard is documented in [Onboarding](onboarding.md).
- Runtime hooks (`/hooks`) are documented in [Runtime Hooks](hooks.md).
- User-defined slash commands (reusable prompt files with `{{args}}` and shell interpolation) are documented in [Custom Slash Commands](custom-commands.md).
