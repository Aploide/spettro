# Architecture Overview

Spettro is a Go application with a Bubble Tea TUI front-end and internal service packages.

## Entry point and runtime

- `cmd/spettro/main.go` initializes config, encrypted keys, provider manager, model catalog, manifest validation, and TUI.
- `internal/tui` is the active runtime: command dispatch, dialogs, rendering, approval flows, and agent execution.
- Project manifest loading is handled by `internal/config` (`LoadAgentManifestForProject`).

## Core packages

- `internal/tui`: interactive terminal UI, command handling, approvals, and session interactions.
- `internal/agent`: LLM runtime loop, native tool-call execution, delegation, policy checks, and **tool output spooling** (large results from `file-read`, `grep`, `shell-exec`, `web-fetch` etc. are written to a session-scoped spool file with a truncated head and a pageable offset, so the model can retrieve the full content via `job-output` with `spool:N` IDs).
- `internal/config`: config persistence, encrypted keys, trust list, manifest parsing/validation/migration.
- `internal/provider`: provider adapters, endpoint resolution, connected model routing, and Fantasy-backed text model execution with legacy SDK fallback for vision or legacy completion endpoints.
- `internal/models`: fetch/cache of `models.dev` catalog.
- `internal/session`: persistent session storage (`messages`, `tasks`, `agents` events) and resume support.
- `internal/storage`: project/global `.spettro` directory setup.
- `internal/hooks`: global/project hook loading, merge, and execution.
- `internal/compact`: context usage policy and compaction guardrails.
- `internal/skills`: Agent Skills discovery, parsing, install/uninstall, and prompt rendering. Discovers `SKILL.md` packs from `<cwd>/.spettro|.agents|.claude|.openai/skills/` and `~/.spettro|.agents|.claude|.openai/skills/` so Claude Code and OpenAI skills work without conversion. See [skills.md](skills.md).

## Agent manifest

Spettro loads `spettro.agents.toml` from project root when present; otherwise it uses built-ins.

See [AGENTS.md](../AGENTS.md) for schema details (`version = 2`, `[runtime]`, `[[tools]]`, `[[agents]]`, permissions, validation).

## Execution flow

1. User prompt enters current active agent (`plan` by default).
2. Agent emits native tool calls via the provider API (parallel-capable via multiple calls per response).
3. Runtime executes allowed tools per manifest and permission policy.
4. Plans can be queued and executed via `/approve` through `coding`.
5. Outputs, tool traces, and session events are appended to timeline/session storage.

## Orchestration contract (orchestrators vs workers)

Spettro deliberately splits the agent roster into **orchestrators** (`plan`, `coding`, `ask`) and **workers** (`explore`, `code`, `git`, `test`, `review`, `docs`). The orchestration contract is:

- Orchestrators are coordinators. They decompose the user's request and spawn workers via the `agent` tool, preferring parallel batches (the runtime allows up to 4 concurrent sub-agents per step). Their prompts in `agents/planning.md`, `agents/coding.md`, and `agents/chat.md` enforce "delegate first".
- `plan` is enforced at the manifest level: it has **no** direct read tools (`glob`/`grep`/`file-read`/`ls`). Discovery must go through an `explore` worker. The corresponding contract tests live in `tests/config/manifest_test.go`.
- `coding` keeps its raw write/exec tools as an emergency escape hatch, but the prompt strongly discourages using them directly. The expected default path is `coding → {explore, docs}` (parallel) `→ code` (impl) `→ {test, review}` (parallel) `→ git`.
- Workers are individual contributors. `agents/code.md` is the dedicated `code` worker prompt; the orchestrator-style `agents/coding.md` is used only by the `coding` orchestrator. Workers do not re-delegate (and `code` is the only worker that has the `agent` tool, gated by handoffs).
- The runtime's `agent` dispatch already validates role + handoff compatibility (`isDelegationRoleAllowed` in `internal/agent/llm_runtime_shell.go`), so workers can't accidentally spawn an orchestrator.

## Provider abstraction

- Text requests route through Charm's `fantasy` SDK for `anthropic`, `openai`, and OpenAI-compatible providers.
- Image requests and legacy completion-only backends fall back to Spettro's direct SDK adapters so existing compatibility is preserved.
- Known provider base URLs and local endpoints still resolve through the same manager layer.
- Catalog-backed model lists are preferred; fallback models are used when catalog is unavailable.
