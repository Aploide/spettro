# Workflows (deterministic multi-agent orchestration)

A **workflow** is a small JavaScript program that decides — in ordinary
control flow, not by asking a model at every step — which sub-agents
run, in what order, and how their results combine. The model writes the
script; Spettro executes it exactly as written.

[Ultra](ultra.md) fans one prompt template over a list of items: one
shape, one round. That covers a lot of work, but not "verify each
finding as it lands", "generate three designs and score them against
each other", or "keep sweeping until two rounds turn up nothing new".
Those need a loop, a condition, or a second stage, and without a script
the model has to re-derive them on every turn — which is where
orchestration drifts.

Workflows work with **any model** — sub-agents inherit the session's
provider, model, thinking level and permissions — and are available in
the TUI, in ACP editors (Zed, …), and in headless/goal runs.

## Turning it on

Ask for one however you like:

```text
use a workflow to modernise these handlers
fan this out across sub-agents and verify each finding
orchestrate the migration with subagents
```

or write the shorthand, **`ultracode`**, anywhere in a message:

```text
ultracode: review this branch across correctness, perf and tests, then
try to refute every finding before you report it
```

Either grants the agent the `workflow` tool for **that turn only**, and
appends the authoring guidance to its system prompt. Ordinary uses of the
word stay quiet — "our deploy workflow is broken" and
`.github/workflows/ci.yml` do not activate anything, and `ultracoded` is
not the keyword.

The difference between the two forms is **consent**, not capability:

| You wrote | What happens |
| --- | --- |
| `ultracode` | A standing yes. The agent writes the script and runs it. |
| "use a workflow …", or the agent proposing one itself | You are asked first: **Run it** / **Save it, don't run** / **Don't run it**. |

Declining is final — the agent is told not to run it anyway and not to
re-propose it. "Save it, don't run" writes the script to
`.spettro/workflows/<name>.js` so you can run it later with
`/workflows run <name>`.

Where nobody can be asked — headless mode, a `/goal` loop, the Telegram
relay — the request that turned workflows on is taken as the answer.
Hanging on a prompt no one will see is not better.

As you type it, the word lights up in the input box — a slow violet-to-cyan
shimmer with a highlight sweeping across it. That is not decoration: the
highlight is driven by the *same* match that decides whether the tool gets
injected, so a lit word is a word that will activate, and `ultracoded` stays
plain.

It is one-shot rather than a persistent toggle on purpose: the guidance
is a couple of kilobytes of system prompt, and the prompt has to stay
byte-stable within a run for caching to hit, so a permanent toggle would
charge every turn for a capability most turns do not need.

Detection lives in the agent runner, keyed off the text of your message,
so the keyword works identically in the TUI, in ACP editors, in
`/goal` runs, over the Telegram relay, and in headless mode. No setting
to find, no per-surface toggle.

**Permission requirement:** like Ultra, workflows need `restricted` or
`yolo`. A script runs many sub-agents concurrently, and `ask-first`
would turn that into a wall of approval prompts.

## Writing a workflow

Every script begins with a header and then uses the globals:

```javascript
export const meta = {
  name: 'review-changes',
  description: 'Review the diff across dimensions, then refute each finding',
  phases: [
    { title: 'Review', detail: 'one agent per dimension' },
    { title: 'Verify', detail: 'refute each finding independently' },
  ],
}

const FINDINGS = {
  type: 'object',
  properties: { findings: { type: 'array', items: { type: 'object' } } },
  required: ['findings'],
}
const VERDICT = {
  type: 'object',
  properties: { real: { type: 'boolean' }, why: { type: 'string' } },
  required: ['real'],
}

const DIMENSIONS = ['correctness bugs', 'performance', 'missing tests']

const results = await pipeline(
  DIMENSIONS,
  d => agent(`Review the diff in this repo for ${d}. Report each finding with a title and a file:line.`,
             { label: `review:${d}`, phase: 'Review', schema: FINDINGS }),
  review => parallel((review?.findings ?? []).map(f => () =>
    agent(`Try to REFUTE this claim about the repo: ${JSON.stringify(f)}. Default to real=false if uncertain.`,
          { label: `verify:${f.title}`, phase: 'Verify', schema: VERDICT })
      .then(v => ({ ...f, verdict: v })))),
)

const confirmed = results.flat().filter(Boolean).filter(f => f.verdict?.real)
log(`${confirmed.length} findings survived verification`)
return { confirmed }
```

The body runs inside an async function, so `await` and a top-level
`return` work exactly as written, on the line numbers you typed.

### The header

`export const meta` must be a **pure object literal** — no variables,
calls, spreads or template interpolation. Spettro parses it before
anything runs, in a JS runtime stripped of every global, so a header
that tried to compute something fails there instead of quietly doing
work before you have seen what the workflow is.

