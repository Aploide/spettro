# Workflows (deterministic multi-agent orchestration)

A **workflow** is a small JavaScript program that decides — in ordinary
control flow, not by asking a model at every step — which sub-agents
run, in what order, and how their results combine. The model writes the
script for the task in front of it; Spettro executes it exactly as
written.

As written is not the same as fixed in advance. A script can discover its
own work-list as it runs, add phases it did not declare, loop until a
search runs dry, and stop at a [checkpoint](#orchestrator-in-the-loop)
to hand the orchestrating model a decision. The model reads the interim
result there and replies before the run goes on. See
[Dynamic workflows](#dynamic-workflows).

Fanning one prompt over a list of items is one shape, one round: in a
script it is a single `parallel()` call. Plenty of work needs more than
that — "verify each finding as it lands", "generate three designs and
score them against each other", "keep sweeping until two rounds turn up
nothing new". Those need a loop, a condition, or a second stage, and
without a script the model has to re-derive them on every turn — which
is where orchestration drifts.

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
appends the authoring guidance to its system prompt. The one exception is
a run [paused at a checkpoint](#orchestrator-in-the-loop): while the
session holds one, every turn gets the tool, so the run can be continued
without repeating the keyword. Ordinary uses of the
word stay quiet — "our deploy workflow is broken" and
`.github/workflows/ci.yml` do not activate anything, and `ultracoded` is
not the keyword.

The difference between the two forms is **consent**, not capability:

| You wrote | What happens |
| --- | --- |
| `ultracode` (or `/ultra` is on) | A standing yes. The agent writes the script and runs it, and treats a workflow as the default way to do the task (see [/ultra and ultracode](#ultra-and-ultracode)). |
| "use a workflow …", or the agent proposing one itself | The agent judges whether a workflow fits. If it writes one, you are asked first: **Run it** / **Save it, don't run** / **Don't run it**. |

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

The keyword is one-shot on purpose: the guidance is a couple of kilobytes
of system prompt, and the prompt has to stay byte-stable within a run for
caching to hit, so a standing toggle charges every turn for it. When that
is what you want, `/ultra on` makes it standing (see
[/ultra and ultracode](#ultra-and-ultracode)).

Detection lives in the agent runner, keyed off the text of your message,
so the keyword works identically in the TUI, in ACP editors, in
`/goal` runs, over the Telegram relay, and in headless mode. The only
setting is `/ultra`, which has the same effect as writing the keyword
every time, as long as the run's permission allows workflows (see
below).

**Permission requirement:** workflows need `restricted` or
`yolo`. A script runs many sub-agents concurrently, and `ask-first`
would turn that into a wall of approval prompts.

## /ultra and ultracode

`ultracode` is more than a shortcut for "use a workflow". A plain-English
request offers the tool and leaves it to the model to judge whether a
script beats doing the work inline. `ultracode` is an opt-in that stands:
every **substantive** task gets a workflow by default, and the aim is the
most exhaustive, correct answer rather than the cheapest one. In
practice the agent:

- **scouts inline first.** It reads enough of the code to know what the
  work-list is before it writes a script.
- **runs one workflow per phase** of multi-phase work: understand →
  design → implement → review. It reads each result before it decides
  what the next workflow should do, instead of writing one script that
  guesses at all four.
- **leans toward adversarial verification:** independent skeptics,
  verifiers with different lenses, loop-until-dry sweeps and a
  completeness critic.
- **uses [checkpoints](#orchestrator-in-the-loop)** when the next stage
  depends on its own judgement of the interim results.
- **works solo only** on conversational turns ("what does this flag do?")
  and trivial mechanical edits.

Token cost is not the constraint under ultracode, but the
[size guideline](#sizing) still applies. It is part of the same prompt
section.

The keyword covers one message. `/ultra` keeps ultracode on for every
turn:

| Surface | How |
| --- | --- |
| TUI | `/ultra [on\|off]` (no argument flips it). The status bar shows `ultra` while it is on, and `ultra:suspended` (dimmed) while the active agent runs under `ask-first`. |
| ACP editors | The **Ultra** toggle in the session config toolbar, or `/ultra [on\|off]`. |
| Config file | `"ultra": true` in `~/.spettro/config.json`. |

The setting is saved to your user config, so it carries over to new
sessions and applies wherever Spettro runs: the TUI, ACP editors, `/goal`
loops and headless mode. It takes effect from the next turn, because the
system prompt is fixed for the length of a run to keep prompt caching
hitting. While it is on, every turn behaves as if the message contained
the keyword: consent is not asked again, and the prompt section is
appended on every turn. That is the cost of a standing mode, and the
reason the keyword exists for the turns where you want it once.

Under `ask-first`, `/ultra` is **suspended** rather than refused. You can
turn it on, and the choice is saved, but it does not engage until the
permission is `restricted` or `yolo`: the workflow tool refuses to run
under `ask-first`, so the standing guidance would only steer the agent at
a tool whose every call is refused. Dropping back to `ask-first` later
suspends it the same way, and raising the level again resumes it without
another `/ultra`.

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

`export const meta` must be a **pure object literal**: no variables,
calls, spreads or template interpolation. Spettro checks that before it
evaluates anything, on the parsed header alone. Objects, arrays, strings,
numbers (with an optional sign), booleans, `null`, `undefined`, `NaN`,
`Infinity`, template strings with no `${…}`, and `+` between those — a
long description is usually a chain of concatenated strings — pass.
Anything else fails with no code run: a call, a variable, a spread, any
other operator, a regex literal. A header that tries to
compute something fails there instead of quietly doing work before you
have seen what the workflow is. A regex is refused because some patterns
take exponential time to compile or match, and a timer cannot interrupt
that, so it would freeze `/workflows` listings.

Only a header that passes is evaluated, in a JS runtime with every global
removed (`String`, `JSON` and `Math` included) and a one-second time
limit. That evaluation is a backstop. The literal-only check is what
keeps a header from running code.

| Field | Required | Meaning |
| --- | --- | --- |
| `name` | yes | short identifier, also the run's display name |
| `description` | yes | one line: what this workflow does |
| `whenToUse` | no | shown in `/workflows` listings |
| `phases` | no | `[{title, detail}]`, declared up front so the panel can draw the plan before the first agent starts. A script can still [add phases at runtime](#dynamic-phases). |
| `params` | no | the arguments the script takes, checked before any agent runs; see [Templates](#templates) |
| `model` | no | note for readers that the script pins a model |

### Globals

| Global | Behaviour |
| --- | --- |
| `agent(prompt, opts)` | Run one sub-agent. Resolves to its final text, or to the parsed object when `opts.schema` is set, or to **`null`** if it failed. |
| `parallel(thunks)` | Run every thunk concurrently and wait for all of them — a **barrier**. A thunk that throws resolves to `null` instead of rejecting the call. |
| `pipeline(items, ...stages)` | Push each item through every stage independently, with **no barrier** between stages. Each stage gets `(prevResult, originalItem, index)`; a stage that throws drops that item to `null`. |
| `phase(title, {detail}?)` | Start a progress group. Later `agent()` calls join it unless they set `opts.phase`. A title not in `meta.phases` is [added at runtime](#dynamic-phases). |
| `log(message)` | Emit a progress line to the user and to the run's result. |
| `args` | Whatever the tool call passed in (`undefined` if nothing), with [declared params](#templates) defaulted and checked. |
| `budget` | `{total, spent(), remaining()}` — see [Token budgets](#token-budgets). |
| `size` | `{tier, agents, fanout, spawned(), remaining()}`, the run's [size guideline](#sizing). |
| `plan(prompt, opts)` | Ask one agent for a work-list; resolves to `[{label, prompt, phase?, data?}]`. See [plan()](#plan). |
| `untilDry(round, opts)` | Repeat `round` until it stops finding anything new. See [untilDry()](#untildry). |
| `checkpoint(message, data)` | Pause until the orchestrating model replies; resolves to its reply. See [Orchestrator in the loop](#orchestrator-in-the-loop). |
| `workflow(name \| {scriptPath} \| {script}, args)` | Run a saved workflow, or a script generated at runtime, as a sub-step. One level only. |

`agent()` options:

| Option | Meaning |
| --- | --- |
| `label` | Display label; defaults to the prompt's first line. |
| `phase` | Assign this call to a phase explicitly (use inside `pipeline`/`parallel` stages, where the global phase may have moved on). |
| `schema` | A JSON Schema. Spettro appends the contract to the prompt, parses the answer back, and retries with the parse error if it does not fit. |
| `agentType` | Manifest agent to run as (default `general-purpose`). Orchestrators are rejected. |
| `model` | Pin this call to a different model than the session's. |
| `effort` | `off`/`low`/`medium`/`high`/`x-high`/`max` thinking level for this call. |
| `isolation` | `"worktree"` gives the sub-agent its own git worktree and branch, merged back when it finishes (see [Workspace isolation](#workspace-isolation-worktrees)). |

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

A *script* error inside a `parallel` thunk or a `pipeline` stage also
turns that branch into `null`, so the other branches keep their results.
That never happens silently. The run logs the branch, the error and the
script line it was thrown on:

```text
pipeline(): item 3 at stage 2 dropped to null: TypeError: Cannot read property 'findings' of undefined (adaptive-audit.workflow.js:104:22)
parallel(): item 0 dropped to null: ReferenceError: verdcit is not defined (review.workflow.js:57:9)
```

A failed `agent()` does not throw, so it never shows up here. It is
counted as a failure instead.

A script that does not compile is rejected before anything runs. The
error quotes the offending line with a caret under the column, so a long
prompt line does not have to be counted by hand. When the line has more
than two backticks, the error also points out the usual cause: a
backtick quoted inside a template literal ends that literal.

Transient provider failures (rate limits, availability) are retried per
agent with exponential backoff: 3 s, 6 s, 12 s.

### Workspace isolation (worktrees)

Agents that *edit* files crowd a shared checkout: many of them writing
into one working tree, one `git status` full of everyone's changes, and
an edit by one landing under another's feet. Setting
`isolation: "worktree"` on an `agent()` call (or on a single `agent`
tool delegation) gives that sub-agent a workspace of its own instead:

1. Spettro creates a git **worktree** under
   `.spettro/worktrees/<instance>-<id>/` in the project root, on a fresh
   **branch named after the sub-agent** (`spettro/code-3-a1b2c3`), forked
   from the current `HEAD`.
2. The sub-agent runs with its cwd inside that worktree (at the same
   relative directory the session is in), so concurrent edits never
   collide and the main checkout stays clean (`.spettro/` is auto-added to
   `.git/info/exclude`). Because `.spettro/` is never checked out into a
   worktree, a sub-agent there reads the project's operator state from the
   main checkout: hooks (`.spettro/hooks.json`), the allow-always command
   and network lists, prompt overrides, instruction files and project
   skills. An allow-always choice made while it runs is saved to the main
   checkout too, so it outlives the worktree.
3. When the call finishes, its branch is **merged back** into the main
   checkout, one merge at a time. Leftover uncommitted work is committed
   first, with a Conventional Commits message written by the LLM from the
   diff (the same machinery as auto-commit; a stock
   `spettro: subagent … work` message is the fallback if that fails).
   After a clean merge the branch and its worktree are **deleted**.

The merge happens once per call, after the last attempt, so a
[schema](#globals) retry reuses the same worktree. Outcomes:

| Status | Meaning |
| --- | --- |
| `merged` | Branch merged into the main checkout, then deleted. |
| `no_changes` | The agent changed nothing; worktree and branch deleted. |
| `conflict` | The merge conflicted: it was aborted and the **branch and worktree are kept** for manual resolution. |
| `preserved` | The agent failed but left work behind; branch and worktree are kept so nothing is lost. A failed agent with nothing to keep has its worktree deleted. |
| `error` | A git step failed; the branch and worktree are kept. |

A conflict or error in a workflow is listed in an `<unmerged>` block of
the run's result, with the branch and worktree path, and the
orchestrating agent is told to resolve it; the same note appears as a
log line in the live view (the TUI panel, the ACP tool card) as it
happens. A single `agent` delegation reports the
outcome in its JSON result (`workspace.merge_status`). Resolve a kept
branch by merging it by hand, then `git worktree remove <path>` and
`git branch -D <branch>`; leftovers also show up in
[`/storage`](storage.md).

Worktree isolation requires the project to be a git repository with at
least one commit. Leave `isolation` unset for read-only fan-outs
(research, review, search): a worktree would only add overhead there.

### What is missing on purpose

`Date.now()`, `Math.random()` and argless `new Date()` all throw. A
workflow has to replay identically when [resumed](#resuming-a-run), and
a script that stamps wall-clock time or randomises a prompt cannot.
Pass timestamps in through `args`, and vary work by item index.

`new Date(0)` and friends still work — only the clock is gone.

## Dynamic workflows

A script with a hardcoded list of files or angles replays the plan its
author had in mind, which goes stale. The task's shape usually shows up
only once agents start reading. Spettro's answer is ordinary control flow
plus a few helpers, so the script decides the shape at runtime:

```javascript
phase('Plan')
const slices = await plan(`Split ${args.scope} into slices to audit for ${args.concern}.`)

phase('Audit')
const findings = await untilDry(async (round, seen) => {
  if (round > 0) phase(`Sweep ${round}`, { detail: `${seen.length} findings so far` })
  const found = await parallel(slices.map(s => () =>
    agent(s.prompt, { label: s.label, schema: FINDINGS })))
  return found.flatMap(f => f?.findings ?? [])
}, { key: f => `${f.file}:${f.line}` })
```

[`adaptive-audit.js`](examples/workflows/adaptive-audit.js) is the full
version: plan, find-then-refute in a pipeline, sweep until dry, then a
checkpoint before anything is fixed.

### Dynamic phases

`meta.phases` is the plan you know before the run starts. `phase(title,
{detail})` with a title that is not declared adds a phase at runtime, in
the order the script reaches it. The TUI panel and the ACP card mark an
added phase so you can tell it from the declared plan. Only `phase()`
moves the run into a phase, declared or not: `opts.phase` on an `agent()`
call files that one agent under a title, but the run's current phase, its
log lines, its phase list and its [automatic
checkpoints](#orchestrator-in-the-loop) still follow the last `phase()`
call. A phase the script
never reaches never appears, which is why
[`adaptive-audit.js`](examples/workflows/adaptive-audit.js) declares only
its two fixed stages and adds its sweep rounds and the fix stage when, and
if, it gets there. `meta.phases` is optional: a script can declare none
and add every phase as it goes.

### `plan()`

`plan(prompt, opts)` asks one agent for a work-list and resolves to an
array of `{label, prompt, phase?, data?}` tasks, ready to hand to
`parallel` or `pipeline`. The structured-output schema is built in, so
you write only the question.

| Option | Meaning |
| --- | --- |
| `max` | Most tasks to keep (default `size.fanout`). Anything past it is dropped, and the drop is logged with a count. A cap is never silent. |
| `label`, `phase`, `agentType`, `model`, `effort` | Passed to the planning agent, as for `agent()`. |

A planner that fails resolves to `[]`, so check the length before you fan
out.

### `untilDry()`

`untilDry(round, opts)` is the [loop-until-dry](#patterns) pattern,
written once. It calls `round(i, seen)` (`i` counts rounds from 0, `seen`
is every item found so far), which returns an array of items or a promise
of one. Each item is deduplicated against **everything seen**, not
against what survived later filtering, so a rejected finding that turns
up again does not count as new. The loop stops at the first of:

| Stop | Default |
| --- | --- |
| `opts.dry` consecutive rounds with nothing new | 2 |
| `opts.maxRounds` rounds | 8 |
| `budget.remaining()` reaching 0 | — |

`opts.key(item)` is the identity used for deduplication (default: the
item's JSON with object keys sorted at every level, so two agents that
build the same object in a different key order still match; a plain
`JSON.stringify` key would not). Agents rarely word the same finding the same way
twice, so pass a key built from stable fields such as file and line.
The result is every new item in the order it was found, and each round
logs how many items it found and how many were new.

### Generated sub-scripts

`workflow({script: source}, args)` runs a script that only exists at
runtime, for example one an agent wrote for a stage nobody planned. The
source gets the same checks as any workflow: a header that parses and a
body that compiles. If it fails them, the call rejects with the reason
instead of taking the parent down, so the parent can catch the error and
ask for a corrected script. The child shows as a nested run under its own
`meta.name`, shares the parent's pool, counters and budget, and cannot
nest further.

## Orchestrator in the loop

Without checkpoints, the orchestrating model only sees a workflow's
result at the end. `checkpoint(message, data)` lets a script stop
halfway and ask:

```javascript
const reply = await checkpoint(
  `${confirmed.length} findings survived refutation. Which should be fixed?`,
  { confirmed })
if (!reply?.fix?.length) return { confirmed }
phase('Fix')
// … fix only what the orchestrator chose
```

When the script reaches the checkpoint, the run **pauses** and the
`workflow` tool call returns to the model with the message, the data
(JSON, cut to 24 KB) and progress so far:

```text
<workflow_checkpoint name="adaptive-audit" run_id="wf_…" checkpoint_id="cp-1" phase="Audit">
<message>7 findings survived refutation. Which should be fixed?</message>
<data>{"confirmed":[…]}</data>
<progress>23 agents · 1 failed · 0 replayed · 412310 tokens; phases: Plan → Audit</progress>
<log>…</log>
</workflow_checkpoint>
```

The model reads the data, decides, and calls the tool again with:

| Arguments | Effect |
| --- | --- |
| `{"continue_run_id": "wf_…", "reply": <any JSON>}` | `checkpoint()` resolves to `reply` and the run carries on, until it settles or reaches the next checkpoint. |
| `{"continue_run_id": "wf_…", "stop": true}` | Stops the run and returns its partial result. Allowed even under `ask-first`, since it starts nothing; continuing is not, because it restarts sub-agents. |

`checkpoint_id` may be added to either. If it does not name the pending
checkpoint, the call is refused, so a reply meant for one question
cannot answer another. Continuing never asks for consent again: you gave
it when the run started.

**Nothing runs while a run is paused.** The pause is surfaced only once
no `agent()` call is in flight anywhere in the run, nested `workflow()`
children and calls still queued for a concurrency slot included. Any
agent the script starts after the checkpoint waits until the reply
arrives. Once the tool call returns, the turn may end, and a sub-agent
still running would report into a turn that no longer exists. A
`checkpoint()` call inside a child workflow joins the same queue.
Several pending checkpoints are answered one at a time, in the order they
were reached.

**Automatic checkpoints.** `auto_checkpoint: true` on the tool call
pauses at every phase boundary once at least one agent has run, with the
message `phase <previous> finished; next: <title>` and the data
`{finished_phase, next_phase, agents, failed}`. Reply `{"stop": true}` to
end the run there; any other reply continues it. Use it to watch a
script you do not trust yet without editing checkpoints into it.

**How long a paused run lives.** A paused run is not tied to the tool
call that started it:

- **TUI and ACP.** The session owns its paused runs, so one can be
  continued in a later turn, after you have weighed in. That includes
  every iteration of a TUI or ACP `/goal` or `/loop`: they share the
  session's runs, so a run paused in one iteration can be continued in
  the next. `/clear`, switching or resuming another session, closing an
  ACP session, and quitting stop every paused run.
- **Headless**, including `spettro --goal`. Paused runs belong to the
  turn, so they must be continued within it, which is what the model
  does anyway.
- **Esc** stops a run that is *running*, as before. A run that is
  paused is not running, and outlives the call that returned it.
- **Idle reaper.** A run left paused for 30 minutes without a continue
  is stopped. Its journal is kept, and the result tells the model it can
  pick the run up with `resume_from_run_id`.

A run stopped on purpose — answered with `stop`, reaped while idle, or
stopped because its session ended — is **stopped**, not failed. Its
journal and partial result are kept like any other run's, and the panel
and the ACP card show `■ stopped: <reason>` in a neutral colour rather
than the failure red. The reason says what stopped it: the orchestrator,
the idle reaper, or the session ending (`/clear` in an ACP editor says
`the conversation was cleared`). A paused run
stopped by the reaper or by `/clear` has no tool call left to report
through, so the host is told separately and closes the panel or card
itself. Otherwise it would read `waiting for orchestrator` for good.

Continuing a run that is no longer live is an error that lists the runs
that are. If the run's journal still exists, the error says to resume it
instead.

**Continuing in a later turn.** You do not have to repeat `ultracode`,
or ask for a workflow again, to answer a paused run. While the session holds a paused run, every turn
gets the `workflow` tool and its guidance, plus a line listing each
paused run: its run ID, name, checkpoint ID and message. "Fix 1 and 3",
written in the next message, is enough. The guidance is the
judge-it variant unless ultracode is on, so a *new* run the model starts
in that turn still asks for your consent as usual. Continuing never asks.
The list is computed once when the turn starts, which keeps the prompt
byte-stable for caching.

**Resuming replays answers.** Answered checkpoints are written to the
journal like agent calls. A run [resumed](#resuming-a-run) from it gets
the recorded reply back at the same `checkpoint()` without pausing (the
panel marks it as replayed), so an edited script does not ask the same
question twice. A checkpoint the journal has no answer for pauses and
waits for the orchestrator, as on a fresh run. Explicit `checkpoint()`
answers are keyed by the message and the data, so one whose data
changed asks again. Automatic phase-boundary checkpoints are keyed by their message alone
(`phase <previous> finished; next: <title>`). Their agent and failure
counts are left out on purpose: a resumed run re-runs the agents that
failed, so it can reach the same boundary with different counts. A
changed count therefore does not make an automatic checkpoint ask again,
and the recorded reply replays.

**No orchestrator.** A host with no model to answer resolves
`checkpoint()` to `null` immediately and logs `checkpoint skipped (no
orchestrator): <message>`. Write scripts so that `null` means "do
nothing irreversible". `adaptive-audit.js` fixes nothing unless the reply
names what to fix.

## Limits

| Limit | Value | Why |
| --- | --- | --- |
| Concurrent agents | `min(16, CPUs - 2)` (4 at the `small` [size](#sizing)), or `max_concurrency` | Excess calls queue; a 100-item `parallel` still completes, just not all at once. |
| Agents per run | 1000 | Runaway-loop backstop, set far above any real workflow. The [size guideline](#sizing) is the number to plan around. |
| Items per `parallel`/`pipeline` call | 4096 | An explicit error, never a silent truncation. |
| Tool timeout | 2 hours | A workflow is many full agent turns. |
| Paused run, idle | 30 minutes | A [checkpoint](#orchestrator-in-the-loop) nobody continues is stopped, and its journal kept. |

## Tool arguments

What the model passes to the `workflow` tool. A new run takes one of
`script`, `script_path` or `name`; a continue call takes `continue_run_id`
instead.

| Argument | Meaning |
| --- | --- |
| `script` | The script source, written for this task. |
| `script_path` | A script file: in the workspace, a run directory, or a saved-workflow folder. |
| `name` | A saved workflow, run as it is. |
| `show` | With `name` (or a `script_path` in a saved-workflow folder or a run's `script.js`): return the script's source and params without running anything. Other files are the file tools' to read. |
| `args` | Any JSON value; the script's `args`. |
| `save_as`, `save_scope` | Also save the script under this name, to the project (default) or `global` folder. See [Templates](#templates). |
| `resume_from_run_id` | Replay unchanged agent calls and answered checkpoints from that run's journal. |
| `max_concurrency` | Concurrent agents for this run. |
| `budget_tokens` | Token ceiling for this run; see [Token budgets](#token-budgets). |
| `size` | `small`, `medium`, `large` or `unbounded`: the [size](#sizing) tier for this run only. |
| `auto_checkpoint` | Pause at every phase boundary. |
| `continue_run_id` | Continue the paused run with this ID. |
| `reply` | With `continue_run_id`: the value `checkpoint()` resolves to. |
| `stop` | With `continue_run_id`: stop the run and return its partial result. |
| `checkpoint_id` | With `continue_run_id`: must name the pending checkpoint if given. |

## Sizing

How big a workflow should be is your call as much as the model's: a quick
check and an exhaustive audit want very different scripts, and nothing in
the task text says which one you meant. The **size tier** says it, as a
number that both the model and the script can plan around:

| Tier | Agents per workflow | `size.fanout` | Default concurrency |
| --- | --- | --- | --- |
| `small` | ~5 | 3 | 4 |
| `medium` (default) | ~10 | 6 | `min(16, CPUs - 2)` |
| `large` | ~30 | 16 | `min(16, CPUs - 2)` |
| `unbounded` | no guideline | 64 | `min(16, CPUs - 2)` |

The tier reaches the model as one line of its workflow guidance ("keep
each workflow under ~10 agents") and reaches the script as the `size`
global:

| Field | Meaning |
| --- | --- |
| `size.tier` | the tier's name |
| `size.agents` | the guideline (`Infinity` when `unbounded`) |
| `size.fanout` | a suggested width for one fan-out, and the default `max` for `plan()` |
| `size.spawned()` | agents started so far in this run |
| `size.remaining()` | agents left under the guideline (`Infinity` when `unbounded`) |

It is a **guideline, not a limit**. A run that goes past `size.agents`
keeps going. The run's log says so once, `size guideline (medium: ~10
agents) exceeded`, and the result the model reads ends with how many
agents ran against the guideline, so the overrun is in front of it when
it writes the next script. The hard stop is still
the 1000-agent backstop in [Limits](#limits).

Set the tier:

- for every session: `/workflows size <tier>` in the TUI (no argument
  shows the current tier and the table), the `workflow_size` selector or
  `/workflow-size <tier>` in ACP editors, or `workflow_size` in
  [`config.json`](configuration.md#workflows);
- for one run: the model passes `size` on the tool call, for instance
  `small` for a quick check during a large session.

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

The target is a **hard ceiling on starting work**: once `spent()` reaches
`total`, further `agent()` calls throw. Agents already running when that
happens finish, so a wide fan-out can end somewhat past the total; size
each wave against `remaining()` when the overshoot matters. Guard the
loop on `budget.total` — with no target set, `remaining()` is `Infinity`
and the loop would run to the 1000-agent cap.

You can set the budget yourself from the message, with a standalone `+`
and an amount in thousands or millions of tokens:

```text
ultracode: find every unchecked error in internal/ +500k
```

`+500k`, `+750K`, `+1.5m` and `+2M` all work. The directive counts only
in a message that turns workflows on (or while `/ultra` is on), and
lights up in the input box with the keyword. It is **one pool for the
whole turn**, not a budget per run: each workflow started in that turn
gets what earlier runs left over as its default `budget_tokens`. Once the
pool is spent, a new run that sets no `budget_tokens` of its own is
refused before any agent starts, with an error saying the turn's budget is
spent. It does not start with a token or two and dispatch a full first
wave anyway. An explicit `budget_tokens` on a call takes precedence over
the pool, which is how the model overrides it when you ask. The model is
told the total in its prompt.

## Watching a run

**TUI.** Under the transcript you get a summary: the phase the run is in,
overall progress, and what is running *right now* — finished agents are
history, and history is what the side panel is for. It scales with the
terminal and never takes more than a few rows, because a running workflow
must not be the reason you cannot read what the agent just said. Once the
run ends it collapses to a single line and gives the rows back.

It shares one budget — about a quarter of the terminal height — with
ordinary delegations and the todo list, so the three of them
together stay bounded no matter how much is in flight. Each block says
how many entries it left out; nothing is hidden silently.

`ctrl+b` has the whole thing: every declared phase drawn from the start
(dimmed until reached), filling in as agents land under it, with
per-phase meters, each agent's live tool call, `log()` lines, and
replayed-from-journal markers. The tree survives the turn that produced
it and is cleared when the next one starts.

The title carries the size tier (`workflow adaptive-audit · medium`, plus
the token budget when one is set). Phases added at runtime are marked
with a `+`. A run paused at a checkpoint shows `⏸ paused at cp-1 —
waiting for orchestrator: <message>` and stops animating. Unlike a
finished run, a paused one is kept when the next turn starts, because
that turn is usually the one that continues it. A run that was stopped
on purpose ends with a neutral `■ stopped: <reason>` instead of the red
failure mark; see [stopped runs](#orchestrator-in-the-loop).

**ACP editors.** The run opens a single `workflow <name>` tool call
whose content is rewritten as it progresses, so the editor shows the
same phase tree growing in place. Each sub-agent additionally gets its
own tool call, so "follow the agent" navigation keeps working. A paused
run keeps its card in progress with a `⏸ waiting for orchestrator` line.
When a later turn continues the run, it keeps the phases, agents and log
lines it already had. A stopped run closes its card as completed, not
failed, with a `■ stopped: <reason>` line, even when the stop happens
between turns (see [ACP](acp.md)).

## Saved workflows

The agent writes these too. By default it **generates** a fresh script
for the task in front of it. A saved workflow is a starting point for
that, not a recording to play back; see [Templates](#templates). If a
script is worth keeping, the agent sets `save_as` and the script lands in
the folder below.

Three scripts ship in [`docs/examples/workflows/`](examples/workflows/):
`review-branch.js` (review, then adversarially refute each finding),
`explain-subsystem.js` (multi-angle sweep, synthesis, critique) and
`adaptive-audit.js` (a runtime-planned audit with a checkpoint before any
fix). Copy any of them into one of the folders below to make it available
by name.

Scripts in either of these folders are reusable by name:

- `.spettro/workflows/<name>.js` — project
- `~/.spettro/workflows/<name>.js` — global

A project script shadows a global one with the same name.

| Command | Description |
| --- | --- |
| `/workflows` | List saved workflows with their descriptions and phases. |
| `/workflows show <name>` | Print a script's header and source. |
| `/workflows run <name> [json \| text]` | Hand one to the agent, with JSON `args` or a description of the task. |
| `/workflows size [tier]` | Show or set the [size tier](#sizing). |
| `/workflows where` | Show the directories being scanned. |

`/workflows run` does not execute the script behind the agent's back: it
dispatches a turn that points the agent at the saved template. The agent
reads it, checks that it fits, adapts anything task-specific or stale and
runs the adapted script inline. If the template fits as it is, the agent
runs it by name with the args. Trailing text that is not JSON is passed
along as the task description, not rejected. Results are only useful to
someone who then acts on them, and this keeps one execution path for
scripts the model wrote and scripts you handed it.

The same commands are available over ACP (`/workflow-size` there sets
the tier).

A script can call another with `workflow('name', args)`, or run one it
[generated at runtime](#generated-sub-scripts) with `workflow({script},
args)`. The child shares the parent's concurrency pool, agent counter and
token budget. Nesting is one level deep — a `workflow()` call inside a
child throws.

### Templates

A saved script that hardcodes "these eight files" replays its author's
plan and goes stale as the code moves. Spettro treats saved workflows as
**templates**:

- **The agent adapts rather than replays.** It reads the saved script
  (the workflow tool with `{"name": …, "show": true}`, which also reaches
  global templates outside the workspace), keeps the shape, and rewrites
  whatever is specific to the old task. Work-lists are discovered at
  runtime (`plan()`, a scouting agent, a `glob`), never copied from the
  last run. It runs a template by name only when the template fits as it
  is.
- **Templates declare their inputs** in `meta.params`, so a run can
  differ without an edit:

```javascript
export const meta = {
  name: 'adaptive-audit',
  description: 'Audit part of the repo for one class of defect',
  params: {
    concern: { type: 'string', required: true, description: 'what to look for' },
    scope:   { type: 'string', default: '.', description: 'directory to audit' },
    fix:     { type: 'boolean', default: false },
    note:    'free-form guidance for the finders',   // shorthand: an optional string
  },
  phases: [{ title: 'Plan' }, { title: 'Audit' }],
}
```

| Param field | Meaning |
| --- | --- |
| `type` | `string`, `number`, `boolean`, `array`, `object` or `any` (default `any`) |
| `description` | shown in the run confirmation and in the prompt that `/workflows run` dispatches |
| `required` | the run fails if it is missing |
| `default` | used when the param is missing |

`args` is checked against the params **before any agent runs**. Missing
args become `{}` and defaults are filled in. A missing required param, or
a value of the wrong type, fails the run with an error naming the
params, such as `workflow "adaptive-audit": missing required param(s):
concern — pass them in the tool call's args`. That costs one tool call,
not a dozen agents. When exactly one param is required and `args` is not
an object (`"args": "unchecked errors"`), the value is bound to that
param.

When the agent saves a script with `save_as` that declares no params and
never reads `args`, the tool result carries a note: the workflow will
replay the same hardcoded work every time, so consider declaring params.
It is only a note, and the save still happens.

## Run artifacts and resuming

Every run writes to `<session>/workflows/<run_id>/`:

| File | Contents |
| --- | --- |
| `script.js` | the exact source that ran |
| `meta.json` | the parsed header, params included |
| `journal.jsonl` | one record per agent call (prompt hash, label, phase, output) and per answered checkpoint (marked `kind: "checkpoint"`, with the reply) |
| `result.json` | the script's return value |

A run paused at a checkpoint has no `result.json` yet. The journal is
written as it goes, so a run that was stopped while paused, by the idle
reaper, `/clear` or quitting, can still be resumed.

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

Answered checkpoints are keyed the same way, by a hash of the message and
the data. A resumed run gets the recorded reply without pausing, and a
checkpoint whose data changed, or that the journal never answered, pauses
again. Automatic phase-boundary checkpoints are keyed by their message
alone, so their changing agent counts do not make them ask again. Journals written before
checkpoints existed have no `kind` field; every entry in them is read as
an agent call.

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
converges. [`untilDry()`](#untildry) is this pattern, built in.

**Plan at runtime.** Have one agent turn the scope into a work-list
([`plan()`](#plan)) instead of hardcoding one. The list fits the code as
it is today, and the script can be saved as a template that still works
next month.

**Checkpoint before acting.** Discover and verify freely, then
[`checkpoint()`](#orchestrator-in-the-loop) before anything that writes,
and let the orchestrator choose what to act on. Treat a `null` reply as
"no".

**Multi-modal sweep.** Parallel agents each searching a different way
(by container, by content, by entity, by time), each blind to what the
others surface.

**Completeness critic.** A final agent asking "what is missing — a
modality not run, a claim unverified, a source unread?" What it finds
becomes the next round.

## When *not* to use one

- A single delegation → the `agent` tool.
- A trivial single-step task → just do it.

A workflow multiplies token usage: every
`agent()` call is a full agent run on the active model.
