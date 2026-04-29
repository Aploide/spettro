# Agent Skills

Spettro implements the [Agent Skills][spec] standard so you can install reusable
capability packs once and have every Spettro agent (planning, coding, exploring,
reviewing, etc.) pick them up automatically. Skills are also compatible with
Claude Code and other clients that use the same `SKILL.md` format.

[spec]: https://agentskills.io/specification

## What is a skill?

A skill is a directory containing a `SKILL.md` file with YAML frontmatter and
Markdown instructions, plus optional bundled `scripts/`, `references/`, and
`assets/` subdirectories:

```
pdf-processing/
├── SKILL.md
├── scripts/
│   └── extract.py
├── references/
│   └── REFERENCE.md
└── assets/
    └── template.docx
```

Minimum frontmatter:

```yaml
---
name: pdf-processing
description: Extract PDF text, fill PDF forms, merge PDFs. Use when handling PDF documents.
---
```

Optional fields are `license`, `compatibility`, `metadata` (key/value map), and
`allowed-tools`.

## Discovery roots

Spettro scans the following directories at every agent run, in priority order
(the first occurrence of a name wins, project-scope overrides user-scope):

| Scope   | Path                                |
| ------- | ----------------------------------- |
| project | `<cwd>/.spettro/skills/`            |
| project | `<cwd>/.agents/skills/`             |
| project | `<cwd>/.claude/skills/`             |
| project | `<cwd>/.openai/skills/`             |
| user    | `~/.spettro/skills/`                |
| user    | `~/.agents/skills/`                 |
| user    | `~/.claude/skills/`                 |
| user    | `~/.openai/skills/`                 |

Skills already installed for Claude Code under `~/.claude/skills/` or shared
`~/.agents/skills/` are discovered automatically without any extra setup. You
can see the live list with `/skill where`.

## Slash commands

| Command | Description |
| --- | --- |
| `/skill list` (or `/skills`) | Show installed skills + scope/source. |
| `/skill install <source> [--project] [--force] [--as=<name>] [--path=<sub>]` | Install from a local directory, https git URL, or `owner/repo`. |
| `/skill info <name>` | Show metadata, resources, and a body excerpt. |
| `/skill enable <name>` / `disable <name>` | Toggle a skill without uninstalling. |
| `/skill uninstall <name> [--project]` | Remove an installed skill. |
| `/skill where` | List every discovery root and whether it exists. |
| `/skill reload` | Re-scan skill directories. |

### Install sources

```
/skill install ./local-skill-folder
/skill install ~/Downloads/my-skill
/skill install https://github.com/anthropics/skills.git --path=skills/pdf
/skill install anthropics/skills --path=skills/pdf
```

The default destination is `~/.spettro/skills/<name>`. Use `--project` to
install into `<cwd>/.spettro/skills/<name>` instead so the skill ships with
the repository.

## How agents use skills

At every run, Spettro injects an `<available_skills>` block into the system
prompt with `name`, `description`, `location`, and bundled `resources` for each
enabled skill. Agents have two activation paths:

1. **Dedicated tool** (preferred): the `skill-read` builtin returns the skill
   body wrapped in `<skill_content name="...">` tags so it is identifiable
   during compaction. The model invokes it with
   `TOOL_CALL {"name":"skill-read","arguments":{"name":"<skill>"}}`.
2. **File read**: the model can also read `SKILL.md` directly via the standard
   `file-read` tool, using the absolute `location` from the catalog.

Bundled scripts and references are listed but **not** eagerly read; the model
loads them on demand when the skill instructions reference them.

## Authoring a skill

```bash
mkdir -p ~/.spettro/skills/my-skill
cat > ~/.spettro/skills/my-skill/SKILL.md <<'EOF'
---
name: my-skill
description: Short summary plus when to use it.
---

# My Skill

1. Step one
2. Step two
EOF
/skill list
```

Spettro applies lenient validation: it warns when `name` does not match the
parent directory or exceeds 64 characters, but still loads the skill so you
can iterate quickly.
