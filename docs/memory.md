# Persistent Memory

Spettro can remember short facts and preferences across sessions. Memory lives
in two plain-Markdown files:

| Scope | File | Contents |
|-------|------|----------|
| user | `~/.spettro/memory.md` | Preferences that apply everywhere (style, language, workflow). |
| project | `<repo>/.spettro/memory.md` | Facts specific to one repository (conventions, commands, layout). |

At session start the combined content of both files is appended to every
agent's system prompt as a `# Memory` section, so saved preferences are
honored automatically.

## The `save-memory` tool

Agents with the `save-memory` tool (default: `coding`, `ask`, `code`) can
persist a fact when you ask them to remember something:

```json
{"fact": "prefers table-driven tests", "scope": "project"}
```

- `fact` — one short line (max 500 chars).
- `scope` — `user` (default) or `project`.

Facts are appended as bullet lines; files are append-only and never
reordered.

## The `/memory` command

| Command | Effect |
|---------|--------|
| `/memory` or `/memory show` | Print both memory files, their paths, and the pending inbox count. |
| `/memory edit [user\|project]` | Open the file in `$EDITOR` (default `vi`). |
| `/memory clear [user\|project\|all]` | Erase saved memory (default: all). |
| `/memory mine [n]` | Scan up to `n` (default 10) recent saved sessions of this project in the background and draft candidate memories into the review inbox. |
| `/memory review` | Open the review inbox dialog. |

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
to the user or project memory file), `d` discard, `esc` close. Approved
memories load into context from the next session.

## Prompt-cache stability

The memory snapshot is loaded **once per session and frozen**. The system
prompt must stay byte-identical across every turn of a session or the
provider prompt cache misses on each request, so:

- facts saved mid-session (via `save-memory` or `/memory edit`) take effect
  at the **next** session start;
- memory files are append-only and capped at 8 KB each when loaded (oldest
  lines are trimmed first).

Keep memories short and stable — they are prepended to every request of
every session.
