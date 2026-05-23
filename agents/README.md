# Spettro Agent Prompt Pack

This folder contains prompt files referenced by `spettro.agents.toml`.

The pack is split along the orchestrator vs worker contract:

- **Orchestrators** (`planning.md`, `coding.md`, `chat.md`) prefer delegation — they decompose work, spawn workers via the `agent` tool (preferring parallel batches), and synthesize the results.
- **Workers** (`code.md`, `explore.md`, `git.md`, `tester.md`, `reviewer.md`, `docs-writer.md`) execute a single focused slice end-to-end and return a tight summary.

## Included prompt files

- `planning.md` (orchestrator)
- `coding.md` (orchestrator)
- `chat.md` (orchestrator)
- `code.md` (worker — implementation)
- `explore.md` (worker — discovery, read-only)
- `git.md` (worker — git workflow)
- `reviewer.md` (worker — sanity checks)
- `tester.md` (worker — verification commands)
- `docs-writer.md` (worker — documentation)

## Default manifest agent IDs

- `plan` -> `planning.md`
- `coding` -> `coding.md`
- `ask` -> `chat.md`
- `explore` -> `explore.md`
- `code` -> `code.md` (dedicated worker prompt; the orchestrator and worker no longer share a file)
- `git` -> `git.md`
- `test` -> `tester.md`
- `review` -> `reviewer.md`
- `docs` -> `docs-writer.md`

## Usage

- Set `prompt_file = "agents/<name>.md"` in each `[[agents]]` block.
- Keep `system_prompt` empty when using `prompt_file` as source of truth.
- Prompts define mission/scope, tool contracts, execution protocol, output contract, and escalation/safety rules.
- The contract that `plan` cannot read files directly (no `glob`/`grep`/`file-read`) is enforced both in the manifest (allowed_tools) AND in `tests/config/manifest_test.go`; don't reintroduce read tools to `plan` without updating both.
