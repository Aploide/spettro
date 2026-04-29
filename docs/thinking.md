# Extended thinking

Spettro exposes a normalized **thinking level** that controls how much
extra reasoning compute the active model spends before answering. The
setting is independent of the active provider — the same `/thinking`
command works regardless of who serves the model — but each provider
maps it to its own native parameter, and providers that don't expose a
thinking control simply ignore it.

## Levels

| Level | Approx. budget tokens | Behaviour |
| --- | --- | --- |
| `off` (default) | 0 | No extended thinking. Sent as `thinking.type=disabled` for Anthropic so behaviour is explicit. |
| `low` | ~2 048 | Short reasoning budget. |
| `medium` | ~5 120 | Medium reasoning budget. |
| `high` | ~16 384 | Long reasoning budget. |
| `x-high` | ~32 768 | Maximum reasoning budget. |

The exact mapping lives in `internal/provider/provider.go` as
`ThinkingBudgetTokens` so it stays consistent across adapters and is easy
to test.

## Provider support

| Provider | Honours thinking? | Notes |
| --- | --- | --- |
| **Anthropic** (`claude-opus-4*`, `claude-sonnet-4-5`, etc.) | yes | Maps to `thinking.type=enabled` + `thinking.budget_tokens`. Spettro automatically bumps `max_tokens` so it stays > the budget (Anthropic rejects requests where they meet or overlap). |
| Other providers (OpenAI, Google, Groq, xAI, OpenAI-compatible local) | no | The field is silently dropped. |

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
