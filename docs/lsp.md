# Language Server (LSP) Integration

Spettro uses Language Server Protocol servers to give the agent real
diagnostics after every file edit and precise symbol navigation
(references / go-to-definition), instead of relying on grep alone.

**It works with zero configuration.** On first use of a supported file type,
Spettro looks for the matching language server and starts it automatically:
first on your `PATH`, then in the places toolchains install servers without
putting them on `PATH` — the project's `node_modules/.bin`, `$GOBIN`,
`$GOPATH/bin`, `~/go/bin`, `~/.local/bin` and `~/.cargo/bin`. If a server is
not installed, LSP silently degrades for that language — nothing breaks, you
just don't get diagnostics.

## Supported languages

| Server key | Filetypes | Auto-detected command(s), first found wins | Install |
|---|---|---|---|
| `go` | `.go` | `gopls` | `go install golang.org/x/tools/gopls@latest` |
| `typescript` | `.ts` `.tsx` `.js` `.jsx` | `typescript-language-server --stdio` | `npm i -g typescript-language-server typescript` |
| `python` | `.py` | `pyright-langserver --stdio`, then `pylsp` |  `pip install python-lsp-server` or `npm i -g pyright` |
| `rust` | `.rs` | `rust-analyzer` | `rustup component add rust-analyzer` |
| `c` | `.c` `.h` | `clangd` | distro package `clangd` / `clang-tools` |
| `cpp` | `.cpp` `.cc` `.cxx` `.hpp` `.hh` `.hxx` | `clangd` | distro package `clangd` / `clang-tools` |
| `csharp` | `.cs` | `csharp-ls`, then `OmniSharp -lsp` / `omnisharp -lsp` | `dotnet tool install -g csharp-ls` |
| `swift` | `.swift` | `sourcekit-lsp` | ships with the Swift toolchain / Xcode |

Servers start lazily (only when a matching file is read or edited), in the
background, once per session, and are cached per workspace. For an agent that
can edit files or query the server, reading a file is enough to start its
server, so it is usually up by the time the file is edited; read-only agents
(Ask, Explore, Review, ...) never start one. A sub-agent working in its own
git worktree has its own servers, and they are stopped when its workspace is
merged back or dropped. A server that fails to start is not retried on every
edit; an `lsp` restart (`op: "restart"`) clears the failure mark. A server
that stops reading its input (wedged or busy-looping) cannot hold up a
request past its deadline: when a write to it is still blocked at the
deadline, or when the user cancels, the server is stopped, and the next
request that needs it starts a fresh one.

## What the agent gets

- **Post-edit diagnostics** — after `file-write` / `file-edit`
  (and `rename-symbol`), the written file is synced to its server
  (`didOpen`/`didChange`, plus `didSave` for servers that check on save) and
  the errors it reports are appended to the tool result, so the agent fixes
  its own type errors in the next step instead of at build time:

  ```
  edited internal/api/handler.go (1 replacements)

  Diagnostics (errors) in internal/api/handler.go:
  internal/api/handler.go:42:9: undefined: reqID (compiler)
  Also 2 errors in 1 other file: internal/api/routes.go (2) — use the lsp tool (op: diagnostics) to list them.
  ```

  Only errors are listed (warnings and hints are left to the `lsp` tool's
  `diagnostics` op), at most 20 for the edited file, and other files get a one-line count
  — which is how a signature change that breaks callers shows up. A clean
  edit adds nothing. The wait is bounded to ~3s, server start included: the
  server's first publish after the change is awaited, and then:

  - a publish marked with the version of the text just sent (gopls,
    pyright, clangd) is final; the wait only lets the rest of that
    diagnosis pass arrive (10 ms of silence), since it carries the files
    that depend on the edited one;
  - typescript-language-server publishes syntactic errors first and
    semantic ones second, so the wait ends at its second publish, or 1 s
    after the first;
  - any other server is final once it has been quiet for a window: 30 ms
    for gopls, pyright and clangd, 300 ms for the rest (`settle_ms` below
    changes it).

  The edit itself never fails because of the server; when it is still
  starting or does not answer in time, a one-line note says the file was
  not checked.

  The counts describe the files as they are on disk: before each check,
  files the server holds open are re-sent if they changed behind its back
  (a shell command, a `git checkout`) and closed if they were deleted, and
  `rename-symbol` sends every file it wrote. gopls is started with
  `diagnosticsDelay` set to `0s`, so it checks the packages that depend on
  the edited one in the same pass instead of a second pass a second later —
  a caller broken in another package is counted on the edit that broke it.
- **`lsp` tool** — the read-only queries, one tool with an `op` argument:

  | `op` | Arguments | Returns |
  |---|---|---|
  | `diagnostics` | `path` (optional) | Diagnostics for one file, or everything published so far across the workspace when called without a path. |
  | `references` | `path`, plus `symbol` or `line` (and optional `character`) | Every reference to the symbol, declaration included, as `path:line:col`. |
  | `definition` | same as `references` | Where the symbol is defined, as `path:line:col`. |
  | `hover` | same as `references` | The symbol's type signature and documentation. |
  | `restart` | `server` (optional) | Restarts that server, or all of them, and reloads the config. A server still starting is cancelled rather than waited for. |

  A position is a symbol name (its first occurrence in the file) or a
  1-based `line`/`character`. The former `diagnostics`, `references`,
  `hover` and `lsp-restart` tools are hidden aliases of these ops
  (`references` with `kind: "definition"` is `op: "definition"`); see
  [Built-in tools](tools.md#retired-names). To keep an agent from some ops,
  give it `lsp-op` rules with the op as the pattern, e.g.
  `{ permission = "lsp-op", pattern = "restart", action = "deny" }`.
- **`rename-symbol` tool** — rename a symbol across the workspace. The
  combined multi-file diff goes through the same approval flow as
  `file-write`, a checkpoint is taken first (so `/rewind` covers it), and the
  result lists every file changed.

## Optional overrides: `.spettro/lsp.json`

You no longer need this file — it exists only to override the defaults.
Spettro reads `~/.spettro/lsp.json` (user-global) and then the project's
`.spettro/lsp.json`; the project file wins per server key, and both overlay
the auto-detected defaults.

```json
{
  "servers": {
    "go": { "enabled": false },
    "typescript": { "command": "deno", "args": ["lsp"] },
    "zig": { "command": "zls", "filetypes": [".zig"] }
  }
}
```

Per entry:

- `command` / `args` — replace the detected server for that key. Omitting
  `command` keeps the detected one, so `{ "enabled": false }` alone just turns
  a language off. An entry without `command` changes only the fields it
  sets: a project's `{ "settle_ms": 200 }` keeps an `"enabled": false` from
  `~/.spettro/lsp.json`.
- `enabled` — defaults to `true`; set `false` to disable a server.
- `filetypes` — extensions the server claims (defaults to the built-in list
  for known keys; required for custom keys like `zig` above).
- `settle_ms` — how long post-edit diagnostics keep listening after the
  server's first unversioned publish before taking it as final (for
  `typescript`, the most it waits for the semantic publish). Raise it for a
  server that reports in several late publishes; like `enabled`, it can be
  set without `command`, e.g. `"python": { "settle_ms": 200 }`.

Edits to `lsp.json` apply after an `lsp` restart (or a new session).
