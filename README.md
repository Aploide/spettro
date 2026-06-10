# Spettro

[![Go 1.26+](https://img.shields.io/badge/Go-1.26%2B-00ADD8?logo=go)](https://go.dev/)
[![UI Bubble Tea](https://img.shields.io/badge/UI-Bubble%20Tea-ff69b4)](https://github.com/charmbracelet/bubbletea)
[![Providers](https://img.shields.io/badge/LLM-OpenAI%20Compatible%20%7C%20Anthropic-6f42c1)](#provider-setup)
[![Status](https://img.shields.io/badge/status-experimental-orange)](#)
[![License](https://img.shields.io/badge/License-GPL--3.0-green)](LICENSE)
[![Ask DeepWiki](https://deepwiki.com/badge.svg)](https://deepwiki.com/cesp99/spettro)

CA: C3fawupKrXdn3X7KPTmmnkwzJQ9qycMNag55ZzQ8pump

Spettro is a terminal-first multi-agent coding assistant written in Go.

# Quick install

```bash
curl -sSfL https://raw.githubusercontent.com/cesp99/spettro/main/install.sh | sh
```

It uses a configurable agent manifest (`spettro.agents.toml` + `agents/*.md` prompts), parallel sub-agent spawning via `TOOL_CALL` and an `agent` tool, plus specialized orchestrator/worker roles (plan, coding, ask, explore, code, git, test, review, docs).

## Highlights

- Configurable multi-agent system via `spettro.agents.toml` and `agents/*.md`
- Parallel `TOOL_CALL` spawning of sub-agents
- Permission policies: `ask-first`, `restricted`, `yolo`
- Live tool traces in planning/coding runs
- Fantasy-backed provider routing for OpenAI, Anthropic, and OpenAI-compatible text calls
- Multi-provider model support via `models.dev` catalog + OpenAI-compatible endpoints
- Anthropic [extended-thinking levels](docs/thinking.md) (`/thinking off|low|medium|high|x-high`)
- Conversation persistence and resume per project
- Project trust prompt before first use in a folder

## Build and run

Requirements:

- Go `1.26.1+`

```bash
git clone https://github.com/cesp99/spettro
cd spettro
make build
./bin/spettro
```

Alternative:

```bash
go run ./cmd/spettro
```

## First-time setup

At first launch:

1. Confirm folder trust.
2. Run `/connect` to add an API key (or local endpoint).
3. Run `/models` to select provider/model.
4. Start with `plan` (default agent) and switch with `Shift+Tab`.

## Common commands

Spettro commands are entered with a leading `/`.

- `/help` show help text
- `/exit`, `/quit` quit Spettro
- `/mode`, `/next` cycle active agent/mode
- `/connect` connect provider or local endpoint
- `/models [provider:model] [api_key]` open selector or set directly
- `/permission <ask-first|restricted|yolo>` set execution policy
- `/permissions [ask-first|restricted|yolo]` show/set permission policy
- `/permissions debug <on|off>` toggle permission diagnostics
- `/budget <n|0>` set request token budget (`0` = unlimited)
- `/thinking <off|low|medium|high|x-high>` set extended-thinking budget (Anthropic Claude Opus/Sonnet; ignored by providers without thinking)
- `/plan [prompt]` switch to plan mode or run plan prompt
- `/approve` execute pending approved plan through coding agent
- `/tasks [list|add|done|set|show]` manage session tasks
- `/mcp <list|read|auth>` manage MCP resources and auth tokens
- `/skill <list|install|info|uninstall|enable|disable|where>` manage Agent Skills (Claude Code / OpenAI / Anthropic format)
- `/hooks` show effective runtime hooks
- `/compact [focus]` summarize conversation history
- `/compact auto <status|on|off>` configure auto-compact
- `/compact policy` show compact thresholds/counters
- `/clear` auto-save and clear current conversation
- `/resume` load a previous saved conversation
- `/init` analyze the repo and create/update `SPETTRO.md`
- `/remote [:port]` expose a loopback HTTP/SSE control plane (see [`docs/remote.md`](docs/remote.md))
- `/remote local [:port]` expose the control plane to LAN devices on `0.0.0.0` (see [`docs/remote.md`](docs/remote.md))
- `/telegram setup <token>` configure a Telegram bot relay so you can drive Spettro from a chat (see [`docs/telegram.md`](docs/telegram.md)). Alias: `/tg`.

For full commands and keybindings, see [`docs/commands.md`](docs/commands.md).

## Project docs

- [Agent Manifest](AGENTS.md)
- [Agent Prompts](agents/README.md)
- [Agent Skills](docs/skills.md)
- [Getting started and workflow](docs/getting-started.md)
- [Commands and keybindings](docs/commands.md)
- [Configuration and storage](docs/configuration.md)
- [Architecture overview](docs/architecture.md)
- [Remote control plane](docs/remote.md)
- [Telegram relay](docs/telegram.md)
- [Extended thinking levels](docs/thinking.md)
- [Troubleshooting](docs/troubleshooting.md)
- [Documentation Index](docs/README.md)

## Development

```bash
make test
make build
make build-all
```
