# Runtime Hooks

Hooks let you run custom bash scripts at key points during an agent run.
They are a lightweight middleware system: a hook receives event data via
stdin and can **allow**, **deny**, or **modify** what happens next.

Think of hooks as programmable guardrails. They complement the built-in
permission policy by letting you enforce project-specific rules that the
manifest schema cannot express — for example: "block `rm -rf`" or "always
append a copyright header on Go files".

## Supported events

| Event | When it fires | What you can do |
|-------|---------------|-----------------|
| `PreToolUse` | Before a tool call is executed. | Allow, deny, or modify arguments. |
| `PostToolUse` | After a tool call completes. | Allow, deny (cannot modify output). |
| `PermissionRequest` | When a permission prompt is about to be shown. | Allow, deny, or modify the request. |
| `SessionStart` | When a new TUI session begins. | Informational only — no decision. |

## Hook file format

Hooks are configured in a JSON file. The file can be an object with a `hooks`
key, or a bare JSON array of rules:

```json
{
  "hooks": [
    {
      "id": "deny-rm-rf",
      "event": "PreToolUse",
      "matcher": "shell-exec",
      "command": "if echo '$SPETTRO_HOOK_COMMAND' | grep -q 'rm -rf'; then echo '{\"decision\":\"deny\",\"reason\":\"rm -rf is not allowed\"}'; else echo '{\"decision\":\"allow\"}'; fi",
      "timeout_sec": 5
    }
  ]
}
```

Equivalent array form:

```json
[
  {
    "id": "deny-rm-rf",
    "event": "PreToolUse",
    "matcher": "shell-exec",
    "command": "if echo '$SPETTRO_HOOK_COMMAND' | grep -q 'rm -rf'; then echo '{\"decision\":\"deny\",\"reason\":\"rm -rf is not allowed\"}'; else echo '{\"decision\":\"allow\"}'; fi",
    "timeout_sec": 5
  }
]
```

### Rule fields

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `id` | string | No | `global-1`, `project-1`, ... | Unique identifier for the rule. Auto-generated when empty. |
| `event` | string | Yes | — | One of `PreToolUse`, `PostToolUse`, `PermissionRequest`, `SessionStart`. |
| `matcher` | string | No | `*` (all tools) | Glob pattern or `re:regex` to match tool IDs (`shell-exec`, `bash`, `file-write`, `file-edit`, `agent`, ...). |
| `command` | string | Yes | — | Shell command to execute. Receives event data on stdin. |
| `timeout_sec` | int | No | `15` | Maximum execution time for the command. |
| `enabled` | bool | No | `true` | Set to `false` to disable a rule without deleting it. |

### Matcher syntax

| Pattern | Matches |
|---------|---------|
| `*` or `""` | Every tool. |
| `shell-exec` | Exactly the `shell-exec` tool. |
| `bash` | Exactly the `bash` tool. |
| `file-*` | Glob: `file-write`, `file-edit`, etc. |
| `re:^git` | Regex: any tool ID starting with `git`. |
| `re:(shell-exec\|bash)` | Regex: either shell tool. |

### What the hook receives on stdin

The hook command receives a JSON object on stdin:

```json
{
  "event": "PreToolUse",
  "tool_id": "shell-exec",
  "tool_args": {"command": "rm -rf /tmp/x"},
  "tool_output": "",
  "command": "rm -rf /tmp/x"
}
```

Fields:

| Field | Type | Description |
|-------|------|-------------|
| `event` | string | The event that triggered the hook. |
| `tool_id` | string | The tool ID (`shell-exec`, `file-write`, etc.). |
| `tool_args` | object | The arguments the tool was called with (varies by tool). |
| `tool_output` | string | The output of the tool call (only meaningful for `PostToolUse`). |
| `command` | string | Shortcut to the command string when the tool is a shell executor. |

Environment variables are also set:

| Variable | Value |
|----------|-------|
| `SPETTRO_HOOK_EVENT` | The event name (`PreToolUse`, `PostToolUse`, ...). |
| `SPETTRO_HOOK_TOOL_ID` | The tool ID. |
| `SPETTRO_HOOK_COMMAND` | The shell command being executed (when applicable). |

