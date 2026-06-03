# Remote control plane (`/remote`)

Spettro can expose a small local HTTP/SSE control plane so that an external
process — a script, an editor extension, another TUI — can drive a running
session: submit prompts, stream live progress (tool calls, comments, agent
output, banners, approval/ask-user prompts) and request an interrupt.

The control plane is purely opt-in. It only starts when you run `/remote`
inside the TUI. By default it binds to `127.0.0.1`; use `/remote local` to
bind to `0.0.0.0` so other devices on your LAN can reach it. It is gated by a
per-session bearer token printed to the chat when it starts.

## Starting and stopping

| Command | Behavior |
| --- | --- |
| `/remote` | Bind on `127.0.0.1:7878`. If that port is busy, scan upward (`7879`, `7880`, …) for ten attempts before letting the OS pick a free port. |
| `/remote :PORT` | Try the requested port first. If it is busy, fall back to an OS-assigned free port and warn you in the banner. |
| `/remote local` | Bind on `0.0.0.0:7878` (LAN-accessible). If that port is busy, scan upward (`7879`, `7880`, …) for ten attempts before letting the OS pick a free port. |
| `/remote local :PORT` | Try the requested port first on `0.0.0.0`. If it is busy, fall back to an OS-assigned free port and warn you in the banner. |
| `/remote stop` (or `off`, `shutdown`) | Stop the server, close all live SSE connections, and free the port. |
| `/remote status` | Print the current URL and bearer token without restarting anything. |

When the server is bound, the TUI prints a system message that contains:

- the URL (`http://127.0.0.1:<port>` or the LAN IP printed for `/remote local`)
- the bearer token (a fresh random hex string per `/remote` invocation)
- a quick reference for every endpoint

The token is shown **only once in the chat**. Copy it before starting your
client. If you lose it, run `/remote status` to print it again, or
`/remote stop` followed by `/remote` to mint a new one.

## Authentication

Every endpoint requires the bearer token. Either:

- send it as `Authorization: Bearer <token>`
- or pass it as a `?token=<token>` query string (handy for curl experiments)

Anything else returns `401 unauthorized`.

By default the server only listens on the loopback interface, so it cannot be
reached from another machine. When you opt into `/remote local`, it binds to
`0.0.0.0` and becomes reachable from other devices on the same network, so
ensure your LAN is trusted and keep the bearer token private. The token
still prevents same-machine cross-origin attacks (browsers cannot set the
`Authorization` header without an explicit CORS preflight, and the server
never enables CORS).

## Endpoints

### `POST /messages`

Submit a prompt or slash command exactly as if the user typed it. If you used
`/remote local`, replace `127.0.0.1` in the examples below with the LAN IP
shown in the banner.

```http
POST /messages
Authorization: Bearer <token>
Content-Type: application/json

{
  "message": "explain the LLM runtime architecture"
}
```

Response:

```json
{
  "accepted": true,
  "queued": false,
  "note": "running"
}
```

Behavior:

- Plain text → routed through `handlePrompt` and starts an agent run with the
  current mode/model.
- Text starting with `/` → executed as a slash command via `handleCommand`.
- If an agent is already running, the message is queued and the response
  carries `"queued": true`. Slash commands are **never** queued — the
  endpoint returns `409 conflict` with an `error` field instead.
- Empty messages return `400 bad request`.

### `GET /events`

Subscribe to a live event stream over Server-Sent Events
(`text/event-stream`). Newly connected clients receive a short replay
buffer (last ~64 events) before live events resume.

```http
GET /events
Authorization: Bearer <token>
Accept: text/event-stream
```

Each event is delivered with the SSE structure:

```
event: <kind>
id: <monotonic-seq>
data: {"seq":42,"kind":"...","at":"2024-04-27T10:34:12.123Z","data":{...}}
```

Heartbeats (`: ping`) are emitted every 15 seconds so intermediaries do not
close idle connections.

### `GET /status`

Return a JSON snapshot of the runtime:

```json
{
  "thinking": true,
  "mode": "coding",
  "active_agent": "coding",
  "session_id": "session-abc",
  "messages_count": 12,
  "tokens_used": 8421,
  "started_at": "2024-04-27T10:30:00Z"
}
```

The snapshot is updated whenever the TUI publishes a `state` event (start of
a run, end of a run, mode change, etc.).

### `POST /interrupt`

Cancel the current agent run and unblock anything queued behind it. No
body is required.

```json
{ "ok": true }
```

This is equivalent to pressing `Esc` while a run is in flight inside the
TUI; pending shell-approval and ask-user prompts are dismissed.

### `GET /`

