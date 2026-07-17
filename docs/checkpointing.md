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

  › 2026-01-15 14:30:23  3 file(s)  implement the auth middleware
    2026-01-15 14:28:10  1 file(s)  add user model
    2026-01-15 14:25:01  2 file(s)  initial scaffold
```

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
- The shadow repo is never automatically pruned. Old checkpoints accumulate.
  You can delete checkpoints manually by removing `~/.spettro/history/`.