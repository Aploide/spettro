# Configuration and Storage

Spettro uses both project-local and user-global storage.

## Global (`~/.spettro/`)

| Path | Purpose |
| --- | --- |
| `config.json` | Active provider/model, permission, token budget, auto-compact, favorites, UI state, local endpoints, [thinking level](thinking.md), [theme](theme.md). |
| `keys.enc` | Encrypted API keys map by provider ID (see [Encrypted API keys](#encrypted-api-keys)). |
| `keys.enc.v1` | Backup of a pre-v2 `keys.enc`, written when it is migrated; kept for one release. |
| `master.key` | Random secret `keys.enc` is encrypted under (created on first use). |
| `trusted.json` | Permanently trusted project paths. |
| `catalog.json` | Cached provider/model catalog (see [Model catalog](#model-catalog)). |
| `catalog-meta.json` | When the server last confirmed `catalog.json`, and the server's `ETag`/`Last-Modified` for it. |
| `update-check.json` | Result of the last GitHub release check, reused for 24 hours. |
| `hooks.json` | Global runtime hooks fallback/default. |
| `lsp.json` | Optional [LSP](lsp.md) overrides; servers are auto-detected on PATH with zero config. |
| `memory.md` | [Persistent memory](memory.md): user-scope facts loaded into agent context each session. |
| `memory-inbox.json` | Drafted memory candidates awaiting `/memory review` approval (never loaded into context). |
| `commands/` | Global [custom slash commands](custom-commands.md) (`.toml` / `.md` prompt files). |
| `skills/` | User [Agent Skills](skills.md) (one folder with a `SKILL.md` per skill). |
| `history/<project-hash>/` | [Checkpointing](checkpointing.md) shadow git repo and conversation snapshots (auto-created; reclaimable via [`/storage clean`](storage.md)). |
| `sessions/<session-id>/` | Session metadata, messages, tasks/todos, and agent events. |
| `conversations/<project-slug>/` | Legacy conversation storage path kept for compatibility tooling. |

## Encrypted API keys

`keys.enc` holds the provider API keys, encrypted with AES-256-GCM. The file
key is derived from the random secret in `master.key` (or from
`SPETTRO_MASTER_KEY` when that variable is set) and a per-file salt.

- **Format v2** (`"kdf": "hkdf-sha256-v2"` in the file) derives the key with
  HKDF-SHA256, which takes microseconds. Earlier versions used scrypt, which
  added about 60 ms to every start of the TUI, ACP and headless modes. The
  security does not change: the input is 32 random bytes, which a slow KDF
  cannot strengthen, and `master.key` sits next to `keys.enc`.
- **Migration.** The first start of a v2-capable build rewrites an older
  `keys.enc` in format v2 (temp file, fsync, rename, so a crash leaves the
  old or the new file, never a partial one). Before replacing it, the old
  file is copied byte for byte to `keys.enc.v1`.
- **Downgrading.** Older builds cannot read a v2 file. To go back, restore
  the backup: `cp ~/.spettro/keys.enc.v1 ~/.spettro/keys.enc`. Keys added
  after the upgrade are not in the backup and must be entered again.
- **Removal timeline.** `keys.enc.v1` is kept for one release: the release
  that introduces format v2 creates it, and the release after that stops
  creating it and deletes an existing copy. Reading older files stays
  supported, so an installation that skips a release still migrates.
- **`SPETTRO_MASTER_KEY`.** A passphrase set in this variable is not
  random, so files encrypted under it keep scrypt and are not migrated.

## Model catalog

The model picker is built from the Spettro provider catalog
(`catalog.spettro.app`). Startup never waits for it:

- A cached copy in `~/.spettro/catalog.json` is used when present.
- Otherwise (first run, cache deleted, offline or behind a broken proxy) the
  snapshot embedded in the binary at build time is used.
- A background refresh then asks the server for a newer catalog only when
  the server last confirmed the cached copy more than 6 hours ago. The
  request is conditional: it carries the `ETag` and `Last-Modified` the server
  sent with that copy (kept in `catalog-meta.json`), so an unchanged catalog
  is not downloaded again. A cache without that file (written by an older
  build, or left behind after `spettro clean`) is downloaded again once
  stale.
- Long sessions re-check hourly under the same rule. The cache is shared by
  every spettro process: when another one (an ACP server, a second TUI) has
  downloaded a newer catalog, a running session switches to it at its next
  hourly check, without a request of its own.

Local endpoints (`local_endpoints` in `config.json`) and the Spettro
Subscription model list are also fetched in the background. Their models
appear in the TUI as each server answers. A result that arrives after you
removed or re-probed that endpoint (or signed out of the subscription) is
dropped, so it never undoes the change. If the configured model cannot run
(no key for its provider) and only a local endpoint can supply a
replacement, the TUI waits up to 2 seconds for the endpoints before it picks
and saves one. ACP `session/new`,
`session/load` and `session/resume` wait up to 2 seconds for them; a slower
server's models reach the editor afterwards as a `config_option_update`.
The headless server waits the same way before its first submission.

## Project-local (`<repo>/.spettro/`)

| Path | Purpose |
| --- | --- |
| `PLAN.md` | Last generated implementation plan. |
| `allowed_commands.json` | Commands approved with “allow always” for this project. |
| `hooks.json` | Project runtime hooks (overrides global by `(event, matcher, id)`). |
| `lsp.json` | Optional project [LSP](lsp.md) overrides (wins over the global file per server key). |
| `memory.md` | [Persistent memory](memory.md): project-scope facts loaded into agent context each session. |
| `commands/` | Project [custom slash commands](custom-commands.md); override global commands on name conflict. |
| `skills/` | Project [Agent Skills](skills.md); win over user skills of the same name. |
| `index.json` | Optional project snapshot when indexer-style flow is used. |

## Project root

| Path | Purpose |
| --- | --- |
| `spettro.agents.toml` | Project-specific agent manifest (fallback is built-in default). |

## Security model

- API keys are not stored in plaintext in `config.json`.
- Keys are encrypted with AES-GCM using a derived machine/user secret.
- Override key derivation input with `SPETTRO_MASTER_KEY`.
- First run in a folder requires explicit trust confirmation.

## Permission levels

| Level | Behavior |
| --- | --- |
| `ask-first` | Strictest flow; approval-first execution model. |
| `restricted` | Allows execution with policy checks and approval gating where required. |
| `yolo` | Least restrictive execution policy. |

## Theme

The TUI palette. See [Themes](theme.md) for detection details and what the
light palette re-tunes.

| `config.json` key | Default | Meaning |
| --- | --- | --- |
| `theme` | `""` (treated as `auto`) | `dark`, `light` or `auto`. Written by `/theme`; an unrecognised value is cleared to the default on load. |

Precedence at startup is `SPETTRO_THEME` (environment, never persisted) >
`theme` in `config.json` > auto-detection > dark. `auto` seeds from `COLORFGBG`
before the first frame, then asks the terminal itself with an OSC 11 background
query and revises the palette when the answer arrives; it degrades to dark
whenever the background cannot be determined — including on a terminal that
cannot be asked at all, where `COLORFGBG` is an inherited value describing
whatever launched Spettro rather than Spettro's own terminal.

`SPETTRO_THEME` is a startup override, not a lock: `/theme` still repaints and
still saves while it is set, and the variable takes the palette back on the next
start.

```bash
SPETTRO_THEME=light spettro     # override for one run
```

## Notifications

When the terminal is unfocused (or a run took more than 10 s), Spettro alerts
you on run/goal completion, on errors, and when the agent is waiting for a
command approval or an answer. Alerts go out on two channels at once: an OSC 9
terminal escape sequence — rendered as a system notification by iTerm2,
WezTerm, Ghostty, and Kitty; degraded to a terminal bell (BEL) elsewhere — and
a best-effort desktop notification (`notify-send` on Linux, `osascript` on
macOS).

| `config.json` key | Default | Meaning |
| --- | --- | --- |
| `notifications_disabled` | `false` | Set `true` to turn all notifications off. |
| `notify_quiet_sec` | `5` | Minimum seconds between notifications; events inside the window are dropped so bursts don't spam. |

## Agent Skills

See [Agent Skills](skills.md) for how skills are found and used.

| `config.json` key | Default | Meaning |
| --- | --- | --- |
| `skills_compat_disabled` | `false` | Set `true` to read skills only from `.spettro/skills` and `~/.spettro/skills`, ignoring the Claude Code and Codex folders (`.claude/skills`, `.agents/skills`, `.codex/skills`, `.openai/skills`). |
| `disabled_skills` | `[]` | Skill names hidden from the agent and the `/` menu; edited by `/skill disable` and `/skill enable`. |

## Checkpointing storage

Shadow-git snapshot storage for `/rewind`; see [Checkpointing](checkpointing.md)
for how each key behaves.

| `config.json` key | Default | Meaning |
| --- | --- | --- |
| `checkpointing_disabled` | `false` | Set `true` to turn checkpointing (and `/rewind`) off entirely. |
| `checkpoint_max_file_mb` | `20` | Files larger than this are excluded from snapshots (recorded and surfaced on rewind). |
| `checkpoint_retention_days` | `14` | Checkpoints older than this are pruned when the shadow repo is opened. |
| `checkpoint_max_gb` | `5` | If the shadow store still exceeds this after retention, the oldest half of the remaining checkpoints is dropped. |
| `checkpoint_warn_gb` | `2` | One-time warning threshold for projects without their own `.git` (where snapshots must copy the tree). |

## Storage cleanup

Session policy for `/storage clean` and `spettro clean`; see
[Storage](storage.md) for the full artifact inventory.

| `config.json` key | Default | Meaning |
| --- | --- | --- |
| `clean_session_age_days` | `30` | Sessions not updated within this many days become clean candidates. |
| `clean_keep_sessions` | `5` | The most recent K sessions per project always survive cleanup, regardless of age. |

### Shell command approvals

- The `bash` tool runs commands via `bash -lc` (PowerShell on Windows; see [Windows](windows.md)).
- Some safe commands are always allowed without a prompt: `ls`, `pwd`, `cat`,
  `head`, `tail`, `wc`, `grep`, `rg`, `stat`, `git status`, `git diff`,
  `go test`/`build`/`vet` and `make test`/`build`. The arguments are checked
  too: a flag that runs another program or writes a file (`rg --pre`,
  `git diff --output`/`--ext-diff`, `go test -exec`/`-o`/`-coverprofile`,
  `make test SHELL=...`, a leading `GIT_EXTERNAL_DIFF=...` or `GOFLAGS=...`)
  sends the command through the normal approval path.
- In non-`yolo` modes, non-default commands require approval.
- Choosing "allow always" stores normalized command approvals in `.spettro/allowed_commands.json`.

### Web access (web-search / web-fetch / download)

- `web-search`, `web-fetch`, and `download` are built-in network tools; see [Web Tools](web-tools.md) for behavior, limits, and the HTML-to-markdown engine.
- In non-`yolo` modes each network target requires approval; "allow always" persists targets in `.spettro/allowed_network.json`.
- All three go through the SSRF-hardened HTTP client: only http/https, non-public IPs (loopback, private ranges, cloud metadata) blocked at dial time, max 5 redirects.
- `download` additionally honors file-write approval and OS sandbox write roots; it never leaves partial files.

### Commit co-authoring (mandatory)

- Every commit Spettro produces — directly via the built-in committer or indirectly when an agent runs `git commit` through the `bash` tool — carries the trailer `Co-Authored-By: Spettro <spettro@eyed.to>`.
- The trailer is auto-injected by the runtime when missing. It is idempotent: if you (or the agent) already supplied the trailer, no second copy is added.
- Only the porcelain `git commit` is rewritten; plumbing such as `git commit-tree` is left untouched.

## Runtime hooks

- Hook files are JSON and can be configured globally (`~/.spettro/hooks.json`) and per-project (`.spettro/hooks.json`).
- Supported events: `PreToolUse`, `PostToolUse`, `PermissionRequest`, `SessionStart`.
- Merge precedence: global rules load first; project rules override by `(event, matcher, id)`.
- Hooks run via `bash -lc` and may emit a JSON decision line:
  - `{"decision":"allow"}`
  - `{"decision":"deny","reason":"..."}`
- `updated_args` is honored for `PreToolUse` shell tools.

## Agent manifest

Spettro loads `spettro.agents.toml` from the project root if present; otherwise it falls back to built-ins.

See [`AGENTS.md`](../AGENTS.md) for full schema and validation. The
snippet below shows the root and `[runtime]` fields only; a complete manifest
also lists its `[[tools]]` and `[[agents]]`. Write the current `version`
(14): an older one is migrated, and the file rewritten with a `.bak`, on the
first load.

```toml
version = 14
default_agent = "plan"

[runtime]
default_permission = "ask-first"
default_timeout_sec = 120
sandbox_mode = "full-access"      # off | read-only | workspace-write | full-access
log_tool_calls = true
# sandbox_net = "none"                    # all | localhost | none | ports:443,8080
# sandbox_allow_dirs = ["/data"]          # extra writable roots inside the sandbox
# sandbox_allow_read_dirs = ["/opt/sdk"]  # extra readable roots (e.g. toolchain caches)
```

## OS sandbox

Spettro can confine the agent at the kernel level — Seatbelt (`sandbox-exec`) on
macOS, Landlock on Linux. It is **opt-in** and off by default; it complements
(never replaces) the approval gates. The boundary is **invisible to the model**:
there is no sandbox tool and no prompt advertisement, blocked operations look
like ordinary failures, and the policy is set only by the operator (CLI flag or
`spettro.agents.toml`) — the model cannot inspect it or request its way out.

Activation precedence: CLI flags > `spettro.agents.toml` > off.

```sh
spettro --sandbox workspace-write                  # writes -> workspace + temp; reads confined
spettro --sandbox read-only --sandbox-net none     # no project/user writes, no network
spettro --sandbox-net ports:443                    # TCP confined to port 443 (any host)
spettro --sandbox workspace-write --sandbox-allow-dir /data        # extra writable root (repeatable)
spettro --sandbox read-only --sandbox-allow-read-dir ~/go/pkg/mod  # extra readable root (repeatable)
```

What is guarded:

- **Writes** — confined identically for shell commands (kernel) and the
  `file-write`/`file-edit` tools (in-process check), so `read-only` is truly
  read-only and cannot be bypassed by writing through a file tool. Writable set:
  temp dirs, any `--sandbox-allow-dir` roots, and — in `workspace-write` only —
  the workspace.
- **Reads** — system paths stay readable so programs run, but the **home tree**
  (other projects, `~/.ssh`, credentials, `~/.spettro` keys) is blocked except
  the workspace and any allowed roots. Toolchain caches that live in `$HOME`
  (e.g. `~/go/pkg/mod`, `~/.cache`) are blocked too, so add them with
  `--sandbox-allow-read-dir` when building under the sandbox.
- **Network** — `none` denies it, `localhost` allows loopback only,
  `ports:443,8080` allows those TCP ports (any host). The spettro process keeps
  network access for the LLM API, so the agent always reaches its model even
  under `net=none`.
- **The spettro process itself** — write-confined as defense-in-depth (Landlock
  in-process on Linux; a one-time `sandbox-exec` re-exec on macOS). Its reads
  stay open so the in-process git committer and skill discovery keep working;
  the model's own read surface is confined at the shell and file-tool layers.

Platform caveats:

- Linux network confinement needs Landlock ABI v4 (kernel 6.7+) and governs TCP
  only (UDP/unix sockets pass); `localhost` degrades to deny-all TCP because
  Landlock cannot scope rules to loopback. If the kernel cannot enforce a
  requested policy, sandboxed commands fail closed (exit 126).
- macOS network filters are ip/port based (no hostname allowlists). Under
  `none`, DNS and unix sockets are blocked too.
- macOS parent confinement uses a `sandbox-exec` re-exec rather than the
  deprecated in-process `sandbox_init`, to keep the `CGO_ENABLED=0` cross-
  compiled release builds working.