Returns a static index of the available endpoints. Useful for debugging.

## Event reference

All events share the envelope:

```json
{
  "seq": 17,
  "kind": "...",
  "at": "RFC3339 timestamp",
  "data": { ... }
}
```

| Kind | Emitted when | Payload |
| --- | --- | --- |
| `remote_started` | `/remote` invoked | `port`, `requested`, `fell_back`, `default_port`, `started_at` |
| `remote_stopped` | `/remote stop` invoked | `address` |
| `state` | Mode change, agent start/stop, etc. | `thinking`, `mode`, `active_agent`, `session_id`, `messages_count`, `tokens_used`, `reason` |
| `user_message` | Local or remote user prompt accepted | `content`, `mentioned_files` |
| `system_message` | Internal info/error message added to the chat | `content` |
| `assistant_message` | Agent run completed successfully | `content`, `thinking`, `meta`, `tools_count`, `tokens_used` |
| `assistant_error` | Agent run failed | `error` |
| `plan` | Plan agent produced a draft | `plan`, `tools_count`, `tokens_used` |
| `plan_error` | Plan agent failed | `error` |
| `comment` | Agent published a progress comment via the `comment` tool | `message` |
| `tool` | Any tool started/finished | `name`, `status` (`running`/`success`/`error`), `agent`, `args`/`args_raw`, `output` |
| `banner` | UI banner shown (info/warn/error/success) | `text`, `level` |
| `approval_request` | Shell approval is needed | `command`, `tool_id`, `segments`, `reason` |
| `ask_user` | The agent invoked `ask-user` | `question`, `options`, `context`, `default`, `allow_free_response` |
| `commit` / `commit_error` | Auto-commit agent finished | `message` / `error` |
| `search` / `search_error` | Repo searcher finished | `result` / `error` |
| `remote_command` | Remote client sent a slash command | `command` |
| `remote_prompt` | Remote client sent a plain prompt | `prompt` |
| `remote_interrupt` | `/interrupt` was received | `thinking` (was a run active?) |

The `kind` field is also reflected as the `event:` SSE name for clients
that filter by event name.

## Quick examples

### `curl` — submit a prompt

```bash
TOKEN=...     # paste from /remote output
PORT=7878
curl -sS -X POST "http://127.0.0.1:$PORT/messages" \
    -H "Authorization: Bearer $TOKEN" \
    -H "Content-Type: application/json" \
    -d '{"message":"summarize the open PRs"}'
```

### `curl` — follow the event stream

```bash
curl -N "http://127.0.0.1:$PORT/events" \
    -H "Authorization: Bearer $TOKEN"
```

### Python — print every event as it arrives

```python
import json, requests
TOKEN = "..."
PORT = 7878

with requests.get(
    f"http://127.0.0.1:{PORT}/events",
    headers={"Authorization": f"Bearer {TOKEN}"},
    stream=True,
) as r:
    for raw in r.iter_lines(decode_unicode=True):
        if raw and raw.startswith("data: "):
            ev = json.loads(raw[6:])
            print(ev["kind"], ev.get("data"))
```

### Node — interrupt the current run

```js
await fetch(`http://127.0.0.1:${PORT}/interrupt`, {
  method: "POST",
  headers: { Authorization: `Bearer ${TOKEN}` },
});
```

## Failure modes

| Symptom | Likely cause |
| --- | --- |
| `401 unauthorized` | Missing or wrong bearer token. |
| `400 bad request` | Empty `message` field or non-JSON body. |
| `405 method not allowed` | Wrong HTTP verb for that endpoint (every endpoint advertises `Allow:`). |
| `409 conflict` from `POST /messages` | Slash command sent while an agent is running. |
| `503 service unavailable` from `POST /messages` or `POST /interrupt` | Server has been stopped (`/remote stop`). |
| Event stream just hangs | Check that your client disables proxy buffering and follows SSE keep-alives. |

## Lifecycle and security notes

- The remote server is owned by the running TUI process and dies with it.
  There is no daemon mode and no persistent socket file.
- Tokens are 16 random bytes (32 hex chars), regenerated on every
  `/remote` invocation. They are never written to disk.
- Submissions go through the same routing as keyboard input — the active
  permission policy, hooks, approval prompts, and budgets all apply.
  `restricted` and `ask-first` policies still pop their dialogs locally
  inside the TUI; the remote client observes the request via the
  `approval_request` / `ask_user` events but cannot answer them.
- Interrupts coalesce: rapid bursts deliver at most one `remoteInterruptMsg`
  to the program loop until it is consumed.
- Slow SSE subscribers are skipped rather than backpressuring the TUI.
