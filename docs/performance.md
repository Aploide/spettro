# Performance

Measured on 2026-09-27. This page is for two readers:

- **Users** who want to know what Spettro costs in start-up time, CPU and
  memory, and how it compares with Claude Code and OpenCode on the same
  machine and the same workload.
- **Maintainers** who change a hot path and need to re-measure it the same
  way, or who want to know why a target was missed.

It compares `core/rework` at `162e78d` ("now") with `0f95899` ("base"), the
commit before the performance work. That work was merged in five parts
(start-up `651da69`, TUI `fa65361`, engine `5c35f77`, tools `ddd3bb6`,
provider `d70e626`) followed by the integration commits `5733d19`,
`c69d385`, `8472a4b` and `162e78d`. Each target in the table below was set
before the work started, next to what Claude Code and OpenCode did on the
same workload.

## Methodology

- **Machine.** One Apple M-series Mac (darwin/arm64), shared with other
  jobs. Every row gives the 1-minute load average it ran at; compare rows
  only at similar load.
- **Builds.** Both Spettro binaries were built with the release flags,
  `GOWORK=off go build -trimpath -ldflags='-s -w'`. Claude Code was version
  2.1.283 and OpenCode version 1.18.32, as installed.
- **Interleaving.** Base and now always ran alternately, the order
  rotating every iteration, in the same time window, with two warm-up
  pairs discarded and n >= 15 measured pairs unless a row says otherwise. Every cell is a
  median. Min and p95 are in the raw logs; a result is judged on the medians
  of the interleaved pair, never on a single run.
- **No network.** Every CLI was pointed at a local fake LLM server: an
  OpenAI-compatible HTTP server on 127.0.0.1 that streams scripted answers
  and tool calls at a fixed token rate. No real provider, key or credit was
  involved.
- **Isolated home.** Every binary ran with `HOME`, `USERPROFILE` and the XDG
  directories in a scratch directory prepared for the run (a dummy key,
  onboarding done, the working directory trusted, no Telegram or remote
  settings).
- **Timing points.**
  - *First frame*: from `fork` of the process in a 120x40 pseudo-terminal to
    the first output that, with escape sequences stripped, contains the
    CLI's ready marker (for Spettro, the permission mode in the status bar
    on the frame's last row).
  - *Headless / ACP ready*: from `fork` to the `SPETTRO_PORT=` line, or to
    the JSON-RPC response of `initialize` / `session/new`.
  - *CPU* is user + system time of the whole process (for Claude Code and
    OpenCode, the whole process tree).
  - *Engine* and *tool* rows run the code in-process from a Go test binary
    built from each commit, so they exclude process start.

## Results

"Met?" compares the median of "now" with the target. A dash means the
row was not measured for that CLI (most rows exercise Spettro internals that
have no counterpart).

