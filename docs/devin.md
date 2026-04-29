# Devin Sessions provider

Spettro can drive [Cognition's Devin](https://devin.ai) agent sessions
through the same UI it uses for Anthropic, OpenAI, and other LLM
providers. Picking the **Devin Session** model in `/models` makes every
prompt create a fresh Devin session, polls it until it reaches a terminal
status, and surfaces the final agent message back into the conversation.

## What's different about this provider

Devin is an *agent runner*, not a chat-completion endpoint:

- **Model selection happens on Devin's side.** The `--model opus` /
  `--model sonnet` / `--model swe` choices documented in the Devin CLI
  are owned by your Devin account and are not selectable per-call from
  Spettro. Whichever model is the default for your account is what Devin
  will use for the session.
- **Thinking levels are owned by Devin too.** Spettro's `/thinking`
  setting is accepted on the call but ignored by the Devin backend.
  Toggle thinking inside Devin's UI / CLI instead (`Alt+T` /
  `Opt+T` in `devin`).
- **Latency is multi-minute.** Devin sessions can take 10+ minutes for
  non-trivial tasks. Spettro blocks (with `/interrupt`-aware cancellation)
  on the session until it reaches a terminal state, then returns the last
  assistant message together with a footer linking the session in
  `app.devin.ai`.
- **Billing is in ACUs, not tokens.** Token counters in the status bar
  will report `0` for Devin runs. Use the Devin dashboard for ACU
  consumption.

## Authentication

Both Devin API generations are supported. The adapter auto-detects which
one to use based on the prefix of your API key:

| API generation | Key prefix | Endpoint base | Org id needed |
| --- | --- | --- | --- |
| **v3** (Service Users + RBAC, current) | `cog_*` | `POST /v3/organizations/{org_id}/sessions` | yes |
| **v1** (Personal/Service API keys, legacy) | `apk_*` | `POST /v1/sessions` | no |

To configure:

```text
/connect                   # save the cog_/apk_ key as the api_keys.devin entry
/devin org-abc123def456    # only needed for cog_ keys
/models devin:session
```

The full key is stored encrypted in `keys.enc`; only the `cog_` /
`apk_` prefix and a short hash are visible in the logs. The org id is
stored as plaintext in `config.json` (it is not a secret).

## How a turn works

1. Spettro POSTs `{"prompt": "...", "tags": ["spettro"], "unlisted": true}`
   to the right `sessions` endpoint and gets back a session id and url.
2. Spettro polls the session (every 5 seconds by default) until it hits
   a terminal status:
   - **v3**: `status` is one of `exit`, `error`, `suspended`, or
     `running` with `status_detail = "finished"`.
   - **v1**: `status_enum` is one of `finished`, `expired`, `blocked`.
3. On terminal, Spettro reads the latest `source: "devin"` message
   (v3) or the last non-user message in the inline `messages[]` array
   (v1). If the session produced a structured output it is preferred
   over free-text messages.
4. The full response is returned as the assistant message in your
   conversation, with a `— Devin session: <url>` footer so you can open
   the run in `app.devin.ai`.

## Cancelling a long-running session

Spettro's normal `/interrupt` (or pressing `Esc`) cancels the polling
goroutine. The Devin session itself keeps running on Cognition's side —
use the Devin dashboard or `devin` CLI to stop it there if needed.

## Limitations

- Attachments (`attachment_urls`), repos, knowledge ids, secrets, and
  playbooks are not yet wired through the Spettro provider; if you need
  those, create the session in Devin's UI/CLI directly.
- ACU consumption is not surfaced in the Spettro status bar.
- Image inputs are not forwarded — Devin sessions don't accept inline
  images via the v1 / v3 create endpoint anyway.

## Related commands

- `/connect` — save or replace the Devin API key.
- `/devin <org-id>` — show or set the v3 organization id.
- `/models devin:session` — make Devin the active model.
- `/thinking <level>` — accepted but ignored by Devin sessions.
