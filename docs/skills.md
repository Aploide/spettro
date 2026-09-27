# Agent Skills

A skill is a folder of instructions (and, optionally, scripts and reference
files) that teaches the agent how to do one kind of task: fill a PDF form,
cut a release, follow a team's review checklist. Spettro implements the
[Agent Skills][spec] format, the same `SKILL.md` format Claude Code and
OpenAI Codex use, so a skill written for either works in Spettro unchanged
and Spettro finds the skills you already installed for them.

[spec]: https://agentskills.io/specification

There are two ways a skill gets used:

- **The agent loads it by itself.** Every run, the system prompt carries a
  short list of the available skills (name and one-line description). When
  a request matches one, the agent calls the `skill` tool to load its full
  instructions, then follows them.
- **You run it.** Type `/<skill-name> [arguments]` (it is in the `/` menu),
  or mention it anywhere in a prompt as `$<skill-name>`.

## Quick start

Create a folder with a `SKILL.md` in it:

```bash
mkdir -p .spettro/skills/changelog
cat > .spettro/skills/changelog/SKILL.md <<'EOF'
---
name: changelog
description: Write a CHANGELOG entry for the current branch. Use when the user asks for release notes or a changelog.
argument-hint: "[version]"
---

# Changelog entry

1. Run `git log --oneline main..HEAD` to see what changed.
2. Group the commits into Added / Changed / Fixed.
3. Prepend a section headed "## $ARGUMENTS" to CHANGELOG.md.
EOF
```

Then, in Spettro:

```
/skills               # the new skill is listed, with its path
/changelog 1.4.0      # run it; $ARGUMENTS becomes "1.4.0"
write the notes for this branch using $changelog
```

