# Storage report and cleanup

Spettro accumulates data under `~/.spettro` (global) and `<project>/.spettro`
(per project): checkpoint shadow repos, saved sessions, caches. `/storage`
shows what is on disk and `/storage clean` (TUI) or `spettro clean` (CLI)
reclaims it safely. Cleanup is always user-invoked and previewed — there is no
automatic background deletion (automatic per-project checkpoint retention is
separate; see [Checkpointing](checkpointing.md)).

## What Spettro writes (inventory)

Every artifact class is declared in a single registry (`internal/storage`)
with a class, sizer, and — only for reclaimable classes — a path-validated
deleter. Cleanup operates exclusively on registered items; anything under
`~/.spettro` the registry does not claim is reported as *unknown* and never
deleted.

| Path | What | Class | Safe to delete? |
| --- | --- | --- | --- |
| `~/.spettro/history/<hash>/` | Checkpoint shadow repo + conversation blobs per project | history | Yes — loses `/rewind` history for that project only |
| `~/.spettro/sessions/session-*/` | Saved conversations, tasks, events | history | Yes — loses `/resume` for that session |
| `~/.spettro/catalog.json` | models.dev catalog cache | cache | Yes — re-fetched on demand |
| `~/.spettro/memory-inbox.json` | Mined-fact candidates | cache | Yes — loses pending `/memory review` items (not preselected) |
| `~/.spettro/skills/` | Installed skills | user | Only via `/skill uninstall` |
| `~/.spettro/memory.md`, `config.json`, `keys.enc`, `master.key`, `trusted.json`, `telegram.json`, `allowed_commands.json`, `commands/` | Config, secrets, user content | secret/user | **Never** — not even listed in the cleaner |
| `<project>/.spettro/cache/` (e.g. `symbols.json`) | Regenerable caches | cache | Yes — rebuilt lazily |
| `<project>/.spettro/memory.md`, `commands/` | User content | user | **Never** |
| `$TMPDIR/spettro-spool-*` | Oversized tool outputs from crashed sessions | cache | Yes, when dead (untouched > 48 h and not the live session's spool) |

## `/storage` (TUI)

- `/storage` — report: one row per artifact class with size and entry count
  (global + current project), orphaned-history count, unknown entries, total,
  and the "safe to reclaim" size.
- `/storage clean` — interactive multi-select over every deletable item, with
  the safe defaults preselected:
  - **orphaned history** — `history/<hash>` entries whose recorded project
    path no longer exists on disk;
  - **old sessions** — older than 30 days (`clean_session_age_days`), never
    the active session, and always keeping the most recent 5 per project
    (`clean_keep_sessions`);
  - **catalog cache**, **project caches**, and **dead spool dirs**.

  Keys: `↑`/`↓` move, `space` toggle, `a` all, `n` none, `enter` delete,
  `esc` cancel. Secret and user classes are not listed at all — not even
  opt-in. If the current project's own checkpoint store is deleted, the live
  Checkpointer re-initializes lazily on the next snapshot.

## `spettro clean` (CLI)

Works without the TUI. Dry-run by default:

```sh
spettro clean            # print the report and the safe-default plan; delete nothing
spettro clean --yes      # execute the safe-default plan
spettro clean --days 60  # override the session age threshold
spettro clean --keep 10  # override sessions kept per project
```

## Orphan detection

The history dir name is `sha256(projectPath)` — one-way — so each project's
`checkpoints.json` records a `project_path` field (written on every
snapshot). An entry is *orphaned* when that path no longer exists. Entries
written before path recording show as "unknown project" and are never
preselected; select them manually if you know they are stale.

## Configuration

| `config.json` key | Default | Meaning |
| --- | --- | --- |
| `clean_session_age_days` | `30` | Sessions not updated within this many days become clean candidates. |
| `clean_keep_sessions` | `5` | The most recent K sessions per project always survive, regardless of age. |

See also: [Checkpointing](checkpointing.md) (automatic per-project
retention), [Session lifecycle](session.md), [Configuration](configuration.md).
