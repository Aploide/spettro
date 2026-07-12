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

Sessions live under `~/.spettro/sessions/`. Each session is identified by a
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
agent manages it with the `task-create`, `task-update`, `task-get`,
`task-list` and `task-delete` tools (the flat `todo-write` tool remains as an
alias writing to the same store):

- Each task has an `id`, `content`, `status` (`pending`, `in_progress`,
  `completed`, `blocked`, `cancelled`) and optional `dependencies` (IDs of
  tasks that must be completed first).
- Dependencies are validated on every change: unknown IDs, self-references and
  cycles are rejected, and a task cannot be moved to `in_progress` or
  `completed` while any dependency is incomplete.
- `task-list` returns tasks in dependency order with a derived `blocked_by`
  field, and supports the pseudo-filters `ready` (pending, all dependencies
  met) and `blocked`.
- The TUI side panel and `/tasks list` render the graph live during runs;
  pending tasks gated by incomplete dependencies show as blocked.
- `task-delete` removes a task by id (or prunes all completed/cancelled
  tasks with `clear_completed`); references to deleted tasks are stripped
  from other tasks' dependencies so the graph stays valid.
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

1. The chat transcript is restored exactly as it appeared (user messages,
   assistant responses, system messages, tool traces, plan cards).
2. The structured conversation history (`convHistory`) is rebuilt, so the LLM
   has full context of what was said and done before.
3. Session events (tool activity, approval decisions, agent spawns) are
   replayed into the activity feed and side panel.
4. Session tasks (todos) are restored.

If the session had an **unfinished goal** in progress, Spettro remembers its
state (objective, iteration count, no-progress counter, elapsed time) and
offers `/goal resume` after loading.

### Auto-resume on startup

At startup, Spettro does not auto-resume — you always start with a fresh
transcript. Use `/resume` explicitly to return to a previous session.

## Compact (`/compact`)

When the conversation grows long, the context window fills up. Compaction
replaces the entire transcript with a summary, freeing token budget for new
work:

```text
/compact
```

The LLM reads the full conversation and produces a condensed summary. The
summary is injected as a system message prefixed with `── conversation
compacted ──`, and the old messages are discarded.

You can focus the compaction on a specific topic:

```text
/compact auth middleware
```

This gives the LLM a hint about what to prioritise in the summary.

After compaction:

- Token usage and context pressure are reset to zero.
- The structured conversation history is rebuilt from the summary (one cache
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

When enabled, Spettro runs a compaction after every agent turn if the context
occupancy is above the threshold percentage (configurable in
`~/.spettro/config.json`, default 85 %).

Auto-compact uses a failure budget: if compaction fails 3 times in a row, it
stops retrying until a manual `/compact` succeeds.

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

## Clear (`/clear`)

```text
/clear
```

- **Saves** the current conversation to disk (exactly as `/resume` would find
  it).
- **Clears** the chat transcript, the structured conversation history, and the
  token counters.
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