| Metric | Base | Now | Target | Met? | Claude Code | OpenCode | Load (1m) |
|---|---|---|---|---|---|---|---|
| TUI first frame, keyed, small git repo (567 files) | 168.2 ms | 45.9 ms | <= 30 ms | no | 283.6 ms | 2724 ms | 2.2 (the 4-CLI run: 18.6) |
| TUI first frame, 30k-file repo | 252.7 ms | 36.2 ms | <= 45 ms | yes | - | - | 3.7 |
| First frame, black-holed proxy / black-holed local endpoint | 10152 / 5152 ms | 52.8 / 52.9 ms | <= 50 ms | no (by 3 ms) | - | - | 1.0-1.4 |
| First frame, no keys | 51.0 ms | 37.1 ms | no regression | yes | - | - | 1.6 |
| Headless ready, keyed | 103.5 ms | 19.3 ms | <= 20 ms | yes | - | - | 1.6 |
| ACP `initialize`, keyed | 104.7 ms | 20.2 ms | <= 20 ms | borderline (+0.2 ms) | - | - | 1.6 |
| ACP `session/new`, keyed | 186.0 ms | 22.4 ms | <= 25 ms | yes | - | - | 1.6 |
| ACP `initialize`, `--sandbox workspace-write` | 217.8 ms | 45.1 ms | <= base unsandboxed + 30 ms | yes | - | - | 1.5 |
| `--version` wall / exit code | no such flag (exit 2) | 14.3 ms / 0 | <= 10 ms, exit 0 | no (exit 0: yes) | 12.0 ms | 394 ms | 1.3 |
| `--help` wall | 18.5 ms | 14.9 ms | <= 10 ms | no | 74.8 ms | 406 ms | 1.3 |
| Binary size, stripped | 55.0 MB | 50.2 MB | <= 42 MB | no | 225.0 MB | 144.6 MB | - |
| Peak RSS of `--help` | 36.1 MB | 26.4 MB | <= 25 MB | no | 106.6 MB | 184.2 MB | 1.3 |
| Idle TUI CPU (30 s window, n=5) | 18.6 % | 0.7 % | <= 0.8 % | yes | 1.2 % | 5.2 % | 15.3 |
| Idle TUI CPU, empty directory (n=5) | 18.0 % | 0.7 % | <= 0.8 % | yes | - | - | 10.0 |
| 200-turn TUI session, total CPU (n=3 each) | 85.4 s | 14.7 s | <= 15 s (stretch <= 10 s) | yes (stretch: no) | 13.6 s (n=1) | 77.7 s (n=1) | 11-17 |
| Turn latency, turns 1-20 -> turns 180-200 | 167.6 -> 571.4 ms | 166.6 -> 169.9 ms | flat, within 10 % | yes (+2 %) | 182 -> 200 ms | 302 -> 330 ms | 11-17 |
| Update+View per stream chunk at 1k msgs / 200 tools, p50 / p99 (4 runs of 1500 chunks) | 66.7 / 68.4 ms | 0.30 / 0.90 ms | <= 2 / <= 16 ms | yes | - | - | 1.4-6.9 |
| Toggle freeze at 1k/200 (ctrl+o, ctrl+g, ctrl+b, shift+tab), median / max | 632 / 1469 ms | 13.9 / 16.7 ms | <= 30 ms | yes | - | - | 1.4-6.9 |
| 1k-message session + 200-tool turn, tools phase | 34.27 s | 3.18 s | <= 5 s | yes | - | - | 1.0-1.7 |
| Same run: token latency p50 / displayed rate while streaming | 15049 ms / 26.0 tok/s (960 of 1000 tokens shown) | 15.6 ms / 50.0 tok/s | p50 <= 30 ms, 50 tok/s | yes | - | - | 1.0-1.7 |
| Engine overhead per step at 500 msgs (Go CPU, allocated) | 16.07 ms, 5.34 MB | 0.86 ms, 0.25 MB | <= 2 ms, <= 0.5 MB | yes | - | - | 1.7 |
| Engine overhead per step at 10 msgs (Go CPU) | 3.38 ms | 0.70 ms | <= 1 ms | yes | - | - | 1.7 |
| 50 KB tool call in 12k fragments: engine CPU / wall | 2720 / 1721 ms | 13.2 / 62.9 ms | <= 30 ms | CPU: yes (the wall time is the fake server pacing 12k events) | - | - | 1.7 |
| 20k-chunk answer: engine CPU / wall | 1859 / 730 ms | 30.6 / 82.9 ms | <= 40 ms engine time | yes | - | - | 3.4 |
| Mutating step, wait on the checkpoint (570-file repo): speculative hit / synchronous fallback | - / 55.4 ms | 0.7 / 55.7 ms | <= 5 ms on a hit | yes | - | - | 2.9 |
| Go `file-edit` with gopls (post-edit diagnostics included) | 310.8 ms | 15.2 ms | <= 40 ms | yes | - | - | 5.3-7.1 |
| Go-fallback `grep`, full scan, no match (61k files) | 6078 ms | 1549 ms | <= 2.0 s | yes | - | - | 1.2-4.4 |
| Go-fallback `grep`, case-insensitive (61k files) | 18186 ms | 1724 ms | <= 3.0 s | yes | - | - | 2.1-4.4 |
| `glob **/*.go` (61k files, 42k matches) | 1465 ms | 199.8 ms | <= 0.4 s | yes | - | - | 1.2-1.4 |
| `glob` with a literal prefix (`go126/src/net/http/**/*.go`) | 1426 ms | 1.04 ms | <= 10 ms | yes | - | - | 1.2-1.4 |
| Memory: TUI idle RSS / 200-turn session peak RSS | 58.6 / 76.6 MB | 43.4 / 66.0 MB | no regression > 15 MB | yes (both lower) | 278 / 504 MB (process tree) | 1186 / 1795 MB (process tree) | 11-17 |