and in any conversation where you ask for release notes, the agent loads it
on its own. Skills you add or edit by hand are picked up on the next
message, in the TUI and in editors alike (see [Caching](#caching)).

## The SKILL.md format

```
pdf-processing/
├── SKILL.md           instructions + frontmatter (required)
├── scripts/           code the instructions tell the agent to run
│   └── extract.py
├── references/        long documents read only when needed
│   └── FORMS.md
└── assets/            templates, images, other files
    └── template.docx
```

`SKILL.md` is Markdown with YAML frontmatter between `---` lines:

```yaml
---
name: pdf-processing
description: Extract PDF text, fill PDF forms, merge PDFs. Use when handling PDF documents.
---
```

| Field | Default | Meaning |
| --- | --- | --- |
| `name` | the folder name | What the skill is called: `/name`, `$name`, and the name the agent loads it by. Lowercase letters, digits and hyphens per the spec (other names still load, with a warning in `/skills`). A name with spaces could never be typed as a command, so the folder name is used instead (or, if that has spaces too, the name with its spaces turned into hyphens), with a warning. |
| `description` | the first line of the body | What the skill does and when to use it. This is all the agent sees before loading the skill, so put the trigger ("Use when ...") in it. |
| `when_to_use` | none | Extra trigger text, appended to the description in the agent's list (Claude Code). |
| `argument-hint` | none | Shown in the `/` menu and to ACP clients, e.g. `[issue-number]`. |
| `arguments` | none | Names for positional arguments, so the body can say `$component` instead of `$0`. A list (`[component, from-lang]`) or a space-separated string. |
| `disable-model-invocation` | `false` | `true`: only you can run it (`/name`, `$name`). The agent is not told about it and the `skill` tool refuses it. For skills with side effects: deploy, release, send. |
| `user-invocable` | `true` | `false`: only the agent can load it; it is hidden from the `/` menu and `$` completion. For background knowledge. |
| `disabled` / `enabled` | enabled | `disabled: true` (or `enabled: false`) hides the skill from everyone. |
| `license`, `compatibility`, `metadata` | none | Spec fields, shown by `/skill info`. |
| `allowed-tools` | none | Recorded and shown by `/skill info`, but **not enforced**: which tools an agent may use is decided by the [agent manifest](../AGENTS.md), and a skill cannot widen it. |

Anything else in the frontmatter (Claude Code's `model`, `context`, `hooks`,
Codex's extras, ...) is kept as metadata and otherwise ignored, so such
skills still load. The parser is deliberately forgiving: a file with no
frontmatter at all loads with its folder name and first line, and lists may
be written inline (`[a, b]`) or as `- item` lines. A value may go on over
more-indented lines, or start on the line after its key; the lines are
joined with spaces, as YAML folds them:

```yaml
description: Extract text from PDFs and fill forms.
  Use when the user mentions PDFs.
```

Only a skill with neither a description nor any body text is rejected (it
is reported under warnings in `/skills`).

Skill folders often come from repositories you cloned, so Spettro treats
them as untrusted input: terminal escape sequences and control characters
are stripped from every frontmatter value (the `/` menu and the `$` list
draw names and descriptions straight onto the terminal), and a `SKILL.md`
that is not a regular file (a pipe, a link to a device) or is larger than
1 MiB is skipped with a warning instead of being read.

### Arguments and variables in the body

When a skill runs, these placeholders in its body are replaced. The rules
match Claude Code's, so its skills behave the same:

| Placeholder | Becomes |
| --- | --- |
| `$ARGUMENTS` | Everything typed after `/name` (empty when nothing was). |
| `$ARGUMENTS[N]`, `$N` | The N-th argument, counting from 0. Quotes group words: `/greet "Ada Lovelace"` makes `$0` = `Ada Lovelace`. A position past the last argument is left as written. |
| `$<name>` | The argument the `arguments` field names at that position; empty when fewer were given. A `$word` that is not a declared name (`$HOME`) is left alone. |
| `${CLAUDE_SKILL_DIR}`, `${SKILL_DIR}`, `${SPETTRO_SKILL_DIR}` | The skill's folder, so scripts can be referenced wherever the skill is installed. |

If you pass arguments but the body has no argument placeholder, the body gets
a final `ARGUMENTS: <what you typed>` line, so the agent still sees them.
The agent can pass arguments too (the `skill` tool's `args`), with the same
substitution.

## Where skills are found

Spettro reads skills from its own folders and, unless you turn it off, from
the folders Claude Code and Codex use. It only ever writes (install,
uninstall) to its own folders; the others are read-only to it.

| Family | Project path | User path | Written by Spettro |
| --- | --- | --- | --- |
| Spettro | `.spettro/skills/` | `~/.spettro/skills/` | yes |
| Cross-client / Codex | `.agents/skills/` | `~/.agents/skills/` | no |
| Claude Code | `.claude/skills/` | `~/.claude/skills/` | no |
| Codex (legacy) | `.codex/skills/` | `$CODEX_HOME/skills/`, else `~/.codex/skills/` | no |
| OpenAI (older configs) | `.openai/skills/` | `~/.openai/skills/` | no |

"Project path" is looked up in the working directory **and each parent up
to the repository root** (the first folder with a `.git`), so a skill checked
in at the root of a monorepo is found from any package. Outside a
repository only the working directory counts.

Each skill is a folder directly inside one of these paths
(`.claude/skills/<skill>/SKILL.md`). The two Codex families, `.agents/skills`
and `.codex/skills` (with `$CODEX_HOME/skills`), may also group skills in
subfolders, as Codex allows: `~/.codex/skills/<group>/<skill>/SKILL.md` is
found too, up to six folders deep. Hidden folders are not searched, and
neither are a skill's own subfolders.

`/skill where` prints the exact list for the current directory, in priority
order, marking which folders exist.

### Precedence and name collisions

When two skills have the same name (case-insensitively), the first one in
this order wins:

1. Project before user.
2. Within the project, the folder nearest the working directory first.
3. Within one folder, the families in table order: Spettro, `.agents`,
   `.claude`, `.codex`, `.openai`.

The losers are listed under "shadowed" in `/skills`, so a clash is never
silent. Note that Claude Code resolves the other way round (your personal
skill beats the project's); Spettro lets the repository's copy win because
it is the one the team reviewed for that code base.

Skills also share the `/` namespace with commands. **Built-in commands
always win**, then [custom commands](custom-commands.md), then skills: a skill
called `help` or `review` (when you also have a `review` custom command) is
not offered in the `/` menu and does not run as `/help`; the agent can
still load it, and `$help` still mentions it.

### Turning the other folders off

Set this in `~/.spettro/config.json` to read only `.spettro/skills`:

```json
{ "skills_compat_disabled": true }
```

The change applies to the next run; no reload needed.

## Using skills

### Running a skill yourself

- **`/<name> [args]`** in the TUI, over ACP (editors list skills in their
  command menu, with the argument hint), and through the headless remote.
  The transcript shows what you typed; the agent receives the skill's
  instructions with your arguments substituted, and starts working.
- **`$<name>`** anywhere in a prompt (Codex style). Typing `$` opens a
  completion list of the skills you can run; on send, the instructions of
  each mentioned skill (up to five) are appended to your prompt. `$5`,
  `$HOME` and other words that are not skill names are left alone.

### The agent loading a skill

The system prompt lists every skill the agent may load, one line each:

```
<available_skills>
- changelog: Write a CHANGELOG entry for the current branch. Use when ...
- pdf-processing: Extract PDF text, fill PDF forms, merge PDFs. ...
</available_skills>
```

Descriptions are cut to 250 characters and the whole list to 8,000
characters; past that the list ends with a note telling the agent to call
`skill` with no name to see the rest. Skills with
`disable-model-invocation: true` or disabled ones are not listed.

Only agents that hold the `skill` tool get the list. An agent without it
(a custom agent whose `allowed_tools` leave it out) could not load a skill,
and `file-read` is no substitute: it is confined to the workspace, and most
skills live under your home directory. If your manifest has a tool of its
own called `skill`, the old `skill-read` built-in stands in for it and the
list names `skill-read` instead (see [tools](tools.md#retired-names)).

The agent then calls the `skill` tool:

| Call | Result |
| --- | --- |
| `{"name": "changelog"}` | The body wrapped in `<skill_content name="changelog">`, the skill's folder, and its bundled files (`scripts/`, `references/`, `assets/`, at most 50 listed). The agent reads those files with `file-read` only when the instructions point to them. |
| `{"name": "changelog", "args": "1.4.0"}` | The same, with the arguments substituted. |
| `{}` or `{"query": "pdf"}` | A JSON list of the skills it may load (optionally filtered). |

`skill-read`, `skill-list`, `activate-skill` and `skill-activate`, and
Claude Code's `{"skill": "<name>"}` spelling, still work as hidden aliases
(see [tools](tools.md#retired-names)).

## Commands

| Command | What it does |
| --- | --- |
| `/skills` | List every skill: who can run it (`/name`, `/name only`, `agent only`, `disabled`), its family and scope, and its `SKILL.md` path; then shadowed skills and warnings. |
| `/<name> [args]` | Run a skill. |
| `/skill install <source> [--project] [--force] [--as=<name>] [--path=<sub>]` | Install from a local folder, an https git URL, or `owner/repo`. Goes to `~/.spettro/skills/<name>`, or `.spettro/skills/<name>` with `--project`. |
| `/skill uninstall <name> [--project]` | Remove a skill installed in `.spettro/skills`. Skills in the Claude Code / Codex folders are never deleted by Spettro; disable them instead. |
| `/skill info <name>` | Metadata, bundled files, warnings and the start of the instructions. |
| `/skill disable <name>` / `enable <name>` | Hide or show a skill everywhere. Stored in `~/.spettro/config.json` as `disabled_skills`, so nothing is written into the skill's folder. `enable` also deletes a `.spettro-disabled` file an older Spettro left in the skill's folder (in any family's folder: it is Spettro's own file). A skill whose `SKILL.md` says `disabled: true` stays off, and `enable` tells you to edit that file. |
| `/skill where` | The discovery folders for the current directory, in priority order. |
| `/skill reload` | Force a re-scan of the skill folders; changes on disk are otherwise picked up by themselves (see below). |

ACP clients get `/skills` and one command per runnable skill; installing
and managing skills is TUI-only.

```
/skill install ./local-skill-folder
/skill install https://github.com/anthropics/skills.git --path=skills/pdf
/skill install anthropics/skills --path=skills/pdf --project
```

## Caching

The discovered skills are kept in memory, so the skill list in the system
prompt is byte-identical on every request; providers cache the prompt
prefix, and a list that changed between requests would throw that cache
away. Before each use, Spettro checks the modification times of the skill
folders and of every `SKILL.md` it read (a handful of `stat` calls, no file
reads), and rescans only when something changed. So a skill you add, edit
or delete by hand shows up on the next message, in the TUI, in an ACP
editor and through the headless remote, without restarting anything, and
the prompt changes only when a skill really did. `/skill reload` forces a
rescan anyway. Changing `skills_compat_disabled` or `disabled_skills`
applies immediately.

## For maintainers

Where the pieces live:

| Concern | Code |
| --- | --- |
| Discovery, precedence, parsing | `internal/skills/skills.go`, `parser.go` |
| Prompt list, activation, argument substitution, `$` mentions | `internal/skills/prompt.go` |
| Cache, and its freshness check | `internal/skills/cache.go` (`skills.Shared`) |
| Settings to catalog (the one entry point every host uses) | `internal/agent/skills_catalog.go` (`SkillCatalogFor`, `ReloadSkills`) |
| The `skill` tool and its aliases | `internal/agent/llm_runtime_skills.go`, `tool_aliases.go`; manifest fold in `internal/config/tool_consolidation.go` (v14) |
| TUI: `/skill`, `/skills`, `/name`, `$name`, menu entries | `internal/tui/commands_skills.go`, `model_commands_catalog.go`, `input_mentions.go` |
| ACP: advertised commands, `/name`, `/skills`, `$name` | `internal/acp/skills.go`, `bridge.go` (Prompt) |
| Headless remote | `cmd/spettro/headless_skills.go` |

Invariants worth keeping:

- Every consumer gets its catalog from `agent.SkillCatalogFor` (or
  `SkillCatalog`). Discovering directly would bypass the user's settings
  and the cache, and the `/` menu could disagree with what the agent sees.
- `CatalogPrompt` must depend only on the catalog and the agent's load
  tool, never on time or step, to keep the system prompt cacheable. The
  load tool comes from `toolRuntime.skillLoadTool`; an agent without one
  gets no list.
- Spettro never writes into a root whose `Root.ReadOnly()` is true. The
  one exception is `/skill enable` deleting a legacy `.spettro-disabled`
  marker, which only an older Spettro could have put there.
- A skill never shadows a command: each host checks its own built-in names
  first (`builtinCommandNames` in the TUI, `acpReservedCommandNames` in ACP,
  `headlessCommandNames` for the remote). Adding a built-in command means
  adding it to that host's list if it is not already advertised.
- Legacy `.spettro-disabled` marker files are still honoured when present,
  but nothing creates them any more.
- The TUI's sub-menus (`/permission `, `/thinking `, `/think `, `/skill `)
  open only after the command and a space, so a skill whose name starts
  with a command name stays reachable from the main menu
  (`slashSubMenu` in `input_mentions.go`).
