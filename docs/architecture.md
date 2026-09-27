# Architecture Overview

Spettro is a Go application with a Bubble Tea TUI front-end and internal service packages.

## Entry point and runtime

- `cmd/spettro/main.go` initializes config, encrypted keys, provider manager, model catalog, manifest validation, and TUI.
- `internal/tui` is the active runtime: command dispatch, dialogs, rendering, approval flows, and agent execution.
- Project manifest loading is handled by `internal/config` (`LoadAgentManifestForProject`).

## Core packages

- `internal/tui`: interactive terminal UI, command handling, approvals, and session interactions. Every frame has to fit the terminal exactly: rows are fitted to their width before lipgloss sees them (a box that wraps a long row grows taller than the layout reserved), and `recalcLayout` measures the rendered input area instead of estimating it. The todo/agent footer is the one flexible block: `parallelFooterBudget` caps it by the rows the rest of the frame needs, so it is what shrinks on a short terminal. Tool labels are chosen by tool name; a name that belongs to a tool of the operator's own (`AgentManifest.UserTool`) gets the generic label instead of the built-in's (ACP cards follow the same rule). The counted wording of a group of calls ("Read 3 files", "Running 2 commands…") comes from one table, `toolWordings` in `tool_wording.go`, which a test keeps complete for every built-in and retired name; the file tools' labels and diffs accept `file_path` as well as `path`. `internal/tui/render_fit_test.go` renders huge tool calls, approvals and dialogs at sizes from 40x15 up and checks every frame; `scripts/tui-e2e/` drives the real binary in a pseudo-terminal against a fake OpenAI-compatible server and checks what reaches the screen (see its README).
- `internal/termtext`: makes untrusted text safe for the cell grid. `SanitizeLine` shows tool output the way a terminal would (escape sequences removed, tabs expanded, carriage returns resolved to the final state of the line); `EscapeControls` shows text the user must judge (a command waiting for approval, a diff) with every control character, Unicode format character (bidi controls, zero-width characters) and invalid byte visible instead; both replace text that is not valid UTF-8 before a stray byte can act as a C1 control; `Fit`, `FitLeft` and `Wrap` bound text to a number of cells, marking cuts with `…`. `SanitizeLine` also applies `StableWidth`, which rewrites grapheme clusters whose width terminals disagree on (emoji ZWJ sequences, the emoji variation selector, skin tones, keycaps, flags) into a form every terminal draws at the width the layout measured. Only emoji sequences are rewritten; text in other scripts is kept byte for byte.
- `internal/agent`: LLM runtime loop, native tool-call execution, delegation, policy checks, and **tool output spooling** (large results from `file-read`, `grep`, `bash`, `web-fetch` etc. are written to a session-scoped spool file with a truncated head and a pageable offset, so the model can retrieve the full content via `tool-output` with `spool:N` IDs).
- `internal/config`: config persistence, encrypted keys, trust list, manifest parsing/validation/migration.
- `internal/provider`: provider adapters, endpoint resolution, connected model routing, and Fantasy-backed text model execution with legacy SDK fallback for vision or legacy completion endpoints.
- `internal/models`: fetch/cache of `models.dev` catalog.
- `internal/session`: persistent session storage (`messages`, `tasks`, `agents` events) and resume support.
- `internal/storage`: project/global `.spettro` directory setup.
- `internal/hooks`: global/project hook loading, merge, and execution.
- `internal/compact`: context usage policy and compaction guardrails.
- `internal/workflow`: the [workflow](workflows.md) script engine — a goja JavaScript runtime with `agent`/`parallel`/`pipeline`/`phase`/`log`/`budget` globals, an event loop that resolves agent promises from goroutines, meta-header parsing, structured-output validation, and the journal that makes a run resumable. It knows nothing about Spettro's agents: sub-agent execution arrives through a `Runner` interface (implemented in `internal/agent/workflow.go`) and progress leaves through an `Observer`, so the engine is testable without a provider.
- `internal/skills`: Agent Skills discovery, parsing, install/uninstall, prompt rendering, argument substitution and `$name` mentions, plus the catalog cache, which rescans only when a skill folder or `SKILL.md` changed on disk. Discovers `SKILL.md` packs from `.spettro/skills/` (project, walking up to the repository root, and `~`) and, read-only, from the Claude Code and Codex folders (`.agents`, `.claude`, `.codex`, `.openai`) so their skills work without conversion. Hosts get the catalog through `agent.SkillCatalogFor`. See [skills.md](skills.md).
- `internal/testhome` (tests only): keeps test binaries off the real `~/.spettro` (config, keys, Telegram settings, `lsp.json`). A package whose tests can reach per-user files calls `os.Exit(testhome.Main(m))` from `TestMain`: HOME, USERPROFILE and the XDG/AppData directories point at a temporary directory and SUDO_USER is cleared, while GOENV, GOCACHE, GOPATH, GOMODCACHE and GOPLSCACHE stay on their real locations. Optional setup functions passed to `Main` prepare the temporary home before the tests run. The package carries a `TestHomeIsIsolated` test calling `testhome.Check`, which fails when that isolation is missing. A test that writes to the home (an `lsp.json`, a config) should still `t.Setenv("HOME", t.TempDir())`: the temporary home is shared by the whole test binary.