Notes on the comparison columns:

- The first-frame and idle-CPU cells for Claude Code and OpenCode come from
  one run that interleaved all four CLIs (n=15 for first frame, n=5 for
  idle). The first-frame run happened at load 18.6; in it Spettro base took
  167.6 ms and now 47.0 ms, so the order is the same as in the quieter
  Spettro-only run (n=20, load 2.2) that the Base and Now cells show.
- The 200-turn columns for Claude Code and OpenCode are single runs made
  next to the Spettro runs. Their CPU and RSS cover the whole process tree.

### What the rows measure

- **Update+View per stream chunk** and **toggle freeze** use a session
  seeded with 1000 messages and a 200-call tool turn, driven through the
  real Bubble Tea program.
- **1k-message session + 200-tool turn** is the wall time of the tools
  phase of one turn (10 rounds of 20 shell calls, each printing 3000 lines)
  on top of a resumed 1000-message session, measured end to end in a
  pseudo-terminal until the last tool result is on screen. The same turn
  then streams a 1000-token answer at 50 tokens/s; token latency is the
  time from the fake server sending a token to that token appearing on the
  screen (every token carries a unique marker). During that stream Spettro
  used 22 % of a core, against 104 % for base, which fell behind; 40 of the
  1000 token markers never showed up in its output.
- **Engine overhead per step** is the agent loop's own work per model step
  (prompt estimate, request encoding, loop detection), without the network.
- **Tool rows** run the `grep`, `glob` and `file-edit` implementations on a
  61k-file corpus (three Go source trees, a `node_modules` tree and a 1.4
  million-line text file). The grep rows force the built-in Go search (no
  ripgrep), which is what runs until ripgrep is available. The gopls row is
  one `file-edit` of a 50-function Go file in a module with a warm gopls,
  including the wait for post-edit diagnostics.
- **200-turn TUI session** replays 200 scripted turns through the TUI and
  records total CPU, the latency of each turn and peak RSS.

## How to reproduce

### Micro-benchmarks (in the repository, run in CI form)

These track the same code paths as the rows above on synthetic data small
enough for CI:

```sh
GOWORK=off go test -run '^$' -bench . -benchmem ./internal/tui          # stream chunk, tool trace, toggles, side panel, header
GOWORK=off go test -run '^$' -bench . -benchmem ./internal/provider     # request encoding, streaming decode, model lookup
GOWORK=off go test -run '^$' -bench . -benchmem ./internal/agent        # prompt estimate, loop detector, grep/glob/walk
GOWORK=off go test -run '^$' -bench . -benchmem ./internal/checkpoint ./internal/indexer ./internal/ignore ./internal/fswalk ./internal/jobs ./internal/config
```

To compare two commits, run the same benchmark on both with `-count 10` and
compare with `benchstat`. Wall-clock numbers are too noisy for CI, so the
regression guards that do run in CI are deterministic instead:

- **Allocation guards** (`testing.AllocsPerRun` with a hard ceiling), for
  example the model lookup and the TUI's `contextWindow` allocate nothing
  (`internal/provider/lookup_test.go`,
  `internal/tui/model_lookup_bench_test.go`), and a steady-state prompt
  estimate or native request encoding stays under a fixed count.
- **Work-count guards**, for example `TestStreamChunkReRendersOneBlock`,
  `TestNoGitOnTheUpdateGoroutine`, `TestCommitRunsNoGit` and
  `TestKeysV2RoundTripNeverRunsScrypt`.

