# Devin integration

Spettro integrates with [Cognition's Devin](https://devin.ai) agent in
two complementary ways. Pick the one that matches how you want to work.

## Two modes

### 1. `devin-session` tool — delegation (recommended)

From inside any Spettro run — planner, coding, ask — the active agent
can call a `devin-session` tool to delegate a **self-contained subtask**
to a fresh Cognition Devin session. Your own conversation, model
(Claude Opus with thinking, or whatever you picked), tool history, and
todos all stay put; the Devin call just shows up as another tool result
in the log.

Typical trigger: the model decides a task is too big for its local
tool loop (wide multi-file refactor, long-running migration,
PR-producing work, sandboxed execution). It emits:

```text
TOOL_CALL {"name":"devin-session","arguments":{
  "task":"refactor the auth module across the 12 files using the new Session type",
  "constraints":"do not touch internal/admin/*, keep go fmt clean",
  "expected_output":"diff summary and a PR url"
}}
```

Spettro's runtime:

1. Reads your saved `api_keys.devin` and, for `cog_*` keys, your
   `devin_org_id`.
2. POSTs to the right Devin Sessions endpoint (v1 `/sessions` for
   `apk_*` keys, v3 `/v3/organizations/{org_id}/sessions` for `cog_*`).
3. Polls the session every ~5s until it reaches a terminal status
   (`finished`, `expired`, `blocked` for v1; `exit`, `error`,
   `suspended`, or `running+finished` for v3). Max wait: 30 minutes.
4. Returns Devin's final agent message (plus an `app.devin.ai` session
   URL footer) as the tool output. The calling agent continues from
   there; your session history, `ThinkingLevel`, and model choice are
   unaffected.

`/interrupt` on the Spettro side cancels the polling goroutine; the
Devin session itself keeps running on Cognition's infrastructure and
has to be cancelled from the Devin dashboard.

Available out of the box to the `plan`, `coding`, and `ask`
orchestrator agents in the default manifest — but only when an
`api_keys.devin` credential is actually configured. If you haven't run
`/connect` (and `/devin <org-id>` for `cog_*` keys), Spettro silently
strips `devin-session` from those agents' allowed-tools list for the
whole run, so the LLM never even sees the tool in its system-prompt
schema and can't be tempted to call something it can't use. As soon as
you add the key the tool reappears on the next turn with no restart
needed. Flip it off explicitly per-project by removing
`"devin-session"` from the agent's `allowed_tools` list in
`spettro.agents.toml`.

### 2. `devin` provider — Devin IS the model

If you want every Spettro prompt to run as a Devin session end-to-end
(no local planner / coder, no Anthropic key, Cognition owns model
selection and billing), pick Devin as the model:

```text
/connect                   # save your cog_* (v3) or apk_* (v1) key as devin
/devin org-abc123def456    # only needed for cog_ keys
/models devin:session
```

This is the right mode when you don't want local tools and just want
Devin to handle the full task. Spettro essentially becomes a thin UI
around `app.devin.ai` in this mode: the `/thinking` slash command is
accepted but ignored, the token counter reads 0 (Devin bills in ACUs),
and every prompt creates a fresh session.

## Authentication

Both modes use the same key storage. Spettro auto-detects the API
generation based on the prefix of your key:

| API generation | Key prefix | Endpoint base | Org id needed |
| --- | --- | --- | --- |
| **v3** (Service Users + RBAC, current) | `cog_*` | `POST /v3/organizations/{org_id}/sessions` | yes |
| **v1** (Personal/Service API keys, legacy) | `apk_*` | `POST /v1/sessions` | no |

The full key is stored encrypted in `~/.spettro/keys.enc`; only the
`cog_` / `apk_` prefix is visible in logs. The org id is stored in
plaintext in `config.json` under `devin_org_id` (it is not a secret).

## Why delegation is the better default

- Your Claude/Opus/Sonnet session keeps its thinking budget, tool
  history, and ongoing conversation intact while Devin handles one
  subtask.
- You see the delegation as a single tool trace in the log; you can
  cite it, follow up on it, or ignore it exactly like any other tool
  result.
- ACU consumption on Cognition's side is scoped to the delegated
  subtask, not the whole conversation.
- If Devin fails or times out, the calling agent gets the error back
  and can retry, work around it, or surface it to you.

## Constraints / gotchas

- Devin sessions can take many minutes; the tool call blocks the
  Spettro tool loop until the session finishes or you `/interrupt`.
- The Spettro runtime has a per-step `max_tool_calls_per_step` cap
  (default 32). A single `devin-session` call counts as one tool call
  against that cap — so you'll never accidentally spawn 30 parallel
  Devin sessions.
- Devin sessions don't accept image attachments via the session
  create endpoint; vision inputs stay on providers that support them.
- Devin's session URL is included in the tool output so you can inspect
  the run, cancel it, or share it.

## Related commands

- `/connect` — save or replace the Devin API key.
- `/devin <org-id>` — show or set the v3 organization id.
- `/models devin:session` — switch Spettro to Devin-as-provider mode.
- `/thinking <level>` — affects Anthropic/etc.; ignored by both Devin paths.
