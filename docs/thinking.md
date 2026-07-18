# Extended thinking

Spettro exposes a normalized **thinking level** that controls how much
extra reasoning compute the active model spends before answering. The
setting is independent of the active provider — the same `/thinking`
command works regardless of who serves the model — and each provider
maps it to its own native parameter: a thinking **token budget** for
Anthropic, a **`reasoning_effort`** value for OpenAI and every
OpenAI-compatible backend. Models that don't reason simply ignore it.

## Levels

| Level | Anthropic budget tokens | OpenAI-style `reasoning_effort` |
| --- | --- | --- |
| `off` (default) | 0 — sent as `thinking.type=disabled` so behaviour is explicit | parameter omitted (provider default; most reasoning models cannot fully disable reasoning) |
| `low` | ~2 048 | `low` |
| `medium` | ~5 120 | `medium` |
| `high` | ~16 384 | `high` |
| `x-high` | ~32 768 | `xhigh` |
| `max` | ~100 000 | `xhigh` (effort is an enum; `xhigh` is its highest value) |

The exact mappings live in `internal/provider/provider.go` as
`ThinkingBudgetTokens` and `ReasoningEffort` so they stay consistent
across adapters and are easy to test.

## Provider support

| Provider | Native parameter | Notes |
| --- | --- | --- |
| **Anthropic** (`claude-opus-4*`, `claude-sonnet-4-5`, etc.) | `thinking.type=enabled` + `thinking.budget_tokens` | Spettro automatically bumps `max_tokens` so it stays > the budget (Anthropic rejects requests where they meet or overlap). |
| **OpenAI** (`o3`, `o4-mini`, `gpt-5*`, …) | `reasoning.effort` (Responses API) / `reasoning_effort` (chat) | Only forwarded to models OpenAI marks as reasoning-capable, so setting a level does not break non-reasoning models like `gpt-4o`. |
| **OpenAI-compatible** (Groq, xAI, DeepSeek, Google's compat endpoint, Spettro Subscription, local Ollama/LM Studio, …) | `reasoning_effort` on chat completions | Sent whenever a non-off level is set; servers that don't know the parameter ignore it. |

Whether a model reasons at all comes from the catalog's `reasoning` flag
(`"reasoning": true` on the model entry), and the Spettro Subscription's
`/v1/models` response carries the same flag. Models without it never see a
thinking parameter, and in the TUI the `/thinking` command, its completion
menu, and the status-bar tag are hidden while such a model is active. The
ACP Thinking selector is the exception: it is always present (showing
`Off` when disabled) so the editor toolbar never loses the control.

## Automatic fallback

If a model rejects the requested level (an effort value it doesn't define,
or a thinking budget above its cap), Spettro does not surface the error —
it silently retries one level lower (`max` → `x-high` → `high` → `medium`
→ `low` → off) until the request goes through, so the run keeps its
continuity. On the `reasoning_effort` wire format, levels that serialize to
the same value are skipped (e.g. `max` and `x-high` are both `xhigh`, so
`max` falls straight to `high`). The persisted setting is untouched: only
the failing request is downgraded.

## Slash commands

```text
/thinking                 # report the current level
/thinking high            # switch to high-budget thinking
/thinking off             # disable extended thinking
```

`/thinking` is an *instant* command: it persists immediately and works
even while an agent run is in flight. The setting is stored in
`config.json` under `thinking_level`.

When extended thinking is enabled, the status bar shows a
`thinking:<level>` tag next to the active model so you can always tell
at a glance whether you're paying for thinking compute.

## Caveats

- Anthropic disallows non-default `temperature`, `top_p`, and `top_k`
  alongside thinking. Spettro doesn't currently set those, so this isn't
  a concern in practice — but anyone forking the Anthropic adapter
  should keep this in mind.
- Sub-agents inherit their parent's thinking level (so `agent` tool
  calls don't quietly downgrade to `off`).
