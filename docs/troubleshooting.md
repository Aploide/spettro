# Troubleshooting

## "Setup required" or provider errors

- Run `/connect` and verify your API key.
- Run `/models` and ensure selected model exists for that provider.
- If using local endpoint, verify it responds to `/v1/models`.

## "No providers connected"

- Use `/connect` and choose a provider.
- For LM Studio/Ollama/llama.cpp/vLLM, choose `Local endpoints (LM Studio/Ollama/llama.cpp/…)` and enter the endpoint (for example `localhost:1234`). You can add several endpoints; when prompted for an API key, press enter to skip it unless the server requires one.
- Then use `/models` and select a model.

## Token budget exceeded or context blocked

- Increase budget with `/budget <n>` or disable limit with `/budget 0`.
- Run `/compact` (or `/compact <focus...>`) to reduce active context size.
- Check `/compact policy` for thresholds and failure counters.
- Toggle auto-compaction with `/compact auto on|off`.

## Hooks not running as expected

- Run `/hooks` to inspect merged global+project rules and warnings.
- Verify event names are exactly: `PreToolUse`, `PostToolUse`, `PermissionRequest`, `SessionStart`.
- Confirm matcher patterns target the tool IDs you expect (`bash`, `file-edit`, etc.). A matcher written for `shell-exec` still fires on `bash`; one for a narrower retired name (`multi-edit`, `ls`, `task-*`, ...) fires only when the model calls that name. The `tool_id` your script receives is always the canonical name.
- Hook commands must exit with code `0` to be treated as successful.

## `/approve` does nothing useful

- Ensure a plan was generated first.
- Ensure there is a pending plan.
- Check current permission with `/permission` or `/permissions`.

## Unknown agent / delegation failures

- Verify agent IDs exist in `spettro.agents.toml`.
- Check `handoffs` and `allowed_tools` references.
- Fix manifest validation issues and restart.

## Manifest validation error

- Fix unknown TOML fields, duplicate IDs, invalid permission values, or missing references.
- See `AGENTS.md` for schema/validation.
- Remove or rename broken `spettro.agents.toml` to use built-ins.

## Search/suggestions feel slow in large repos

- Mention suggestions and repository scans may slow down in very large trees.
- Narrow prompts and use focused file mentions (`@path/to/file`).

## Approval prompts lack detail

- Enable diagnostics with `/permissions debug on`.
- Use `/permissions` to inspect active rules and recent decisions.

## Text is unreadable on a light terminal

- Run `/theme light`. It repaints immediately and persists the choice.
- Run bare `/theme` to open the picker and preview each palette, and to see what Spettro detected and which source decided it.
- `auto` degrades to the dark palette whenever the terminal's background cannot
  be determined — a terminal that ignores the OSC 11 query and sets no
  `COLORFGBG`, output redirected to a file, or `TERM=dumb`.
- To force a palette without saving it, set `SPETTRO_THEME=light` (or `dark`).
- See [Themes](theme.md) for the full precedence and detection rules.

## The agent stops early or ends without doing anything

The run loop does not accept every reply without tool calls as the final
answer. Each case below gets a bounded nudge (a short user message asking
the model to go on), reported in the transcript as a note:

- An empty reply is nudged up to twice; a third ends the turn with an error.
- A short reply that only announces work ("I'll start by exploring the
  repository...") before the turn has made any tool call is nudged once.
  A reply that asks the user something (it contains `?` or the word "you")
  or introduces an answer with a colon ("Let me explain: ...") is never
  treated as an announcement, so a clarifying question still ends the turn
  and waits for the user.
- A reply whose finish reason says it stopped for tool calls, but which
  carries none, is nudged once to send the call again.

A second announce-only reply in the same turn ends it, and so does a second
dropped-call reply that carries text. A second dropped-call reply with no
text is an empty reply, so it gets the empty-reply nudges above. None of
these can loop. To see what the provider actually returned, set
`SPETTRO_DEBUG_LOG` to a file path before starting spettro (TUI, `--acp` or
`--goal`):

```bash
SPETTRO_DEBUG_LOG=/tmp/spettro-debug.log spettro --goal "fix the failing test"
```

Every reply is then logged at debug level with its normalized and raw
finish reason, the number of tool calls parsed and seen in the stream
(unnamed calls and orphan argument fragments included), text and reasoning
sizes, output tokens and the first characters of the text. Without the
variable nothing is logged.

## A local server or OpenAI-compatible provider misbehaves

Streamed requests to OpenAI-compatible endpoints (catalog providers with an
OpenAI-style API, the Spettro Subscription, local servers) go through
Spettro's own chat-completions client. To rule it out, send them through the
fantasy SDK it replaced, for one run or for good:

```bash
SPETTRO_PROVIDER_WIRE=fantasy spettro    # one run
```

or set `"provider_wire": "fantasy"` in `~/.spettro/config.json` (see
[Configuration](configuration.md#provider-wire)). If the problem goes away,
please report it with the debug log described above.

## Reset local state

If needed, remove local Spettro state:

```bash
rm -rf .spettro
rm -rf ~/.spettro/config.json ~/.spettro/keys.enc ~/.spettro/trusted.json ~/.spettro/sessions
```

Only do this if you are comfortable losing local session history, trusted paths, and encrypted keys.