## TUI render pipeline

The transcript and the frame around it are built so that the work per frame
does not grow with the length of the session:

- **Render cache** (`render_cache.go`). Each chat message is rendered once into
  rows and cached by its id; a refresh reuses an entry while the message's
  fields equal the snapshot taken when it was rendered (string equality, a
  pointer comparison when nothing changed) and the layout it depends on (pane
  width, and the ctrl+o/ctrl+g toggles for messages with tool calls) still
  holds. A message keeps the accent colour of the mode it was written in, so a
  mode switch re-renders nothing. A message with a running pty tool is never
  cached (its live tail comes from the pty session).
- **Frame budget**. When many messages need rendering at once (a resize,
  ctrl+o, ctrl+g, a theme switch, `/resume`), a refresh renders for about
  12 ms, newest first, and leaves the older messages' previous rendering (or a
  one-row placeholder) in place; `renderFillMsg` continues on the following
  frames until the transcript is complete. The scroll height is approximate
  until then; a view following the bottom stays at the bottom.
- **Live drafts**. The streamed answer and thinking are rendered
  incrementally: completed lines (outside a code fence or table) are rendered
  once, and only the line being written is rendered per token. The final
  message replaces the draft at run end and is rendered whole.
- **Transcript viewport** (`lineview.go`). The viewport holds the rendered
  blocks as row slices with per-block offsets and draws only the rows on
  screen; content is wrapped to the pane width before it gets there. It
  follows the latest output only while it is at the bottom; submitting a
  prompt jumps to the bottom. Building with `-tags spettro_bubblesviewport`
  swaps in the previous `bubbles/viewport` for comparison (one release).
- **Run events** (`run_events.go`). Stream chunks and tool traces travel from
  the agent to the UI through one unbounded, ordered queue; the agent never
  blocks on rendering, and the UI applies whatever accumulated since the last
  frame as one batch (one refresh, one frame). The run's done message is sent
  only after the UI applied the run's last events.
- **Frame memo** (`frame.go`). The header, input box, status bar, side panel
  and the delegation/todo footer are kept between frames and re-rendered only
  after a message that can change them; a streamed token or an animation tick
  redraws the transcript rows and the working indicator only (a tick also
  redraws the chrome parts that animate, such as a glaring task). The frame
  is joined from rows measured once.
- **Idle**. The 50 ms animation tick runs only while something animates
  (a run, onboarding and sign-in spinners, the MAX plan label, a goal's clock,
  running delegations, in-progress tasks, glowing input keywords); a 1 s
  clock tick redraws the status bar while a `/loop` counts down or a
  background job or pty session runs (either can end with no message
  reaching the TUI); banners clear with a one-shot timer; the input cursor
  is steady unless `cursor_blink` is set. Otherwise an idle TUI does no work
  between keystrokes.
