# Session Lifecycle

Spettro saves your conversation history so you can pause and resume work, clear
context when it gets full, and pick up where you left off — even across TUI
restarts.

This page covers the full lifecycle: auto-save, debounced writes, session
resume, compaction, and the clear/compact distinction.

## Auto-save

Spettro automatically saves the current session to disk as you work:

- **After every completed agent run** (when the assistant message is appended).
- **Debounced during a run** — tool-stream updates and progress comments are
  persisted at most once every 2 seconds to avoid thrashing the disk.
- **On interrupt** — when you press `Esc` mid-run, the kept-progress summary
  is saved immediately.
- **On `/clear`, `/compact`, and session switch** — an unconditional save
  guarantees nothing is lost at these critical points.
- **On exit** — the final turn inside the debounce window is flushed before
  the TUI shuts down.

### Storage path

Sessions live under `~/.spettro/sessions/`. Old sessions can be reclaimed
with [`/storage clean`](storage.md), which never touches the active session
and always keeps the most recent few per project. Each session is identified by a
project-specific hash combined with a timestamp:

```
~/.spettro/sessions/
└── <session-id>/
    ├── metadata.json    — project hash, start time, goal state
    ├── messages.json    — chat messages (user, assistant, system)
    ├── tasks.json       — session task graph (todos.json kept as legacy alias)
    └── events.jsonl     — tool traces, approval decisions, agent spawns
```

### Task graph

Session tasks form a persistent dependency graph, not just a flat list. The
agent manages it with the `todo-write` tool (the retired `task-create`,
`task-update`, `task-get`, `task-list` and `task-delete` names still work as
aliases of it). The tool's description tells the model to use it only for
genuinely multi-step work and never as the only call in a step: a step spent
on the list alone does not advance the task. It is advertised in every host,
headless goal runs included; ACP clients show the list as the session plan.

- `todos` replaces the whole list; with `merge: true` it inserts or updates
  only the given tasks by `id`, and fields left out keep their stored value.
  `delete` removes tasks by id and `clear_completed` prunes completed and
  cancelled ones. A call with none of these only reads the list.
- Every call returns the full list in dependency order, each task with a
  derived `blocked_by` (its incomplete dependencies) and `ready` (pending,
  all dependencies met).
- Sub-agents share their parent's session folder, so below the top level a
  full replace is merged instead: a worker can add and update tasks but
  never wipe the orchestrator's list. In that merge a task written without
  an `id` updates the stored task with exactly the same `content`, so a
  worker that rewrites its whole list does not add copies.

- Each task has an `id`, `content`, `status` (`pending`, `in_progress`,
  `completed`, `blocked`, `cancelled`) and optional `dependencies` (IDs of
  tasks that must be completed first).
- Dependencies are validated on every change: self-references and cycles are
  rejected, dependencies on unknown IDs are dropped with a note in the
  result, and a merged task cannot be moved to `in_progress` or `completed`
  while any dependency is incomplete. Tasks written without an `id` get the
  next free `task-N`.
- The TUI side panel and `/tasks list` render the graph live during runs;
  pending tasks gated by incomplete dependencies show as blocked.
- References to deleted tasks are stripped from other tasks' dependencies so
  the graph stays valid.
- The graph is persisted per session, so a `/resume` restores the plan
  exactly where it was left.

The session directory is created inside the project-local `.spettro/` directory
when one exists, falling back to the global `~/.spettro/sessions/`.

### What is NOT saved

Transient stream blocks (the live "thinking…" and "answering…" messages that
update character-by-character during a run) are stripped before saving. Only
the final, authoritative assistant message is persisted.

## Resume

You can load a previous session with `/resume`:

```text
/resume
```

This opens a picker showing saved sessions for the current project:

```
Choose a session to resume:
  › 2025-01-15 14:30  —  implementing the auth middleware
    2025-01-14 10:15  —  reviewing PR #42
    2025-01-12 16:00  —  setting up CI pipeline
```