### Hook output / decision format

The hook must print its decision as a JSON object on **the last non-empty line
of stdout**:

```json
{"decision":"allow"}
```

```json
{"decision":"deny","reason":"rm -rf is dangerous"}
```

```json
{"decision":"deny","reason":"use --soft flag instead","message":"Please use --soft when deleting"}
```

For `PreToolUse` shell-exec tools, you can also modify the arguments:

```json
{"decision":"allow","updated_args":"pip install requests --no-cache-dir"}
```

Decision values:

| Decision | Effect |
|----------|--------|
| `allow` | Proceed normally. |
| `deny` | Block the operation. The `reason` is shown to the user. |
| (anything else, or no decision line) | Treated as `allow`. |

The `message` field (when present) is displayed as a banner or log entry
but does not affect the decision.

If the hook exits with a non-zero exit code, the decision defaults to `allow`
but the error is logged.

## File locations

Hooks are loaded from two locations, merged by `(event, matcher, id)`:

| Scope | Path | Precedence |
|-------|------|------------|
| Global | `~/.spettro/hooks.json` | Loaded first. |
| Project | `<cwd>/.spettro/hooks.json` | Overrides global by `(event, matcher, id)`. |

Project rules do NOT inherit from global — they replace the matching
`(event, matcher, id)` tuple entirely.

## Viewing active hooks

```text
/hooks
```

Prints the merged effective configuration with source annotations and any
validation issues:

```
Effective hooks (2 global, 1 project):
  global  PreToolUse    deny-rm-rf      shell-exec     ✓
  global  PostToolUse   log-edits       file-write     ✓
  project PreToolUse    custom-approve  shell-exec     ✗ unsupported event "foo"
```

## Writing a hook

### Example: block dangerous commands

File: `~/.spettro/hooks.json`

```json
{
  "hooks": [
    {
      "id": "block-force-push",
      "event": "PreToolUse",
      "matcher": "re:(shell-exec|bash)",
      "command": "if echo \"$SPETTRO_HOOK_COMMAND\" | grep -qP 'git\s+push\s+.*--force'; then echo '{\"decision\":\"deny\",\"reason\":\"force push is not allowed\"}'; else echo '{\"decision\":\"allow\"}'; fi",
      "timeout_sec": 3
    }
  ]
}
```

### Example: log every file edit

File: `.spettro/hooks.json` (project-scoped)

```json
{
  "hooks": [
    {
      "id": "log-file-edit",
      "event": "PostToolUse",
      "matcher": "file-write",
      "command": "read input; echo \"$input\" | jq -r '.tool_args.path' >> /tmp/spettro-edits.log; echo '{\"decision\":\"allow\"}'"
    }
  ]
}
```

### Example: modify shell commands on the fly

```json
{
  "hooks": [
    {
      "id": "add-cache-dir",
      "event": "PreToolUse",
      "matcher": "shell-exec",
      "command": "read input; cmd=$(echo \"$input\" | jq -r '.command // empty'); if echo \"$cmd\" | grep -q 'pip install'; then echo '{\"decision\":\"allow\",\"updated_args\":\"'"$cmd"' --no-cache-dir\"}'; else echo '{\"decision\":\"allow\"}'; fi",
      "timeout_sec": 3
    }
  ]
}
```

## Performance notes

- Hooks run synchronously on the tool-call execution path. Every hook adds
  latency equal to the time your `command` takes to execute.
- Keep hook commands fast (parsing JSON or grepping a string, not installing
  packages).
- The default timeout is 15 seconds; set `timeout_sec` lower for simple
  decisions and higher for hooks that need to make network calls.
- A slow or hanging hook blocks the agent run. Use `timeout_sec` generously
  and test your hooks before deploying them.

## Security notes

- Hook commands run as the same user as Spettro, with the same privileges.
- They are **not** confined by the OS sandbox, even when `--sandbox` is active.
- Do not read untrusted input into `eval` or dynamic `source` inside hooks.
- Hooks in project `.spettro/hooks.json` are checked into version control —
  review them as part of your code review.