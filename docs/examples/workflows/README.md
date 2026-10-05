# Example workflows

Ready-to-run [workflow](../../workflows.md) scripts. Copy one into a folder
Spettro discovers and it becomes available by name:

```bash
mkdir -p .spettro/workflows                       # this project only
cp docs/examples/workflows/review-branch.js .spettro/workflows/

mkdir -p ~/.spettro/workflows                     # every project
cp docs/examples/workflows/explain-subsystem.js ~/.spettro/workflows/
```

Then:

```text
/workflows                                        list what is available
/workflows show review-branch                     read it before running it
/workflows run review-branch {"base": "develop"}  run it with args
/workflows run adaptive-audit {"concern": "errors that are logged and then dropped", "scope": "internal/agent"}
```

A saved workflow is a [template](../../workflows.md#templates), not a
recording: `/workflows run` hands it to the agent, which checks that it
fits the task, adapts what does not, and runs the result.

| Script | What it does |
| --- | --- |
| [`review-branch.js`](review-branch.js) | Reviews the branch across independent dimensions in parallel, then spawns a skeptic per finding whose job is to *refute* it. Only findings that survive are reported. Demonstrates `pipeline` (no barrier between review and verification), structured output, and adversarial verification. |
| [`explain-subsystem.js`](explain-subsystem.js) | Explains a subsystem by reading it from five deliberately different angles at once, synthesising one account, then critiquing that account against the code. Demonstrates `parallel` as a genuine barrier, phase progression, and a completeness critic. |
| [`adaptive-audit.js`](adaptive-audit.js) | Audits part of the repo for one class of defect without knowing the work-list up front: `plan()` splits the scope into slices at runtime, each slice is found-then-refuted in a `pipeline`, and `untilDry()` keeps planning new angles until two rounds turn up nothing new. With `fix: true` it stops at a `checkpoint()` and fixes only what the orchestrator approves. Demonstrates [dynamic workflows](../../workflows.md#dynamic-workflows): `meta.params`, phases added at runtime, `size`, and the [orchestrator in the loop](../../workflows.md#orchestrator-in-the-loop). |

`review-branch` and `explain-subsystem` are read-only: they run searches,
reads and `git diff`, and change nothing. `adaptive-audit` is read-only
too unless you pass `fix: true` *and* the orchestrator names findings to
fix at its checkpoint; each fixer then works in its own git worktree.
Treat all three as starting points — a workflow is just a script, and the
useful ones are usually shaped around your repository.