- `↑` / `↓` to navigate.
- `Enter` to load the selected session.
- `Esc` to cancel.

When a session is loaded:

1. The chat messages are restored: your prompts, the assistant's answers
   (with their thinking) and system messages. Tool-call rows and plan cards
   are not saved with the messages, so they do not reappear in the
   transcript (step 3 shows what the tools did).
2. The model's context for the first new turn is the saved transcript as
   flattened text, since the structured history of that conversation (tool
   calls and their outputs) is not saved. From that turn on the structured
   history grows again as usual.
3. Session events (tool activity, approval decisions, agent spawns) are
   replayed into the activity feed and side panel.
4. Session tasks (todos) are restored.
5. What belonged to the conversation you left is dropped: the context gauge
   starts from zero again, and a plan waiting for `/approve` is discarded.

If the session had an **unfinished goal** in progress, Spettro remembers its
state (objective, iteration count, no-progress counter, elapsed time) and
offers `/goal resume` after loading.

### Auto-resume on startup

At startup, Spettro does not auto-resume — you always start with a fresh
transcript. Use `/resume` explicitly to return to a previous session.

## Compact (`/compact`)

When the conversation grows long, the context window fills up. Compaction
shrinks the conversation the model sees, freeing token budget for new work:

```text
/compact
```

The TUI, ACP and the run loop (every mode, headless and `/goal` included)
share one compaction core, which always keeps, verbatim:

- **the original task**: the conversation's first message (with its
  environment snapshot), even many turns later;
- **the latest user messages**: up to three of the most recent user
  requests and steering messages from the compacted span, so the current
  request is never summarized away however long the run on it has been;
- **the most recent tool exchanges**: the last three tool calls with their
  results (two on a forced compaction), with call/result pairing checked
  before the history is used, so providers never see an orphaned tool result
  or an unanswered call.

In the TUI the transcript view is replaced by the summary, prefixed with
`── conversation compacted ──`.

You can focus the compaction on a specific topic:

```text
/compact auth middleware
```

This gives the LLM a hint about what to prioritise in the summary.

### Two-stage compaction: prune, then summarize

Stage 1 is cheap and needs no model call; stage 2 is the summarizer.

- **Stage 1: prune old tool outputs.** Every tool result larger than ~500
  tokens is already persisted to the session spool at execution time. Before
  summarizing anything, compaction replaces old results with a short stub
  that keeps the size, the spool ID, the tool name, an args digest, the
  ok/error status, and the first and last line:

  ```text
  [output elided: 48210 chars, spool:7 — re-read with tool-output {"id":"spool:7"}] bash args={"command":"go test ./..."} — 1204 lines, status error, head: "…", tail: "FAIL spettro/internal/agent"
  ```

  Spooled outputs go first, and the most recent ones (about a fifth of the
  window, up to 40k tokens) are left alone. If that is not enough, every
  large output before the verbatim tail is stubbed, with a head/tail excerpt
  for outputs that have no spool copy, and very large strings in old tool-call
  arguments (such as a whole file passed to `file-write`) are elided. The full
  output stays on disk and the model can re-read it at any time with the
  `tool-output` tool (`{"id":"spool:7","offset":0,"limit":4000}`). If pruning
  brings the estimate back under the auto-compact threshold, compaction stops
  here: no summarizer call and no turn dropped.

- **Stage 2: summarize.** If the history is still too large, or on an
  explicit `/compact`, the turns between the task and the verbatim tail are
  replaced by one structured summary with these sections: *Goal*, *Decisions
  and findings*, *Files modified* (each path and what changed), *Current
  state* (test and build status, with exact failing tests and error text),
  *Next steps* and *References* (spool IDs worth re-reading). The summarizer
  sees edits, commands and error output (bounded per item to fit its window,
  keeping the head and tail of long outputs), plus any earlier summary to
  merge. The list of files changed by the edit tools is also derived straight
  from the tool log and attached to the summary, so no edited file can be
  forgotten. If the summarizer fails while the run is recovering from an
  overflowing context, a summary extracted from the transcript (user
  messages, files modified, recent commands and errors) is used instead of
  failing the run (not when the run itself was cancelled: the history is then
  left as it was). A compaction that would not make the history smaller is
  discarded.

