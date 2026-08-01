# Getting Started

This guide focuses on the daily usage flow. For command details, see [`commands.md`](commands.md).

## 1) Launch Spettro

```bash
./bin/spettro
```

On first launch in a folder, Spettro shows a trust dialog:

- `Yes, trust this session`
- `Yes, and remember this folder`
- `No, exit`

## 2) Configure model access

Use:

- `/connect` to add provider API key or a local endpoint
- `/models` to select provider/model

Spettro supports:

- Native Anthropic API.
- OpenAI-compatible providers through provider-specific base URLs.
- Local OpenAI-compatible endpoints (for example LM Studio/Ollama).

Model metadata is loaded from `https://models.dev/api.json` and cached locally.

## 3) Work with the agent system

Spettro starts with `default_agent` from manifest (default: `plan`).

- Explore first with `glob`/`grep`/`file-read`.
- Spawn sub-agents in parallel via the native `agent` tool (multiple calls in one response run concurrently).
- Switch mode/agent with `Shift+Tab` or `/mode`.

## 4) Approval flow

If permission is `ask-first`:

1. Generate a plan.
2. Review and refine if needed.
3. Run `/approve` to execute through the `coding` agent.

## 5) Persist and resume conversations

- `/compact` summarizes long threads (optionally focused).
- `/clear` saves current conversation and clears the active thread.
- `/resume` loads a prior saved conversation for the same project.

## 6) Customize

Edit `spettro.agents.toml` and `agents/*.md` (for example `agents/docs-writer.md`), then restart Spettro.
