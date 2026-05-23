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
| `sessions/<session-id>/` | Session metadata, messages, tasks/todos, and agent events. |
| `conversations/<project-slug>/` | Legacy conversation storage path kept for compatibility tooling. |

## Project-local (`<repo>/.spettro/`)

| Path | Purpose |
| --- | --- |
| `PLAN.md` | Last generated implementation plan. |
| `allowed_commands.json` | Commands approved with “allow always” for this project. |
| `hooks.json` | Project runtime hooks (overrides global by `(event, matcher, id)`). |
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

### Commit co-authoring (mandatory)

- Every commit Spettro produces — directly via the built-in committer or indirectly when an agent runs `git commit` through `shell-exec`/`bash` — carries the trailer `Co-Authored-By: Spettro <spettro@eyed.to>`.
- The trailer is auto-injected by the runtime when missing. It is idempotent: if you (or the agent) already supplied the trailer, no second copy is added.
- Only the porcelain `git commit` is rewritten; plumbing such as `git commit-tree` is left untouched.

### Media generation (xAI Grok Imagine)

- `grok-image` and `grok-video` are built-in tools that call `https://api.x.ai/v1/images/generations` and `https://api.x.ai/v1/videos/generations` respectively.
- Both look up the xAI key from the encrypted store (`x-ai`/`xai`) or `$XAI_API_KEY`; configure it once via `/connect x-ai` or by exporting the env var.
- Outputs are written into the workspace. When no `path` is given, Spettro picks `public/` for Next.js projects and `assets/` everywhere else, slugging the prompt for the filename.
- These tools are listed in `coding`/`code` agents by default; add them to other agents in `spettro.agents.toml` if you want broader access.

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
version = 2
default_agent = "plan"

[runtime]
default_permission = "ask-first"
default_timeout_sec = 120
sandbox_mode = "workspace-write"
log_tool_calls = true
```