After compaction:

- Token usage and context pressure are reset to zero.
- The compacted structured history is carried to the next turn (one cache
  miss on the next request, then the new prefix caches again).
- Session tasks are kept.

### Auto-compact

Auto-compaction runs automatically when the context window exceeds a
configured threshold:

```text
/compact auto on              # enable
/compact auto off             # disable
/compact auto status          # check current setting
```

When enabled, Spettro compacts in two places:

- **Between turns** (TUI and ACP): after an agent turn, if context occupancy
  is above the threshold percentage. This goes cheapest first like the run
  loop: pruning before summarizing, and no summarizer call when the pressure
  comes from the system prompt and tool schemas rather than the history (it
  then waits, silently, for the history to grow before trying again).
- **Inside the run loop** (all modes, including headless and `/goal`): before
  each model step, the runtime estimates context pressure and, past the
  threshold, prunes old tool outputs and, if that is not enough, summarizes
  older turns as described above. A one-line notice
  ("compacted 42k → 6k tokens …") appears in the transcript. This is what
  lets long unattended goal runs survive without anyone watching the gauge.

The threshold percentage is configurable in `~/.spettro/config.json`
(default 85 % of the model's effective window). The `auto_compact_*` settings
below apply to both triggers.

Auto-compact uses a failure budget: if the summarizer fails 3 times in a row
(provider errors), auto-compaction pauses instead of burning a failing call
every step; a successful compaction (e.g. manual `/compact`) resets the
counter. Failures never abort the run — the runtime warns and retries at the
next threshold crossing, and an over-budget request still gets one forced
compaction as a last resort.

### Configuration

| Config key | Default | Description |
|------------|---------|-------------|
| `auto_compact_enabled` | `true` | Enable auto-compaction. |
| `auto_compact_threshold_pct` | `85` | Context window % at which auto-compact triggers. |
| `auto_compact_max_failures` | `3` | Consecutive failures before auto-compact gives up. |

### Policy

```text
/compact policy
```

Shows the current thresholds, failure counter, and warning level:

```
context window:  100000 tokens
threshold:       85000 tokens (85 %)
currently used:  32000 tokens
status:          OK (32%)

auto-compact:    on
failures:        0 / 3
```

The context gauge in the status bar turns yellow at ≥75 % and red at ≥90 %.

### Live updates during a run

Both the context gauge and the session cost counters update **after every
LLM request inside a turn**, not only when the agent finishes. Multi-step
runs (tool loops, goal iterations) therefore show rising occupancy and cost
while the agent is still working, so you can interrupt early if a run is
burning more context or budget than expected.

Two counters are kept deliberately separate:

| Counter | What it measures | Status-bar role |
| --- | --- | --- |
| **Context occupancy** (`contextTokens`) | Largest single LLM request of the current/most recent run — how full the window is | Drives the `N / M ctx` gauge and auto-compact |
| **Session cost** (`totalTokensUsed`) | Sum of every prompt+completion token across the whole session | Goodbye stats, remote status, `/stats` |

A multi-step run that re-embeds the same history on every step does **not**
inflate the gauge: only the largest request counts as occupancy, while each
step still adds its cost. When the run ends, the final totals only add any
remainder that live updates missed (for example a dropped event), so cost is
never double-counted.

`/stats` still shows the full provider-reported breakdown (input, output,
cache read/write, per-model) once you want the detailed accounting.

In [ACP mode](acp.md) the same live path emits a `usage_update` session
notification after every request, and the completed turn's aggregated usage
is returned on the `session/prompt` response.

## Clear (`/clear`)

```text
/clear
```

- **Saves** the current conversation to disk (exactly as `/resume` would find
  it).
- **Clears** the chat transcript, the structured conversation history, the
  token counters and context gauge, and a plan waiting for `/approve`.
- Starts a fresh session.

Use `/clear` when you want to start a new topic without losing the previous
one. The saved session is available via `/resume` later.

## Full lifecycle example

```
1. Start Spettro         → fresh session
2. Work for a while      → auto-save runs in background (debounced)
3. Context is getting
   tight (yellow gauge)  → auto-compact when crossing 85%
4. Continue working      → auto-save continues
5. Switch topics         → /clear  (saves + starts fresh)
6. Next day              → /resume, pick yesterday's session
7. Work more             → /compact manually to keep context lean
8. Quit                  → flushSave writes the last turn
```

## Retention

Sessions are never automatically deleted. They accumulate under
`~/.spettro/sessions/`. You can remove old sessions manually:

```bash
rm -rf ~/.spettro/sessions/<session-id>
```

There is no built-in session manager or retention policy yet.

## Background jobs

Spettro tracks detached shell processes started by the agent with `run_in_background`
(e.g., dev servers, watch builds, long-running scripts). Jobs are **process-wide**
session state: they outlive individual agent turns and are killed when the session
ends.

### Listing jobs

```text
/jobs
```

or

```text
/jobs list
```

Prints every tracked job with its ID, status, command, and elapsed time:

```
background jobs:
- job-1 [running] npx vite --port 5173 (started 5m23s ago)
- job-2 [exited] go run ./cmd/server (started 2m10s ago)

kill with /jobs kill <id> or /jobs kill all
```

### Killing a job

```text
/jobs kill job-1
```

Kills the job's entire process group. Accepts any job ID shown in the listing.

```text
/jobs kill all
```

Terminates every running job at once.

### Lifecycle

- Jobs are created when the agent calls `bash` with
  `run_in_background: true`.
- Output is captured in a per-job ring buffer (up to 1 MiB of combined
  stdout/stderr, oldest bytes dropped when exceeded).
- When the session ends (TUI exit, `/exit`), all remaining jobs are killed
  automatically.
- When the terminal or tmux pane spettro runs in is closed (SIGHUP), spettro
  kills every foreground shell command, background job and PTY session before
  it exits. From that moment it also refuses to start new foreground shell
  commands (they fail with "spettro is shutting down; command not started"), so
  an agent reacting to its killed command cannot leave a new one running.
  Under `nohup` the hangup
  is ignored and everything keeps running.
- Jobs survive `/clear` (which only resets the conversation). Use `/jobs kill all`
  to clean up explicitly.

### Tool output spooling

Oversized tool results (from `file-read`, `grep`, `glob`, `bash`, `web-fetch`)
are automatically spooled to disk instead of being
hard-truncated. The model receives a truncated head with a footer containing a
`spool:N` ID and an offset, and can page through the full result with the
`tool-output` tool (`{"id":"spool:N","offset":Z,"limit":M}`), which every agent
holding `file-read` has.

In addition, *every* tool result over ~500 tokens — even ones small enough to
stay in context untruncated — is written to the spool at execution time. This
backs reference-based compaction (see [Compact](#compact-compact)): when the
context fills up, old oversized results are swapped for `[output elided: …]` stubs
pointing at their spool IDs rather than being lost to summarization.

Spool files are tied to the conversation, not to a single run: they survive
run end, and are deleted on `/clear` and when the process exits (TUI exit,
`/exit`).

```text
# example: model receives truncated grep output with a footer
[truncated: 12,400 of 13,000 lines omitted; use tool-output {"id":"spool:2","offset":1800} to read more]

# model pages through the omitted portion
~> tool-output {"id":"spool:2","offset":1800}
<~ output=spool:2 size=280000 next_offset=9800 (more available)
# the next chunk of content...
```

`bash` (and its retired `bash-output` alias) also accepts `job_id` and
`offset` in place of `command`, and then reads a background job's output like
`job-output`.