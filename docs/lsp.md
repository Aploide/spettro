# Language Server (LSP) Integration

Spettro uses Language Server Protocol servers to give the agent real
diagnostics after every file edit and precise symbol navigation
(references / go-to-definition), instead of relying on grep alone.

**It works with zero configuration.** On first use of a supported file type,
Spettro looks for the matching language server on your `PATH` and starts it
automatically. If a server is not installed, LSP silently degrades for that
language — nothing breaks, you just don't get diagnostics.

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

Servers start lazily (only when a matching file is touched) and are cached per
workspace. A server that fails to start is not retried on every edit; the
`lsp-restart` tool clears the failure mark.

## What the agent gets

- **Post-edit diagnostics** — after `file-write` / `file-edit` / `multi-edit`,
  fresh diagnostics for the changed file are appended to the tool result
  (bounded to ~3s so edits never feel slow), so the agent sees its own type
  errors immediately instead of at build time.
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
