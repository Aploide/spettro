# Configuration and Storage

Spettro uses both project-local and user-global storage.

## Global (`~/.spettro/`)

| Path | Purpose |
| --- | --- |
| `config.json` | Active provider/model, permission, token budget, auto-compact, favorites, UI state, local endpoints, [thinking level](thinking.md). |
| `keys.enc` | Encrypted API keys map by provider ID. |
| `trusted.json` | Permanently trusted project paths. |
| `models.json` | Cached `models.dev` catalog. |
| `hooks.json` | Global runtime hooks fallback/default. |
| `lsp.json` | Optional [LSP](lsp.md) overrides; servers are auto-detected on PATH with zero config. |
| `memory.md` | [Persistent memory](memory.md): user-scope facts loaded into agent context each session. |
| `memory-inbox.json` | Drafted memory candidates awaiting `/memory review` approval (never loaded into context). |
| `commands/` | Global [custom slash commands](custom-commands.md) (`.toml` / `.md` prompt files). |
| `history/<project-hash>/` | [Checkpointing](checkpointing.md) shadow git repo and conversation snapshots (auto-created, never pruned). |
| `sessions/<session-id>/` | Session metadata, messages, tasks/todos, and agent events. |
| `conversations/<project-slug>/` | Legacy conversation storage path kept for compatibility tooling. |

## Project-local (`<repo>/.spettro/`)

| Path | Purpose |
| --- | --- |
| `PLAN.md` | Last generated implementation plan. |
| `allowed_commands.json` | Commands approved with “allow always” for this project. |
| `hooks.json` | Project runtime hooks (overrides global by `(event, matcher, id)`). |
| `lsp.json` | Optional project [LSP](lsp.md) overrides (wins over the global file per server key). |
| `memory.md` | [Persistent memory](memory.md): project-scope facts loaded into agent context each session. |
| `commands/` | Project [custom slash commands](custom-commands.md); override global commands on name conflict. |
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

### Shell command approvals

- Shell tools run via `bash -lc` (`shell-exec`/`bash`).
- Some safe read-only commands are always allowed.
- In non-`yolo` modes, non-default commands require approval.
- Choosing "allow always" stores normalized command approvals in `.spettro/allowed_commands.json`.

### Web access (web-search / web-fetch / download)

- `web-search`, `web-fetch`, and `download` are built-in network tools; see [Web Tools](web-tools.md) for behavior, limits, and the HTML-to-markdown engine.
- In non-`yolo` modes each network target requires approval; "allow always" persists targets in `.spettro/allowed_network.json`.
- All three go through the SSRF-hardened HTTP client: only http/https, non-public IPs (loopback, private ranges, cloud metadata) blocked at dial time, max 5 redirects.
- `download` additionally honors file-write approval and OS sandbox write roots; it never leaves partial files.

### Commit co-authoring (mandatory)

- Every commit Spettro produces — directly via the built-in committer or indirectly when an agent runs `git commit` through `shell-exec`/`bash` — carries the trailer `Co-Authored-By: Spettro <spettro@eyed.to>`.
- The trailer is auto-injected by the runtime when missing. It is idempotent: if you (or the agent) already supplied the trailer, no second copy is added.
- Only the porcelain `git commit` is rewritten; plumbing such as `git commit-tree` is left untouched.

### Media generation (xAI Grok Imagine)

- `grok-image` and `grok-video` are built-in tools that call `https://api.x.ai/v1/images/generations` and `https://api.x.ai/v1/videos/generations` respectively.
- Both look up the xAI key from the encrypted store (`x-ai`/`xai`) or `$XAI_API_KEY`; configure it once via `/connect x-ai` or by exporting the env var.
- Outputs are written into the workspace. When no `path` is given, Spettro picks `public/` for Next.js projects and `assets/` everywhere else, slugging the prompt for the filename.
- These tools are listed in `coding`/`code` agents by default; add them to other agents in `spettro.agents.toml` if you want broader access.
- When the Telegram relay is running and at least one chat is bound, every successful `grok-image` / `grok-video` call is also broadcast to those chats: images via `sendPhoto`, videos via `sendVideo`, falling back to `sendDocument` for files that exceed Telegram's inline-media caps (10 MB for photos, 50 MB for videos). The originating prompt becomes the Telegram caption (truncated). Upload errors surface through `/telegram status`.

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

See [`AGENTS.md`](../AGENTS.md) for full schema and validation.

```toml
version = 3
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
