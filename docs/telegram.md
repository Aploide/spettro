# Telegram relay (`/telegram` / `/tg`)

Spettro can drive a session from a Telegram chat: send prompts from your
phone, receive live agent output, answer ask-user questions and interrupt
runs without sitting in front of the TUI.

The relay is purely opt-in. Nothing connects to Telegram until you run
`/telegram setup` inside the TUI and `/telegram start`. The bot token is
stored encrypted alongside provider API keys; the allowlist lives in
plain JSON under `~/.spettro/telegram.json`.

## Quickstart

1. Talk to [@BotFather](https://t.me/BotFather) on Telegram and create
   a new bot. BotFather replies with an API token like
   `123456:ABC-DEF1234ghIkl-zyx57W2v1u123ew11`.
2. In Spettro, run:
   ```
   /telegram setup 123456:ABC-DEF1234ghIkl-zyx57W2v1u123ew11
   ```
   Spettro validates the token (`getMe`), saves it encrypted and prints
   the bot's `@username`.
3. Authorize yourself — pick whichever you prefer:
   ```
   /telegram allow @your_username     # by Telegram username
   /telegram allow 123456789          # by numeric user / chat id
   ```
   Both `/telegram` and `/tg` are accepted; pick the shorter alias.
4. Start polling:
   ```
   /telegram start
   ```
5. Open your bot in Telegram (`t.me/<bot_username>`), send any message.
   The relay forwards it to the active agent and echoes the assistant's
   reply back to the chat.

On the next launch, Spettro autostarts the relay because `/telegram start`
flips an `auto_start` flag in `~/.spettro/telegram.json`. Disable it with
`/telegram stop`.

## Command reference

### From the Spettro TUI

| Command | Behavior |
| --- | --- |
| `/telegram setup <token>` | Validate + store the BotFather token, print the bot's `@username`. |
| `/telegram token <token>` | Replace the stored token. Same probe as `setup`. |
| `/telegram start` | Start the long-poll worker. Persists `auto_start = true`. |
| `/telegram stop` | Stop polling. Persists `auto_start = false`. |
| `/telegram restart` | Stop and start. Useful after editing the allowlist while running. |
| `/telegram status` | Print bot, allowlist, currently bound chats, last error. |
| `/telegram allow <@u\|id>` | Add a username or chat id to the allowlist. |
| `/telegram deny  <@u\|id>` | Remove a username or chat id. Aliases: `remove`, `revoke`. |
| `/telegram list` | Print the current allowlist. |
| `/telegram reset` | Forget the token and clear `telegram.json`. |
| `/tg ...` | Alias of `/telegram`. |

The allowlist accepts:
- `@username` — case-insensitive Telegram username, with or without the
  leading `@`.
- A numeric chat id — Telegram user ids are positive; supergroup /
  channel ids are negative.

Entries are deduplicated and sorted on save (numeric ids first, then
usernames alphabetically).

### From inside Telegram

When messaging the bot:

| Message | Effect |
| --- | --- |
| Plain text (no leading `/`) | Treated as a new prompt for the active agent. |
| `/plan ...`, `/approve`, `/models ...`, etc. | Treated as a Spettro slash command (subject to the usual "not while thinking" rules). |
| Plain text **while Spettro is awaiting an ask-user answer** | Routed as the answer to the pending dialog. Spettro confirms in the TUI. |
| `/cancel` (or `/stop`) | Interrupt the currently running agent (equivalent to pressing Esc). |
| `/whoami` | Print the bot's identity and allowlist size. |
| `/help`, `/start` | Print the bot-side cheat sheet. |

Bot-side commands like `/cancel` and `/help` are handled by the relay
itself — they never reach Spettro's agent loop.

## What gets forwarded

The relay forwards a curated subset of Spettro's internal event stream so
the chat stays useful rather than noisy:

| Forwarded | Emoji / prefix |
| --- | --- |
| Final assistant message | `🤖` |
| Plan output | `📋 plan` |
| Progress comments (via the `comment` tool) | `💬` |
| Banners with level `warn` / `error` | `⚠️` |
| Agent errors | `⚠️ error` |
| Ask-user dialog | `❓` + options + “reply with your answer” hint |
| Shell-approval request | `🔐` + the command (handle inside the TUI) |
| Successful commits | `🟢 commit` |
| Generated image (`grok-image`) | `sendPhoto` with the prompt as caption (`🖼 …`); falls back to `sendDocument` when the file is over Telegram's 10 MB photo cap. |
| Generated video (`grok-video`) | `sendVideo` with the prompt as caption (`🎬 …`); falls back to `sendDocument` over 50 MB. |

Tool traces (e.g. every `file-write` and `shell-exec`) are **not**
forwarded by default to keep the chat readable. State changes
(run start, run done) are silent unless the `verbose` flag is set in
`telegram.json`.

Outbound messages longer than ~3800 characters are split at line/word
boundaries and sent as a sequence of chunks with `(...cont)` / `…
(continued)` markers.

## Authentication and allowlist semantics

Every incoming Telegram update is checked against the allowlist:

- The username on the sender (`message.from.username`) is matched
  case-insensitively against `@username` entries.
- The numeric user id (`message.from.id`) and chat id
  (`message.chat.id`) are matched against numeric entries.
- If neither matches, the relay replies with a polite "this chat is
  not allowed" note and ignores the message.

Once an authorized chat sends its first message, the relay remembers
the chat id for the remainder of the session and forwards outbound
events to it. Re-binding happens on the next message after a stop /
start.

## Storage

- `~/.spettro/keys.enc` — the BotFather token, encrypted under the
  same master secret as your provider API keys
  (see [`configuration.md`](configuration.md)).
- `~/.spettro/telegram.json` — non-secret state:
  ```json
  {
    "bot_username": "MyBot",
    "allowlist": [
      {"chat_id": 123456789},
      {"username": "carlo"}
    ],
    "auto_start": true,
    "last_update_id": 42
  }
  ```

`last_update_id` is the offset for `getUpdates` so a restart does not
replay messages you already processed.

## Failure modes

| Symptom | Likely cause |
| --- | --- |
| `telegram: token rejected — 401 Unauthorized` | Wrong / revoked BotFather token. Recreate with `@BotFather` and rerun `/telegram setup`. |
| `🚫 This chat is not allowed to drive Spettro.` (in Telegram) | The sender's username/id is not on the allowlist. Run `/telegram allow ...` in the TUI. |
| Messages go in but no reply | Check `/telegram status` for `last error` / `last send`. The relay may be backing off after a transient API failure. |
| `telegram autostart failed: ...` on launch | Network down or token revoked. Run `/telegram start` manually once the issue is resolved. |
| Bot replies are arriving in the wrong chat | Multiple chats messaged the bot during the same session. The relay broadcasts to every bound chat; remove unwanted entries from the allowlist or `/telegram restart`. |

## Security notes

- The relay never logs the bot token. Errors that include the Bot API
  URL are sanitised to `/bot***/<method>`.
- All API calls go to `api.telegram.org`. The relay deletes any
  registered webhook on start so long-polling can't conflict with
  another deployment.
- The allowlist is authoritative: nothing happens until at least one
  entry matches. There is no public discovery flow.
- The relay does not surface shell-approval decisions remotely — those
  must still be answered in the TUI. Ask-user dialogs *can* be
  answered from Telegram via plain text.
