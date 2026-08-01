# Troubleshooting

## "Setup required" or provider errors

- Run `/connect` and verify your API key.
- Run `/models` and ensure selected model exists for that provider.
- If using local endpoint, verify it responds to `/v1/models`.

## "No providers connected"

- Use `/connect` and choose a provider.
- For LM Studio/Ollama/llama.cpp/vLLM, choose `Local endpoints (LM Studio/Ollama/llama.cpp/…)` and enter the endpoint (for example `localhost:1234`). You can add several endpoints; when prompted for an API key, press enter to skip it unless the server requires one.
- Then use `/models` and select a model.

## Token budget exceeded or context blocked

- Increase budget with `/budget <n>` or disable limit with `/budget 0`.
- Run `/compact` (or `/compact <focus...>`) to reduce active context size.
- Check `/compact policy` for thresholds and failure counters.
- Toggle auto-compaction with `/compact auto on|off`.

## Hooks not running as expected

- Run `/hooks` to inspect merged global+project rules and warnings.
- Verify event names are exactly: `PreToolUse`, `PostToolUse`, `PermissionRequest`, `SessionStart`.
- Confirm matcher patterns target the tool IDs you expect (`bash`, `shell-exec`, etc.).
- Hook commands must exit with code `0` to be treated as successful.

## `/approve` does nothing useful

- Ensure a plan was generated first.
- Ensure there is a pending plan.
- Check current permission with `/permission` or `/permissions`.

## Unknown agent / delegation failures

- Verify agent IDs exist in `spettro.agents.toml`.
- Check `handoffs` and `allowed_tools` references.
- Fix manifest validation issues and restart.

## Manifest validation error

- Fix unknown TOML fields, duplicate IDs, invalid permission values, or missing references.
- See `AGENTS.md` for schema/validation.
- Remove or rename broken `spettro.agents.toml` to use built-ins.

## Search/suggestions feel slow in large repos

- Mention suggestions and repository scans may slow down in very large trees.
- Narrow prompts and use focused file mentions (`@path/to/file`).

## Approval prompts lack detail

- Enable diagnostics with `/permissions debug on`.
- Use `/permissions` to inspect active rules and recent decisions.

## Reset local state

If needed, remove local Spettro state:

```bash
rm -rf .spettro
rm -rf ~/.spettro/config.json ~/.spettro/keys.enc ~/.spettro/trusted.json ~/.spettro/sessions
```

Only do this if you are comfortable losing local session history, trusted paths, and encrypted keys.
