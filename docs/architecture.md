# Architecture Overview

Spettro is a Go application with a Bubble Tea TUI front-end and internal service packages.

## Entry point and runtime

- `cmd/spettro/main.go` initializes config, encrypted keys, provider manager, model catalog, manifest validation, and TUI.
- `internal/tui` is the active runtime: command dispatch, dialogs, rendering, approval flows, and agent execution.
- Project manifest loading is handled by `internal/config` (`LoadAgentManifestForProject`).

## Core packages

- `internal/tui`: interactive terminal UI, command handling, approvals, and session interactions.
- `internal/agent`: LLM runtime loop, `TOOL_CALL` parsing/execution, delegation, policy checks.
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
2. Agent emits `TOOL_CALL` lines (parallel-capable via multiple lines).
3. Runtime executes allowed tools per manifest and permission policy.
4. Plans can be queued and executed via `/approve` through `coding`.
5. Outputs, tool traces, and session events are appended to timeline/session storage.

## Provider abstraction

- Text requests route through Charm's `fantasy` SDK for `anthropic`, `openai`, and OpenAI-compatible providers.
- Image requests and legacy completion-only backends fall back to Spettro's direct SDK adapters so existing compatibility is preserved.
- Known provider base URLs and local endpoints still resolve through the same manager layer.
- Catalog-backed model lists are preferred; fallback models are used when catalog is unavailable.
