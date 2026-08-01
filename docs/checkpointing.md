# Checkpointing and Rewind

Spettro automatically snapshots the project working tree before every
file-modifying tool call, so you can rewind files and/or conversation to any
earlier step — like a time machine for your coding session.

## How it works

Checkpointing is built on a **shadow git repository** stored in Spettro's data
directory, completely separate from the project's own `.git`. Before each
`file-write`, `file-edit`, `multi-edit`, or similar write tool, Spettro:

1. Stages all changes in the project working tree.
2. Commits to the shadow repo with a label describing the pending tool call.
3. Saves a snapshot of the current conversation alongside it.

The shadow repository lives under `~/.spettro/history/<project-hash>/repo.git`.
It has its own identity (`spettro <spettro@localhost>`), its own config, and
its own hooks path — nothing in the user's git config or hooks can interfere.
The project's `.gitignore` files are honoured, so build artifacts and
dependencies are never tracked.

### Storage design

Checkpointing is engineered to not duplicate your repository:

- **Object borrowing (alternates).** When the project has its own `.git`, the
  shadow repo points `objects/info/alternates` at the project's object store
  (worktree/submodule aware via `git rev-parse --git-common-dir`). Content
  already committed in your repo is *borrowed*, not copied — the shadow store
  only holds uncommitted deltas, so even a 20 GB repo costs almost nothing.
- **Size-capped snapshots.** Files above `checkpoint_max_file_mb` (default
  20 MB) are excluded from snapshots, along with default heavyweight patterns
  (`*.iso`, `*.qcow2`, `*.safetensors`, `*.gguf`, …) seeded in the shadow
  repo's `info/exclude` (editable there). Skipped files are recorded on the
  checkpoint, and `/rewind` warns that they are unaffected by a restore.
- **No-change fast path.** If the tree is identical to the previous
  checkpoint, no new commit is minted — the list entry points at the same
  commit and only the conversation snapshot is stored.
- **Maintenance.** The shadow repo runs with `core.untrackedCache` and
  `index.version=4` to keep `add -A` fast on large trees, `git gc --auto`
  runs every 20 snapshots, and reflogs are disabled so pruned checkpoints
  can actually be collected.
- **Retention.** On open (never in the per-snapshot hot path), checkpoints
  older than `checkpoint_retention_days` (default 14) are pruned: list
  entries and conversation blobs are deleted, their pinning refs
  (`refs/checkpoints/<hash>`) removed, and `git gc --prune=now` reclaims the
  objects. If the store still exceeds `checkpoint_max_gb` (default 5), the
  oldest half of the remaining checkpoints is dropped too.
- **Big-repo guard.** For projects *without* their own `.git` (no alternates
  available), a first snapshot must copy the tracked tree. If the project is
  larger than `checkpoint_warn_gb` (default 2), a one-time banner warns you
  before that happens; disable checkpointing with
  `"checkpointing_disabled": true` in `config.json` if you don't want it.

All keys live in `config.json` — see
[Configuration](configuration.md#checkpointing-storage).

### The alternates caveat

Because the shadow repo borrows objects from your project's `.git`, an
aggressive `git gc --prune` in the project can delete objects an old
checkpoint still needs. Checkpoints are a convenience cache, so this is
accepted: before restoring, Spettro verifies every object is reachable and
fails with a clear "checkpoint is no longer restorable" error instead of
corrupting the working tree. Newer checkpoints are unaffected.

## /checkpoints — disk usage

```text
/checkpoints
```

Prints the number of checkpoints for the current project, the shadow-store
disk usage for this project and across all projects under
`~/.spettro/history/`, and the store path. To reclaim checkpoint history
across projects (including orphaned entries for moved/deleted projects), use
[`/storage clean` or `spettro clean`](storage.md).

### Requirements

- **git** must be installed and on `$PATH`. If `git` is not found,
  checkpointing is silently disabled (no snapshots, no `/rewind`).
- The shadow repo is created on first use (lazy init). Failure is non-fatal:
  the session continues without checkpointing.

## /rewind — restoring a checkpoint

```text
/rewind
```

Opens a checkpoint picker dialog showing every snapshot taken during the
session, ordered oldest first (newest at the bottom):

```
◈ rewind to checkpoint

  › 2026-01-15 14:30:23  3 file(s) edited  implement the auth middleware
    2026-01-15 14:28:10  1 file(s) edited  add user model
    2026-01-15 14:25:01  2 file(s) edited  initial scaffold
```

The file count on each row is what changed *after* that checkpoint (that
turn's edits, up to the next checkpoint — or up to the current working tree
for the newest row). In other words, it is exactly what rewinding to that row
would undo; edits you made before the checkpoint are captured inside it and
are restored, not deleted.

Navigation in the picker:

| Key | Action |
| --- | --- |
| `↑` / `↓` | Move selection |
| `PgUp` / `PgDn` | Page up/down through history |
| `Home` / `End` | Jump to first / last checkpoint |
| `Enter` | Select checkpoint and choose restore mode |
| `Esc` / `Ctrl+C` | Close the picker |

After pressing Enter on a checkpoint, you choose a restore mode:

| Mode | Effect |
| --- | --- |
| **restore conversation and files** | Both files and chat are rewound to that point |
| **restore files only** | Only the working tree is reset; conversation kept |
| **restore conversation only** | Only the chat transcript is restored; files as-is |

### Keyboard shortcut

You can also open the rewind picker by pressing **Esc twice** when the session
is idle (no agent run in progress).

### What happens on restore

**File restore** (`RestoreFiles`) resets the project working tree to the
checkpoint's commit: tracked files are restored to their state at that point,
and files created after the checkpoint are removed. Gitignored files are left
untouched.

**Conversation restore** replaces the chat transcript and the structured
conversation history (`convHistory`) with the snapshot stored at the
checkpoint. The LLM will see exactly the conversation as it was when the
snapshot was taken.

## The checkpoint data directory

```
~/.spettro/history/
└── <project-hash>/
    ├── repo.git/           # bare shadow git repository
    ├── conv/               # conversation snapshots (one per checkpoint)
    │   ├── <commit-hash>.json
    │   └── ...
    └── checkpoints.json    # index of all checkpoints
```

The project hash is the first 8 hex digits of SHA-256(`<project-path>`), so
the same project always maps to the same history directory across sessions.

## Edge cases

- **No checkpoints yet** — `/rewind` shows a message: "no checkpoints yet —
  they are taken before each file-modifying tool". No dialog opens.
- **Checkpointing disabled** — `/rewind` shows a warning banner:
  "checkpointing unavailable (is git installed?)". The session continues
  normally without snapshots.
- **Corrupt conversation snapshot** — restore shows an error and does not
  touch the current session.
- **No conversation stored** — some early checkpoints may lack a conversation
  blob; restore warns and proceeds with files only.
- **Large histories** — the picker pages up/down. Checkpoints are indexed in
  `checkpoints.json` so loading is fast even with hundreds of entries.

## Limitations

- Checkpoints are taken **per-session**: the shadow repo is additive across
  sessions, but `/rewind` only lists checkpoints from the current and previous
  sessions in the same project.
- Conversation snapshots are stored as plain JSON in `~/.spettro/history/`.
  They are not encrypted; if you need privacy, consider encrypting the
  directory yourself.
- Retention is time/size based (`checkpoint_retention_days`,
  `checkpoint_max_gb`), not per-checkpoint: you cannot pin an individual
  checkpoint beyond the horizon. You can still delete everything manually by
  removing `~/.spettro/history/`.
- Restores of old checkpoints can fail if the project repo's objects were
  pruned (see the alternates caveat above).