# Checkpointing and Rewind

Spettro automatically snapshots the project working tree before every agent
step that modifies files, so you can rewind files and/or conversation to any
earlier step — like a time machine for your coding session.

## How it works

Checkpointing is built on a **shadow git repository** stored in Spettro's data
directory, completely separate from the project's own `.git`. Before the
first `file-write`, `file-edit`, shell command or similar write
tool of each step (one model reply), Spettro:

1. Stages all changes in the project working tree.
2. Commits to the shadow repo with a label describing the pending tool call.
3. Saves a snapshot of the current conversation alongside it.

A step's mutating calls run one at a time in the order the model gave them,
so that single snapshot captures the tree as it was before any of them, and
rewinding to it undoes the whole step.

### Snapshots prepared while the model generates

Staging and committing the tree is the expensive part of a snapshot (several
git processes over the whole tree). Nothing is supposed to change the tree
while the model is generating a step, so once a run has taken its first
checkpoint, Spettro does that part for each later step as soon as the step's
request is sent, in the background. When the step's first mutating call
arrives, it only records the prepared snapshot as the checkpoint (a list
entry and the conversation blob, no git process), so the call no longer waits
for git. The checkpoint's content is the same as a synchronous snapshot's,
except for the limitation below.

A run's first mutating step always snapshots when the call arrives, as before:
a run that never edits anything (a question, a review) never stages or copies
the tree. The step that ends an editing run (the model's final answer) still
prepares a snapshot that no call claims; that is one discarded preparation per
editing run, done while the model writes its answer.

Spettro falls back to taking the snapshot when the call arrives:

- while a background shell job or a pty session is running (it can change
  files at any moment);
- when hooks are configured that run around tool calls (`PreToolUse`,
  `PostToolUse`, `PermissionRequest`): their commands can change files right
  before the call;
- when a tool that can write files without taking a checkpoint of its own
  (`download`, MCP and custom tools, a sub-agent) ran after the preparation
  started, earlier in the same step or in an earlier one;
- when the preparation started 30 seconds or more before the call (a long
  generation, or an approval or `ask-user` wait earlier in the step);
- when preparing failed, or something else was snapshotted or restored in the
  meantime (for example by a sub-agent working in the same checkout).

If a file the agent read or wrote changed after the preparation started (you
saved it in your editor, say), the tracked files are staged again
(`git add -u`) before the checkpoint is recorded, so your edit is in it. This
covers files the agent read after the preparation started too. The check is
one `stat` per such file (about 2.5 µs each, at most 1024 files; past that the
tracked files are always staged again).

A preparation that no mutating call claimed is kept for the next step when
that step ran only tools that cannot change the tree (reads, searches,
read-only shell commands) and it is less than 30 seconds old; otherwise the
next step prepares a new one and the unclaimed commit is unpinned. An
unclaimed commit left by an exit is unpinned by the next session that opens
the project's checkpoints once it is two minutes old. Younger ones are left
alone, because another Spettro session on the same project may still claim
its own.

**Limitation.** A file the agent never read or wrote that another program
creates or edits after the preparation started (at most 30 seconds before the
mutating call) is captured by the next checkpoint rather than this one.
Rewinding to this step's checkpoint then reverts that edit, or removes that new
file. Checkpointing never waits on a file-system monitor daemon (a deliberate
choice: no background daemons), so this window cannot be closed without
re-scanning the whole tree on every mutating call.

Shell commands that provably cannot write to the working tree take no
snapshot: `ls`, `cat`, `head`, `grep`/`rg`, `find` without `-delete`/`-exec`,
`sed -n '<range>p'`, `git status`/`diff`/`log`/`show`, `go vet`/`list`, and
pipelines of those. The classifier is deliberately narrow: anything else —
build and test runners (`go test` can write golden files), interpreters,
redirections into files, command substitution, background jobs — still
snapshots first. So does a command prefixed with an environment assignment
other than a locale or display setting (`GIT_EXTERNAL_DIFF=… git diff`,
`GOFLAGS=… go vet`), `go vet`/`list` with any `-mod`/`-modfile` flag (which
can rewrite `go.mod` and `go.sum`), and `git`/`go` when the inherited
environment sets `GIT_EXTERNAL_DIFF`, `GIT_CONFIG_COUNT`,
`GIT_CONFIG_PARAMETERS`, or a `GOFLAGS` (from the environment or `go env -w`)
with `-mod`, `-modfile`, `-toolexec` or `-vettool`.

Sub-agents running in their own git worktree (`isolation: "worktree"`) do not
snapshot the main checkout, since their edits land outside it; instead a
snapshot is taken right before their branch is merged back.

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
- **Few git processes per snapshot.** The shadow index persists between
  snapshots, so `add -A` only rehashes files whose stat data changed, and a
  single `diff-index` against the previous checkpoint decides whether
  anything changed, counts the changed files and finds files over the size
  cap. A changed tree then costs `write-tree`, `commit-tree` and one
  `update-ref --stdin`.
- **No-change fast path.** If the tree is identical to the previous
  checkpoint, no new commit is minted — the list entry points at the same
  commit and only the conversation snapshot is stored. If the conversation is
  identical too, no entry is added at all.
- **Conversation blobs are shared.** Every snapshot in one run records the
  same run-start conversation; it is written once and later checkpoints
  reference it, instead of one copy per snapshot. Retention only deletes a
  blob once no kept checkpoint references it.
- **Maintenance.** The shadow repo runs with `core.untrackedCache` and
  `index.version=4` to keep `add -A` fast on large trees, `git gc --auto`
  runs every 20 snapshots (at the start of the next prepared snapshot, while
  the model generates, or right after a snapshot taken when the call arrived,
  as before; a session opened after an exit that skipped it runs it then), and
  reflogs are disabled so pruned checkpoints can actually be collected.
- **Cached index file.** `checkpoints.json` is kept in memory between
  snapshots and re-read only when its size or modification time changes (for
  example, another Spettro session on the same project added a checkpoint).
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
    ├── checkpoints.json    # index of all checkpoints
    └── prepared/           # transient: one empty marker per prepared,
        └── <commit-hash>   #   unclaimed snapshot
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