- **Off the Update goroutine**. The side panel's git state (`git status`,
  `git diff --numstat`), `/diff` (which also hands the modified files it
  lists to the side panel), a goal iteration's workspace fingerprints
  (taken in the run's command before and after the run), and the save of
  the mode and side panel toggle to `config.json` run as background
  commands. `TestNoGitOnTheUpdateGoroutine` counts git processes started by
  `tui.New` and by Update.
- **Side panel**. Only the rows in the visible window are styled, the list
  reads the activity feed in place instead of copying it, the frame is drawn
  by hand (byte for byte what the lipgloss border style drew, checked by
  `TestSidePanelBoxMatchesLipgloss`), and the activity feed keeps the newest
  2,000 items (the subtitle counts the rest).

## Agent manifest

Spettro loads `spettro.agents.toml` from project root when present; otherwise it uses built-ins.

See [AGENTS.md](../AGENTS.md) for schema details (`version = 14`, `[runtime]`, `[[tools]]`, `[[agents]]`, permissions, validation, and the migrations that bring an older manifest up to date).

## Execution flow

1. User prompt enters current active agent (`plan` by default).
2. Agent emits native tool calls via the provider API (parallel-capable via multiple calls per response).
3. Runtime executes allowed tools per manifest and permission policy.
4. Plans can be queued and executed via `/approve` through `coding`.
5. Outputs, tool traces, and session events are appended to timeline/session storage.

## Orchestration contract (orchestrators vs workers)

Spettro deliberately splits the agent roster into **orchestrators** (`plan`, `coding`, `ask`) and **workers** (`explore`, `code`, `git`, `test`, `review`, `docs`, `general-purpose`). The orchestration contract is:

- Orchestrators are coordinators. They decompose the user's request and spawn workers via the `agent` tool, preferring parallel batches (the runtime allows up to 4 concurrent sub-agents per step). Their prompts in `agents/planning.md`, `agents/coding.md`, and `agents/chat.md` enforce "delegate first".
- `plan` is enforced at the manifest level: it has **no** direct read tools (`glob`/`grep`/`file-read`). Discovery must go through an `explore` worker. The corresponding contract tests live in `tests/config/manifest_test.go`.
- `coding` keeps its raw write/exec tools as an emergency escape hatch, but the prompt strongly discourages using them directly. The expected default path is `coding → {explore, docs}` (parallel) `→ code` (impl) `→ {test, review}` (parallel) `→ git`.
- Workers are individual contributors. `agents/code.md` is the dedicated `code` worker prompt; the orchestrator-style `agents/coding.md` is used only by the `coding` orchestrator. Workers do not re-delegate (and `code` is the only worker that has the `agent` tool, gated by handoffs).
- `general-purpose` is the fallback worker: every other worker covers one slice, so an open-ended subtask that mixes discovery, change, and verification had to be split by hand. It holds the read/write/execute surface those specialists split between them, and its prompt lives in `agents/general-purpose.md`.
- The runtime's `agent` dispatch already validates role + handoff compatibility (`isDelegationRoleAllowed` in `internal/agent/llm_runtime_shell.go`), so workers can't accidentally spawn an orchestrator.
- Each delegated run is bounded by the `agent` tool's `timeout_sec`. A worker that can write files or run commands (`file-write`/`file-edit`/`multi-edit`/`shell-exec`/`bash` among its tools) gets 900s while the manifest still carries the shipped 300s; any other value set by the user applies as written. At 80% of the limit the worker is told to stop starting new work and report. A worker that times out or fails does not lose its work: the parent gets a `"status":"timed_out"` (or `"failed"`) report carrying its last message, `files_modified`, the shell commands it ran and its last tool results, and its changes stay on disk (or on its preserved branch under `isolation: "worktree"`). See `internal/agent/subagent_timeout.go`.

## Provider abstraction

- Streamed requests to OpenAI-compatible chat-completions endpoints (catalog providers with an OpenAI-style API, the Spettro Subscription, local servers) go through Spettro's own client in `internal/provider/wire/chatcompletions` (encoder, SSE reader, chunk decoder, tool-argument tracker) driven by `internal/provider/native*.go`. It sends the same request JSON as fantasy (pinned by golden tests) but caches each message's encoding across the steps of a run, so a step re-encodes only what it added (one cache lane per conversation, so the main agent and up to 47 concurrent sub-agents each keep theirs; 8 MB in all; a request of a single message, such as a compaction summary, takes no lane; a lane unused for 5 minutes is released; images go into the body straight from the media cache without being copied), and it decodes streamed tool-call arguments in linear time. `"provider_wire": "fantasy"` in `config.json` or `SPETTRO_PROVIDER_WIRE=fantasy` switches back; an encoder failure falls back to fantasy on its own.
- Everything else (Anthropic-protocol providers, the official `openai` provider, non-streamed requests) routes through Charm's `fantasy` SDK. Both streaming paths feed the same collector (`stream_collect.go`), so replies, finish reasons, usage and errors come out identical, apart from the few malformed replies the native client accepts where fantasy fails (listed under [Provider wire](configuration.md#provider-wire)).
- Legacy completion-only backends fall back to Spettro's direct SDK adapters, which use the same OpenAI and Anthropic SDK copies fantasy links.
- Per-request model metadata (vision, tool calling, context window, output limit) comes from an index rebuilt whenever a model list changes (`Manager.Lookup`), and attached images and tool schemas are cached across requests (`request_cache.go`; images up to 6 MB, each kept only in the forms requests used and released after 5 minutes without use).
- Known provider base URLs and local endpoints still resolve through the same manager layer.
- Catalog-backed model lists are preferred; fallback models are used when catalog is unavailable.
