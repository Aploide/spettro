# Custom Slash Commands

Save reusable prompts as files and run them as your own slash commands. A
custom command expands into a full prompt that is sent to the active agent —
exactly as if you had typed it — so it works with queuing, plan/coding modes,
session history, and prompt caching like any other prompt.

## Where command files live

Spettro scans two directories at startup:

| Location | Scope |
| --- | --- |
| `~/.spettro/commands/` | Global — available in every project. |
| `<project root>/.spettro/commands/` | Project — available in this project only. |

On a name conflict the **project definition wins**. Subdirectories create
namespaced names: `.spettro/commands/git/pr.toml` becomes `/git:pr`.

Command files are discovered when Spettro starts; restart after adding or
editing files.

## File formats

Both TOML and markdown are supported; pick whichever reads better for the
prompt you're writing.

### TOML (`.toml`)

```toml
# .spettro/commands/review.toml  →  /review
description = "review a file for correctness and style"
prompt = """
Review {{args}} for correctness bugs first, then style.
Report findings ordered by severity, with file:line references.
"""
```

- `prompt` (required) — the text sent to the agent.
- `description` (optional) — shown in `/help` and the autocomplete menu.

### Markdown (`.md`)

The optional YAML frontmatter supplies the `description`; the body is the
prompt.

```markdown
---
description: open a pull request for the current branch
---
Look at the commits on the current branch relative to main, then open a
pull request titled after the overall change: {{args}}
```

Saved as `.spettro/commands/git/pr.md`, this runs as `/git:pr`.

## Placeholders

### `{{args}}`

Every occurrence of `{{args}}` is replaced with whatever you type after the
command name:

```
/review internal/tui/model.go
```

expands `Review {{args}} for correctness…` into
`Review internal/tui/model.go for correctness…`. If you pass no arguments,
`{{args}}` is replaced with an empty string.

### Shell interpolation: `` !`command` ``

A prompt may embed live shell output with `` !`command` ``. Each interpolation
runs via `sh -c` in the project directory (15-second timeout) and its trimmed
output is spliced into the prompt before it is sent:

```toml
# .spettro/commands/changelog.toml  →  /changelog
description = "draft a changelog entry from recent commits"
prompt = """
Recent commits:
!`git log --oneline -15`

Draft a concise changelog entry covering these changes. {{args}}
"""
```

**Permission gating:** shell interpolation only executes under the `yolo`
permission level (`/permission yolo`). Under `restricted` or `ask-first` the
command refuses to run with an explanatory error instead of silently dropping
the shell output. Commands without `` !`…` `` work at every permission level.

If an interpolated command fails (non-zero exit), the custom command aborts
and shows the command's output — nothing is sent to the agent.

## Running commands

- Type `/` to open the autocomplete; custom commands are listed alongside
  built-ins with their description (or `custom command (project)` /
  `custom command (user)` when no description is set).
- `/help` appends a "custom commands" section listing everything discovered.
- Names are matched case-insensitively; built-in commands always take
  precedence, so a custom file named `help.toml` cannot shadow `/help`.

## More examples

Fix a GitHub issue by number — `/fix-issue 42`:

```toml
# ~/.spettro/commands/fix-issue.toml
description = "fetch a GitHub issue and fix it"
prompt = """
Issue details:
!`gh issue view {{args}}`

Fix this issue. Locate the relevant code, implement the fix, and add a
regression test.
"""
```

Explain the current diff — `/explain-diff`:

```markdown
---
description: explain the working-tree diff
---
Here is my current working-tree diff:

!`git diff`

Explain what these changes do and point out anything risky.
```

Project-specific test helper — `/test-pkg internal/tui`:

```toml
# <project>/.spettro/commands/test-pkg.toml
description = "run and fix tests for a package"
prompt = "Run `go test ./{{args}}/`. If anything fails, diagnose and fix it, then re-run until green."
```

## Troubleshooting

- **Command doesn't appear** — check the file extension (`.toml` or `.md`
  only), then restart Spettro. Files that fail to parse (e.g. a TOML file
  missing `prompt`, or an `.md` with an empty body) are skipped.
- **"requires yolo permission"** — the prompt contains `` !`…` ``; either
  switch with `/permission yolo` or remove the interpolation.
- **Same name in both scopes** — the project file wins; the global one is
  ignored for that project.
