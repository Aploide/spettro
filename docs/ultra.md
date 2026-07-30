# Ultra mode (agent swarm)

Ultra is a toggle that turns the top-level agent into a swarm
orchestrator: instead of doing hard or decomposable work itself, it fans
the work out across **many parallel sub-agents** with the `ultra` tool.
It works with **any model** — sub-agents inherit the active session's
provider, model, thinking level, and permissions — and is available in
the TUI, in ACP editors (Zed, …), and in headless/goal runs.

## Enabling it

| Surface | How |
| --- | --- |
| TUI | `/ultra` (or `/ultra on` / `/ultra off`) — instant, persisted, shown as a bold `ultra` tag in the status bar |
| ACP editors | the **Ultra** toggle in the session config toolbar |
| Config file | `"ultra": true` in `~/.spettro/config.json` |

The setting is persistent and applies to the **next** run/turn (the
system prompt is fixed per run to keep prompt caching intact).

**Permission requirement:** Ultra needs the `restricted` or `yolo`
permission level. A swarm executes many sub-agents concurrently, and
`ask-first` would flood you with per-action approval prompts, so:

- turning Ultra on under `ask-first` is refused — switch first with
  `/permission restricted` or `/permission yolo`;
- if you later drop back to `ask-first`, Ultra is *suspended* (the
  toggle stays saved but the tool is not injected) until you raise the
  level again.

## How it works

When Ultra is on, the top-level agent:

1. gains the `ultra` tool (regardless of its manifest role — Ultra
   bypasses the usual `PrimaryOnly`/handoff gating by design);
2. receives extra system-prompt guidance: explore lightly, then
   decompose the main work as finely as independence allows and hand it
   to the swarm — one item per file/package/test suite, each with a
   distinct, non-overlapping scope.

The tool call looks like:

```json
{
  "description": "Add doc comments to every exported symbol",
  "prompt_template": "Add doc comments to all exported symbols in {{item}}, then run go vet on the package.",
  "items": ["internal/agent/ultra.go", "internal/acp/bridge.go", "internal/tui/model.go"],
  "subagent_type": "code"
}
```

- `prompt_template` must contain the `{{item}}` placeholder; each item
  fills it into one self-contained sub-agent task (sub-agents cannot see
  the parent's context or each other).
- Between **2 and 32** items per call; every filled prompt must be
  distinct.
- `subagent_type` picks the worker from the [agent manifest](../AGENTS.md)
  (default `code`); orchestrator agents are rejected.

Execution details:

- **Launch ramp** — up to 5 sub-agents start immediately, then one more
  every 700 ms, to avoid hammering the provider.
- **Concurrency cap** — set the `SPETTRO_ULTRA_MAX_CONCURRENCY`
  environment variable to hard-cap simultaneous sub-agents (default:
  uncapped beyond the ramp).
- **Retries** — transient provider failures (rate limits, availability)
  are retried per sub-agent with exponential backoff (3 s, 6 s, 12 s).
- **Results** — returned to the main agent **in input order** as an
  `<ultra_result>` block with a `completed/failed` summary; each
  sub-agent's final message is its entire handoff. The main agent is
  instructed to review the results, re-dispatch failures, and verify the
  integrated outcome.
- Sub-agents never get the `ultra` tool themselves (no recursive
  swarms), and the normal delegation depth limits still apply.

## Workspace isolation (worktrees)

A swarm that *edits* files can crowd the shared checkout: many agents
writing into one working tree, one `git status` full of everyone's
changes. Setting `"isolation": "worktree"` on the `ultra` call (or on a
single `agent` delegation) gives every sub-agent its own workspace
instead:

1. For each member, Spettro creates a git **worktree** under
   `.spettro/worktrees/<agent>-<id>/` in the project root, on a fresh
   **branch named after the sub-agent** (`spettro/code-3-a1b2c3`),
   forked from the current `HEAD`.
2. Each sub-agent runs with its cwd inside its own worktree, so
   concurrent edits never collide and the main checkout stays clean
   (`.spettro/` is auto-added to `.git/info/exclude`).
3. When the swarm finishes, the branches are **merged back one at a
   time, in item order**, into the main checkout; leftover uncommitted
   work is committed first, with a Conventional Commits message written
   by the LLM from the diff (same machinery as auto-commit; a stock
   `spettro: subagent … work` message is the fallback if that fails).
   After a successful merge the branch and its worktree are **deleted**.

Outcomes per member (visible as `merge="…"` in the `<ultra_result>`
block and in the `agent` tool's JSON result):

| Status | Meaning |
| --- | --- |
| `merged` | branch merged into the main checkout, then deleted |
| `no_changes` | the agent changed nothing; worktree and branch deleted |
| `conflict` | the merge conflicted: it was aborted and the **branch and worktree are kept** for manual resolution |
| `preserved` | the agent failed but left work behind; branch and worktree are kept so nothing is lost |
| `error` | a git step failed; details in the result |

Kept branches are reported with their paths; resolve them by merging
manually, then `git worktree remove <path>` and `git branch -D
<branch>` (leftovers also show up in `/storage`). Worktree isolation
requires the project to be a git repository with at least one commit.
Leave `isolation` unset for read-only fan-outs (research, review,
search) — worktrees would only add overhead there.

## Observability: watching the swarm

Every swarm member gets a **distinct instance name** — `code#1`,
`code#2`, … in item order — and every tool trace it emits carries that
name, so its activity is attributable end to end:

- **TUI transcript** — running members appear under the `agents` block
  with their instance name and assigned item.
- **`ctrl+b` activity panel** — a dedicated **swarm** section lists every
  member with a live status (`▶` running, `✓` done, `✗` failed), a
  `N running · N done · N failed` header, and what each agent is doing
  right now (its latest tool call, falling back to its assigned item).
  Completed and failed members stay listed until the next turn, so you
  always see the whole fan-out. The activity list below groups tool
  calls per instance name. A banner reminds you of `ctrl+b` when a
  swarm starts with the panel hidden.
- **ACP editors** — each member's lifecycle arrives as an
  `agent code#3: <item>` tool call, and its individual tool calls are
  prefixed `[code#3] …`, so editors show which agent is doing what.
- **Results** — the `<ultra_result>` block names each sub-agent
  (`name="code#3"`), so the orchestrator can refer to and re-dispatch a
  specific member.

## When to use it

Ultra shines on wide, parallelizable work: sweeping refactors, adding
tests or docs across many files, mass migrations, or repo-wide audits.
For trivial single-step tasks the agent is told to just do them
directly, and for a single delegation the regular `agent` tool remains
the right choice.

Note that a swarm multiplies token usage — every sub-agent is a full
agent run on the active model.