### Process start, `--help`, `--version`, first frame

[`scripts/perf`](../scripts/perf/README.md) has two drivers that interleave
two builds and isolate the home for every run:

```sh
scripts/perf/procab.py base='/tmp/spettro-base.bin --help' now='bin/spettro --help'
scripts/perf/firstframe.py --cwd ~/code/some-repo --home-template /tmp/keyed-home \
    base=/tmp/spettro-base.bin now=bin/spettro
```

Binary size is `make size`.

### End-to-end sessions, engine and tool rows

These used scratch harnesses that are not committed, because they depend on
large local corpora and on internals that change often. Rebuilding one takes
little code:

- **Fake LLM server.** A small HTTP server that answers
  `/v1/chat/completions` with server-sent events: a scripted number of
  tokens at a fixed rate, and, for tool turns, rounds of `bash` tool calls.
  [`scripts/tui-e2e/fake_openai.py`](../scripts/tui-e2e/fake_openai.py) is a
  starting point.
- **TUI driver.** Start the binary in a pseudo-terminal with an isolated
  home whose `config.json` points `local_endpoints` and `active_provider` at
  the fake server, answer the terminal's start-up queries, type a prompt,
  and time the arrival of markers the fake server embeds in its tokens.
  Sample the process's CPU and RSS with `ps` while it runs. For seeded
  sessions, write a 1000-message session file into the isolated home and
  resume it.
- **Engine and tool rows.** A throwaway `_test.go` file in
  `internal/agent` (never committed) that calls the tool implementation
  directly (`runGrep`, `runGlob`, `runFileEdit`) or runs the agent loop
  against the fake server, prints wall time, CPU from `getrusage` and bytes
  allocated from `runtime.MemStats`, and runs one case per process. Build
  the test binary on both commits with `go test -c` and alternate them.
  The package's `TestMain` turns installed language servers off; the gopls
  case re-enables Go with a project `.spettro/lsp.json`.

Whatever harness you use, keep every run's `HOME`, `USERPROFILE` and XDG
directories in a scratch directory. A harness that runs Spettro with your
real home rewrites your configuration and keys.

## Targets not met, and why

Four targets were missed and one was met only to within noise. None of
them can be closed with changes inside this repository alone; each needs a
change to a dependency, and the maintainers decided against carrying forks
of Bubble Tea or fantasy (they cost more to keep in step with upstream than
the milliseconds are worth).

### TUI first frame: 45.9 ms against 30 ms

Instrumented marks (n=20) split the keyed first frame in a small repository
into three parts:

| Phase | Time |
|---|---|
| Process start to `main` (dynamic loading, Go runtime and package initialisation) | 21 ms from the fork in the pty |
| Spettro's own bootstrap plus `tui.New` | 4 ms |
| Waiting for Bubble Tea's first renderer tick | 17.5 ms |

Spettro's own part is down to 4 ms; in base it was about 130 ms by
subtraction (scrypt key derivation, a catalog fetch, git status and a second
manifest parse all ran before the first frame). The remaining 17.5 ms is
Bubble Tea's renderer: it paints only on its 60 fps ticker, so the first
complete `View` waits for the first tick. Flushing the first frame
immediately needs a change inside Bubble Tea. A patched copy was prototyped
and saves 11 to 17 ms, but a fork was ruled out; the change is suitable for
an upstream pull request, and the frame will drop to about 30 ms once
upstream has it. Raising the frame rate with `tea.WithFPS(120)` gets 33.5 ms
but raises idle CPU from 0.45 % to 0.65 %, so it was not adopted.

The process-start share is also why the frame is 21 ms before Spettro runs a
line of its own code: see the next section.

### `--version` (14.3 ms) and `--help` (14.9 ms) against 10 ms

`--version` is handled as the first statement of `main`, so its time is
almost entirely process start: loading a 50 MB binary and initialising the
Go runtime and the package-level state of every linked package. A
hello-world Go binary starts in 4 ms on the same machine; the difference
scales with the amount of code linked in, which is the binary-size problem
below. `--version` does exit 0 now (base had no such flag and exited 2).

