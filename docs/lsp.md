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
edit; the `lsp-restart` tool clears the failure mark.

## What the agent gets

- **Post-edit diagnostics** — after `file-write` / `file-edit` / `multi-edit`
  (and `rename-symbol`), the written file is synced to its server
  (`didOpen`/`didChange`, plus `didSave` for servers that check on save) and
  the errors it reports are appended to the tool result, so the agent fixes
  its own type errors in the next step instead of at build time:

  ```
  edited internal/api/handler.go (1 replacements)

  Diagnostics (errors) in internal/api/handler.go:
  internal/api/handler.go:42:9: undefined: reqID (compiler)
  Also 2 errors in 1 other file: internal/api/routes.go (2) — use the diagnostics tool to list them.
  ```

  Only errors are listed (warnings and hints are left to the `diagnostics`
  tool), at most 20 for the edited file, and other files get a one-line count
  — which is how a signature change that breaks callers shows up. A clean
  edit adds nothing. The wait is bounded to ~3s, server start included: the
  server's first publish after the change is awaited, then a short quiet
  window catches servers that publish in stages. The edit itself never fails
  because of the server; when it is still starting or does not answer in
  time, a one-line note says the file was not checked.

  The counts describe the files as they are on disk: before each check,
  files the server holds open are re-sent if they changed behind its back
  (a shell command, a `git checkout`) and closed if they were deleted, and
  `rename-symbol` sends every file it wrote. gopls is started with
  `diagnosticsDelay` set to `0s`, so it checks the packages that depend on
  the edited one in the same pass instead of a second pass a second later —
  a caller broken in another package is counted on the edit that broke it.
- **`diagnostics` tool** — diagnostics for one file, or everything published
  so far across the workspace when called without a path.
- **`references` tool** — references or definition for a symbol
  (by name or by line/character position).
- **`hover` tool** — type signature and documentation for a symbol
  (by name or by line/character position).
- **`rename-symbol` tool** — rename a symbol across the workspace. The
  combined multi-file diff goes through the same approval flow as
  `file-write`, a checkpoint is taken first (so `/rewind` covers it), and the
  result lists every file changed.
- **`lsp-restart` tool** — restart one or all servers and reload the config.
  A server still starting is cancelled rather than waited for.

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
  a language off.
- `enabled` — defaults to `true`; set `false` to disable a server.
- `filetypes` — extensions the server claims (defaults to the built-in list
  for known keys; required for custom keys like `zig` above).

Edits to `lsp.json` apply after an `lsp-restart` (or a new session).
