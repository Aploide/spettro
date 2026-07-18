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

## When to use it

Ultra shines on wide, parallelizable work: sweeping refactors, adding
tests or docs across many files, mass migrations, or repo-wide audits.
For trivial single-step tasks the agent is told to just do them
directly, and for a single delegation the regular `agent` tool remains
the right choice.

Note that a swarm multiplies token usage — every sub-agent is a full
agent run on the active model.
