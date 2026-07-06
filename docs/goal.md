# Goal Mode (`/goal`)

Spettro can operate autonomously — you set an objective and the agent runs
continuously until it is done, stalled, or hits a safety limit. This is called
**goal mode**.

Goal mode works in both the TUI (via `/goal`) and the headless server (via the
`--goal` CLI flag). It is designed for multi-step tasks that need more than one
agent run: updating dependencies, migrating a codebase, implementing a feature
that spans multiple files and verification cycles.

## Quickstart

Inside the TUI:

```text
/goal update all npm dependencies to their latest major version
```

The agent begins working autonomously. It runs **iterations** — full LLM
tool-loop turns — until one of these happens:

- The objective is met and the agent calls `goal-complete` ✅
- No progress is detected for several iterations in a row ⏹
- The iteration safety cap is reached ⏹
- You press `Esc` or send `/goal stop` ✋

## How it works

### The goal loop

1. You run `/goal <objective>`.
2. Spettro dispatches the objective as a task to the **coding** orchestrator
   with the `GoalModePreamble` prepended: instructions to work autonomously,
   not ask for continuation, verify work, and call `goal-complete` when done.
3. That run completes. If the agent called `goal-complete`, the goal ends.
4. If not, Spettro checks for progress (see below) and either:
   - dispatches another iteration (the agent resumes where it left off), or
   - stalls the goal and reports why.
5. The loop repeats until one of the termination conditions fires.

### Progress detection

Between iterations, Spettro fingerprints the workspace by hashing
`git status --porcelain` output. If the signature does not change across
consecutive iterations, the goal is considered **stalled**. The stall limit is
configurable (default: 3 iterations with no change).

This is conservative: false "progress" (a change that isn't actually progress)
just lets the loop continue; false "stall" is the bad case, so the detection
prefers to under-report stalls.

### Conversation state across iterations

The LLM conversation from each iteration is fed back as the structured
`Messages` prefix for the next one. This keeps the provider request prefix
byte-stable so prompt caching hits and previously generated tokens are never
re-summarised or discarded between turns.

When the context window gets tight, Spettro **auto-compacts** between
iterations (respecting your auto-compact threshold). The compaction preserves
a summary that the next iteration extends.

## Commands

| Command | Description |
|---------|-------------|
| `/goal <objective>` | Start a new goal run with the given objective. |
| `/goal stop` | Abandon the active goal and cancel any in-flight run. |
| `/goal status` | Show the current goal's iteration count, no-progress counter, and elapsed time. |
| `/goal resume` | Resume an unfinished goal from a loaded session (shown via `/resume`). |

Only available in the TUI. Headless goal mode is started via the `--goal` CLI
flag (see below).

## Configuration

These settings live in `~/.spettro/config.json`:

| Setting | Config key | Default | Description |
|---------|-----------|---------|-------------|
| Shell timeout | `goal_shell_timeout_sec` | 600 (10 min) | Max wall-clock time per shell/bash tool call during goal runs. Longer than the default timeout to accommodate installs and builds. |
| Max iterations | `goal_max_iterations` | 0 (unlimited) | Safety cap on the total number of outer-loop iterations. Set to a positive integer to prevent runaway loops. |
| Stall limit | `goal_no_progress_limit` | 3 | Consecutive iterations with no workspace change before the goal is declared stalled. |

Set them with:

```text
/budget 0
/compact auto on
```

(The token budget and auto-compact settings apply to goal runs the same way
they apply to ordinary runs — auto-compact is especially useful for long goal
sessions.)

## Permission mode and goal mode

Goal mode pauses on approval prompts the same way ordinary runs do. For
**fully unattended** operation, set permission to `yolo` before starting:

```text
/permission yolo
/goal deploy the application to staging
```

When permission is `ask-first` or `restricted`, Spettro prints a warning
before starting the goal loop so you know what to expect.

## Headless goal mode

Outside the TUI, start a goal run with:

```bash
spettro --goal "update all dependencies" --sandbox workspace-write
```

Flags available:

| Flag | Description |
|------|-------------|
| `--goal <objective>` | Start goal mode with the given objective. |
| `--cwd <path>` | Working directory (default: current directory). |
| `--sandbox <mode>` | OS sandbox mode: `off`, `read-only`, `workspace-write`. |
| `--sandbox-net <policy>` | Network policy: `all`, `localhost`, `none`, or `ports:443,8080`. |
| `--sandbox-allow-dir <path>` | Extra writable directory inside the sandbox (repeatable). |
| `--sandbox-allow-read-dir <path>` | Extra readable directory inside the sandbox (repeatable). |

In headless mode:

- Permission is **forced to `yolo`** (unattended operation).
- Tool traces are printed to stdout with `[✓]` / `[✗]` markers.
- The `ask-user` tool is unavailable — if the agent tries to ask a question,
  the goal fails with an error.
- Exit codes: `0` for goal complete, `1` for stall/error/interrupt.

Output:

```
Starting goal mode: update all npm dependencies
Max iterations: 0 (unlimited), No-progress limit: 3

=== Iteration 1 ===
  [✓] glob: success
  [✓] file-read: success
  ...

=== Iteration 2 ===
  ...

✓ Goal complete: all dependencies updated and verified
Iterations: 3, Duration: 2m15s
```

## ACP mode

Over the Agent Client Protocol (ACP), `/goal <objective>` is advertised as an
available command and runs the autonomous loop inside the prompt turn. Cancel
the turn (`session/cancel`) to stop the goal.

## Caveats

- **Long-running builds**: set `goal_shell_timeout_sec` high enough (default
  10 minutes is usually sufficient). The headless mode applies the timeout
  from config; the TUI applies it per shell/bash tool call.
- **Expensive models**: goal mode can accumulate many iterations (and many
  LLM calls per iteration). Keep an eye on your token usage. The `tokens_used`
  line in the status bar tracks cumulative session cost.
- **No undo**: goal mode makes real changes to your workspace. Use the sandbox
  (`--sandbox workspace-write` or `read-only`) as a safety net, and `git` to
  review what changed before committing.
- **Interrupting**: `Esc` in the TUI or `POST /interrupt` over the remote
  control plane cancels the in-flight run **and** abandons the goal entirely.