### Binary size (50.2 MB against 42 MB) and `--help` RSS (26.4 MB against 25 MB)

Spettro's own packages are about 2.5 MB of the binary's code and data; the
rest is the Go runtime and standard library and the dependencies. The
largest avoidable part is in the provider stack: fantasy's Anthropic
provider links Bedrock and Vertex support unconditionally, which pulls in
the AWS SDK, Google's auth libraries, gRPC and protobuf, whether or not
anyone uses those providers. A prototype built at the start of this work
with the two removed from a patched copy of fantasy was 45.3 MB against
54.6 MB for the same commit, and 40.1 MB together with the SDK
deduplication described below, which would meet the target. The `--help`
RSS misses its target by 1.4 MB for the same reason: `--help` does almost
no work of its own, so its RSS is mostly the runtime and the linked
packages' code and data touched while the process starts, and it shrinks
with the binary.

What was possible without forking was done: Spettro's own code now imports
the SDK packages fantasy already depends on instead of second copies (part
of the 4.8 MB saved), every packaging path strips symbols, and the macOS
clipboard library is loaded on first paste instead of at start (an earlier
measurement put it at 8 MB of RSS per process). Removing Bedrock and
Vertex needs fantasy to move them into separate packages; that is an
upstream change, and a vendored copy was ruled out.
`make size` prints the size and the package count so that a new dependency
that makes it worse shows up in review.

### Close calls

- **First frame with a black-holed proxy or local endpoint: 52.8 ms against
  50 ms.** No network call is on the start-up path any more (base waited
  5 to 10 s for the timeouts). What remains is mostly the same process
  start and renderer tick as the normal first frame; the few milliseconds
  over the keyed row were not broken down further. It moves with the
  first-frame fix above.
- **ACP `initialize`: 20.2 ms against 20 ms**, within the run-to-run noise
  of the interleaved pair; also dominated by process start.
- **200-turn session CPU: 14.7 s**, inside the 15 s target but not the 10 s
  stretch goal. Garbage-collector tuning (`GOGC=150` with a 256 MiB memory
  limit) saved 3.3 % of CPU and cost 4.6 MB of peak RSS, below the 5 % bar
  set for changing it, so the defaults stay.

## Changes behind the numbers

A short map for maintainers; the commit messages and the doc comments on
each cache carry the details (what it caches, its key, what invalidates it,
which goroutine owns it).

- **Start-up** (`651da69`): `keys.enc` v2 with an HKDF key instead of scrypt
  on every start (migrated on first use, a `keys.enc.v1` backup kept for one
  release); an embedded model catalog snapshot, so no network call happens
  before the first frame; local endpoints and model lists discovered in the
  background; the macOS clipboard library loaded on first paste; sandbox
  re-exec before the configuration is loaded; `--version`.
- **TUI** (`fa65361`, `5733d19`, `c69d385`, `162e78d`): each message is
  rendered once and cached; stream chunks are batched and re-render only
  the streaming block; timers stop when idle; git status runs off the UI
  goroutine; the side-panel activity feed keeps the last 2000 items; the
  header reads model metadata through an index.
- **Engine** (`5c35f77`, `8472a4b`): the prompt size is estimated
  incrementally; the loop detector normalises in one pass; tool output
  spools asynchronously; checkpoints are prepared speculatively while the
  model is still streaming.
- **Tools** (`ddd3bb6`): LSP diagnostics wait for the server to settle
  instead of a fixed delay; a compiled gitignore matcher and a parallel
  walker; a literal glob prefix is walked directly; ripgrep is downloaded
  (pinned, checksum-verified) on first use, with the Go search as fallback;
  one symbol index shared per workspace.
- **Provider** (`d70e626`): Spettro's own chat-completions client for
  OpenAI-compatible, subscription and local endpoints, which re-encodes only
  the messages a step added and decodes streamed tool-call arguments in
  linear time; the fantasy SDK stays as the fallback and for Anthropic
  (`provider_wire`, see [configuration](configuration.md)).
