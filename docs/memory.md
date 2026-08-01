# Persistent Memory

Spettro can remember short facts and preferences across sessions. Memory lives
in two plain-Markdown files:

| Scope | File | Contents |
|-------|------|----------|
| user | `~/.spettro/memory.md` | Preferences that apply everywhere (style, language, workflow). |
| project | `<repo>/.spettro/memory.md` | Facts specific to one repository (conventions, commands, layout). |

At session start the combined content of both files is appended to every
agent's system prompt as a `# Memory` section, so saved preferences are
honored automatically. Project facts are injected before user facts,
recently-used facts first.

## Fact metadata

Each bullet carries an HTML-comment tail that is invisible in rendered
Markdown and stripped before prompt injection:

```
- prefers table-driven tests <!-- id:m-a1b2c3 added:2026-07-23 used:2026-07-23 -->
```

- `id` — stable short hash of the fact text, used by `/memory curate` ops.
- `added` — date the fact was first saved.
- `used` — bumped when the same fact is saved again or curation confirms it.

Legacy bare bullets (no comment) stay valid; they are stamped automatically
the first time the file is rewritten (a dedupe bump or a curation op). The
files remain hand-editable Markdown — edit freely, the tail is optional.

## The `save-memory` tool

Agents with the `save-memory` tool (default: `coding`, `ask`, `code`) can
persist a fact when you ask them to remember something:

```json
{"fact": "prefers table-driven tests", "scope": "project"}
```

- `fact` — one short line (max 500 chars).
- `scope` — `user` (default) or `project`.

Saving is dedup-aware:

- **Exact duplicate** (same text after normalization) — nothing is appended;
  the existing fact's `used:` date is bumped.
- **Near-duplicate or likely contradiction** (high token overlap or same
  leading phrase, e.g. "prefers tabs" vs "prefers spaces") — the new fact is
  routed to the review inbox as a *supersede candidate* instead of being
  appended. Resolve it with `/memory review`: approving replaces the old
  fact with the new one; discarding keeps the old fact.
- Otherwise the fact is appended with fresh metadata.

## The `/memory` command

| Command | Effect |
|---------|--------|
| `/memory` or `/memory show` | Print both memory files, their paths, and the pending inbox count. |
| `/memory edit [user\|project]` | Open the file in `$EDITOR` (default `vi`). |
| `/memory clear [user\|project\|all]` | Erase saved memory (default: all). |
| `/memory mine [n]` | Scan up to `n` (default 10) recent saved sessions of this project in the background and draft candidate memories into the review inbox. |
| `/memory review` | Open the review inbox dialog. |
| `/memory curate [user\|project\|all]` | One LLM pass over the saved facts proposing merges, rewrites, and deletions; each op is applied only after you approve it. |

## Mining and the review inbox

`/memory mine` sends recent session transcripts to the active model and asks
it to extract recurring durable signals — stable preferences, project
conventions, repeated corrections. The run happens in the background (you can
keep chatting) and finishes with a banner.

Drafted candidates land in `~/.spettro/memory-inbox.json`, deduplicated
against both the inbox and your existing memory. **Nothing in the inbox is
ever loaded into agent context**: a candidate only becomes active memory when
you approve it in `/memory review`.

In the review dialog: `↑`/`↓` navigate, `a`/`enter` approve (appends the fact
— or, for a supersede candidate, replaces the fact it collides with), `d`
discard, `esc` close. Approved memories load into context from the next
session.

## Curation

`/memory curate` sends the full fact list (with ids and dates) to the active
model in one call per scope and gets back edit operations:

- `merge` — combine overlapping facts into one (keeps the earliest `added:`).
- `rewrite` — replace a vague or outdated fact's text.
- `delete` — drop a fact that is stale or contradicted by a newer one.

For project scope, facts unused for more than 90 days whose referenced paths
no longer exist in the working tree are flagged as staleness evidence in the
prompt. Ops appear in a review dialog (`a`/`enter` apply, `d` skip, `esc`
close); each applied op rewrites the file atomically (temp+rename), and
skipped ops change nothing. Like mining, curation only runs when you invoke
it — there is no automatic or background LLM spend.

## Prompt-cache stability

The memory snapshot is loaded **once per session and frozen**. The system
prompt must stay byte-identical across every turn of a session or the
provider prompt cache misses on each request, so:

- facts saved mid-session (via `save-memory` or `/memory edit`) take effect
  at the **next** session start;
- each file's injected content is capped at 8 KB. Facts are injected
  recently-used first, so when the cap hits, the **stalest** facts are the
  ones dropped, never the freshest.

Keep memories short and stable — they are prepended to every request of
every session. Run `/memory curate` occasionally to keep the list small and
consistent.