| Field | Required | Meaning |
| --- | --- | --- |
| `name` | yes | short identifier, also the run's display name |
| `description` | yes | one line: what this workflow does |
| `whenToUse` | no | shown in `/workflows` listings |
| `phases` | no | `[{title, detail}]` — declared up front so the panel can draw the whole plan before the first agent starts |
| `model` | no | note for readers that the script pins a model |

### Globals

| Global | Behaviour |
| --- | --- |
| `agent(prompt, opts)` | Run one sub-agent. Resolves to its final text, or to the parsed object when `opts.schema` is set, or to **`null`** if it failed. |
| `parallel(thunks)` | Run every thunk concurrently and wait for all of them — a **barrier**. A thunk that throws resolves to `null` instead of rejecting the call. |
| `pipeline(items, ...stages)` | Push each item through every stage independently, with **no barrier** between stages. Each stage gets `(prevResult, originalItem, index)`; a stage that throws drops that item to `null`. |
| `phase(title)` | Start a progress group. Later `agent()` calls join it unless they set `opts.phase`. |
| `log(message)` | Emit a progress line to the user and to the run's result. |
| `args` | Whatever the tool call passed in (`undefined` if nothing). |
| `budget` | `{total, spent(), remaining()}` — see [Token budgets](#token-budgets). |
| `workflow(name \| {scriptPath}, args)` | Run a saved workflow as a sub-step. One level only. |

`agent()` options:

| Option | Meaning |
| --- | --- |
| `label` | Display label; defaults to the prompt's first line. |
| `phase` | Assign this call to a phase explicitly (use inside `pipeline`/`parallel` stages, where the global phase may have moved on). |
| `schema` | A JSON Schema. Spettro appends the contract to the prompt, parses the answer back, and retries with the parse error if it does not fit. |
| `agentType` | Manifest agent to run as (default `general-purpose`). Orchestrators are rejected. |
| `model` | Pin this call to a different model than the session's. |
| `effort` | `off`/`low`/`medium`/`high`/`x-high`/`max` thinking level for this call. |
| `isolation` | `"worktree"` gives the sub-agent its own git worktree and branch, merged back when it finishes (see [Ultra's worktree section](ultra.md#workspace-isolation-worktrees)). |

### `pipeline` vs `parallel`

Default to `pipeline`. A barrier is only right when a stage genuinely
needs *every* previous result at once — deduplicating across the whole
set, or bailing out when the total count is zero. It is not justified by
"I need to flatten first" (do that inside a stage) or "the stages feel
separate" (that is what stages are).

Barrier latency is real: if five finders run and the slowest takes three
times the fastest, `parallel`-then-`parallel` idles the four fast ones
for two thirds of the round. `pipeline` gets the same work done in the
time of the slowest single chain.

### Failure semantics

A failed agent resolves to `null` rather than rejecting. A fan-out is
worth running precisely when individual members may die, and a rejection
would take the whole script down with the first one. Failures are
counted, shown in the panel, and reported in the tool result, so the
orchestrating agent knows to re-dispatch — but `.filter(Boolean)` is
still the habit to keep.

Transient provider failures (rate limits, availability) are retried per
agent with the same backoff Ultra uses: 3 s, 6 s, 12 s.

### What is missing on purpose

`Date.now()`, `Math.random()` and argless `new Date()` all throw. A
workflow has to replay identically when [resumed](#resuming-a-run), and
a script that stamps wall-clock time or randomises a prompt cannot.
Pass timestamps in through `args`, and vary work by item index.

`new Date(0)` and friends still work — only the clock is gone.

## Limits

| Limit | Value | Why |
| --- | --- | --- |
| Concurrent agents | `min(16, CPUs - 2)`, or `max_concurrency` | Excess calls queue; a 100-item `parallel` still completes, just not all at once. |
| Agents per run | 1000 | Runaway-loop backstop, set far above any real workflow. |
| Items per `parallel`/`pipeline` call | 4096 | An explicit error, never a silent truncation. |
| Tool timeout | 2 hours | A workflow is many full agent turns. |

## Token budgets

Pass `budget_tokens` on the tool call to give the script a target:

```javascript
const bugs = []
while (budget.total && budget.remaining() > 50_000) {
  const found = await agent('Find a bug in this repo nobody has reported yet.', {schema: BUGS})
  bugs.push(...(found?.bugs ?? []))
  log(`${bugs.length} found, ${Math.round(budget.remaining() / 1000)}k left`)
}
```

The target is a **hard ceiling**: once `spent()` reaches `total`, further
`agent()` calls throw. Guard the loop on `budget.total` — with no target
set, `remaining()` is `Infinity` and the loop would run to the 1000-agent
cap.

## Watching a run

**TUI.** Under the transcript you get a summary: the phase the run is in,
overall progress, and what is running *right now* — finished agents are
history, and history is what the side panel is for. It scales with the
terminal and never takes more than a few rows, because a running workflow
must not be the reason you cannot read what the agent just said. Once the
run ends it collapses to a single line and gives the rows back.

It shares one budget — about a quarter of the terminal height — with an
ultra swarm, ordinary delegations and the todo list, so the four of them
together stay bounded no matter how much is in flight. Each block says
how many entries it left out; nothing is hidden silently.

`ctrl+b` has the whole thing: every declared phase drawn from the start
(dimmed until reached), filling in as agents land under it, with
per-phase meters, each agent's live tool call, `log()` lines, and
replayed-from-journal markers. The tree survives the turn that produced
it and is cleared when the next one starts.

**ACP editors.** The run opens a single `workflow <name>` tool call
whose content is rewritten as it progresses, so the editor shows the
same phase tree growing in place. Each sub-agent additionally gets its
own tool call, so "follow the agent" navigation keeps working.

## Saved workflows

The agent writes these too. If no saved workflow fits what you asked for
— the normal case — it authors one from scratch; if the script is worth
keeping it sets `save_as` and the script lands in the folder below,
runnable by name from then on.

Two ready-to-run scripts ship in
[`docs/examples/workflows/`](examples/workflows/): `review-branch.js`
(review, then adversarially refute each finding) and
`explain-subsystem.js` (multi-angle sweep, synthesis, critique). Copy
either into one of the folders below to make it available by name.

Scripts in either of these folders are reusable by name:

- `.spettro/workflows/<name>.js` — project
- `~/.spettro/workflows/<name>.js` — global

A project script shadows a global one with the same name.

| Command | Description |
| --- | --- |
| `/workflows` | List saved workflows with their descriptions and phases. |
| `/workflows show <name>` | Print a script's header and source. |
| `/workflows run <name> [json]` | Run one, optionally with JSON `args`. |
| `/workflows where` | Show the directories being scanned. |

`/workflows run` does not execute the script behind the agent's back: it
dispatches a turn instructing the agent to invoke that saved workflow.
Results are only useful to someone who then acts on them, and that keeps
one execution path for scripts the model wrote and scripts you handed it.

The same commands are available over ACP.

A script can call another with `workflow('name', args)`. The child shares
the parent's concurrency pool, agent counter and token budget. Nesting is
one level deep — a `workflow()` call inside a child throws.

## Run artifacts and resuming

Every run writes to `<session>/workflows/<run_id>/`:

| File | Contents |
| --- | --- |
| `script.js` | the exact source that ran |
| `meta.json` | the parsed header |
| `journal.jsonl` | one record per agent call: prompt hash, label, phase, output |
| `result.json` | the script's return value |

### Resuming a run

Re-run with `script_path` and `resume_from_run_id` and every agent call
whose prompt and options are unchanged replays from the journal instead
of executing. A relative `script_path` is relative to the agent's
workspace; the script must be in the workspace, in a run directory of this
or another session, or in a saved-workflow folder. Edit one stage of a twelve-agent script and only that
stage — and whatever depends on it — costs anything.

Entries are keyed by a hash of `(prompt, agentType, model, effort,
isolation, schema)`, not by call order: `parallel` and `pipeline`
interleave, so ordinal position is not reproducible while the identity of
a given call is. Identical calls replay first-come-first-served, so a
fan-out of N identical prompts resumes correctly too.

Failed calls are never cached — the whole point of resuming is to retry
them.

## Patterns

These are shapes worth reaching for, not a taxonomy. Compose freely.

**Adversarial verify.** Spawn several independent skeptics per finding,
each prompted to *refute* it; keep it only if a majority fail. Stops
plausible-but-wrong findings from surviving.

**Perspective-diverse verify.** When a claim can be wrong in more than
one way, give each verifier a distinct lens (correctness, security,
performance, does-it-reproduce) instead of N identical refuters.

**Judge panel.** Generate N independent attempts from different angles,
score them with parallel judges, synthesise from the winner while
grafting the best ideas from the runners-up. Beats one-attempt-iterated
when the solution space is wide.

**Loop until dry.** For unknown-size discovery, keep spawning finders
until K consecutive rounds turn up nothing new. Deduplicate against
everything *seen*, not against what was *confirmed* — otherwise
judge-rejected findings reappear every round and the loop never
converges.

**Multi-modal sweep.** Parallel agents each searching a different way
(by container, by content, by entity, by time), each blind to what the
others surface.

**Completeness critic.** A final agent asking "what is missing — a
modality not run, a claim unverified, a source unread?" What it finds
becomes the next round.

## When *not* to use one

- A single delegation → the `agent` tool.
- One template over N items, one round → [Ultra](ultra.md).
- A trivial single-step task → just do it.

A workflow multiplies token usage the same way a swarm does: every
`agent()` call is a full agent run on the active model.
