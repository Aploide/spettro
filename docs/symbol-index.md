# Symbol-aware repo search

`grep`'s `symbol` argument is backed by a lightweight symbol index
(`internal/indexer`). When the agent looks up a bare identifier — a function,
method, type, class, const or variable name — with `grep {"symbol": "Name"}`,
the result starts with a ranked `definitions:` block, followed by the usual
case-insensitive full-text matches (the usages):

```
3 definitions:
internal/agent/searcher.go:31  type RepoSearcher  type RepoSearcher struct {
...

42 matches:
internal/agent/llm_runtime.go:222: searcher RepoSearcher
...
```

Each definition line has the form `path:line  kind name  signature`.
Definitions are ranked exact name → case-insensitive exact → prefix →
substring. Queries that are not identifier-shaped (phrases, paths, regexes)
skip the index entirely and behave exactly like before.

The prompt guidance steers symbol lookups to `grep` with `symbol` first;
`grep` with a `pattern` stays the default for regexes, phrases and
non-symbol text. The two forms are exclusive: a call passes one or the other.

This lookup used to be a separate `repo-search` tool (granted to every agent
holding `grep` by the v7 manifest migration). Manifest v12 folded it into
`grep`; `repo-search {"query": q}` still works as an alias of
`grep {"symbol": q}`, and an empty symbol lists every file.

## How the index works

- **Backends are pluggable.** The built-in backend extracts symbols with
  per-language line patterns for Go, Python, and JavaScript/TypeScript
  (`.js/.jsx/.ts/.tsx/.mjs/.cjs`). Unsupported languages simply fall back to
  plain full-text search — the index never makes a search slower than grep.
- **Lazy, shared and cached.** Nothing is scanned until the first symbol
  lookup of a session (the TUI warms the index in the background at
  startup). There is one index per workspace root per process, shared by
  the TUI and every session, persisted at
  `<project>/.spettro/cache/symbols.idx` and reloaded by the next process
  (older `symbols.json` and `symbols.gob` caches are removed). The cache is
  rewritten only when the index changed, never by a start-up on an
  unchanged tree.
- **Freshness.** Only the first lookup of a process with no usable cache
  waits for a full scan. After that a lookup answers from memory at once
  (a few milliseconds on a 60k-file tree) and never waits for a re-scan;
  instead:
  - the files the symbol search's usage `grep` matched are re-checked by
    mtime and size (and re-parsed, or added, when they changed) before the
    definitions are listed. Every file defining the name contains it, so
    unless the usages were cut at 200 matches the definitions are current
    even for edits made outside Spettro a moment ago;
  - the files of the best 50 definitions are re-checked the same way, so a
    listed definition never points into a changed or deleted file;
  - the agent's own `file-write` / `file-edit` tools queue the touched file
    for re-parsing before the next lookup;
  - a full re-scan (new, changed and deleted files) starts in the
    background when the last one is more than 30 seconds old, or after a
    `bash` command (foreground or background job) or a checkpoint rewind.
  So a definition can be missing only from a file changed outside
  Spettro that the usage grep did not reach (a name with more than 200
  matches), until that background re-scan finishes.
- **Bounded.** Indexing stops at 100k source files or 10 seconds (reading
  and parsing run in parallel), skips files over 1 MiB, and respects
  `.gitignore` files the way `grep` does (nested ones included; files
  above the project root do not apply, so a dotfiles `~/.gitignore` cannot
  hide a project's sources) plus the usual junk directories (`.git`,
  `node_modules`, `vendor`, `dist`, `build`, virtualenvs). Symlinks to
  source files are indexed under the link's path; symlinked directories
  are not followed. When the file cap or the time bound cuts the index
  short, the result says so, also when no definition was found (the
  definition may be in the part never indexed).
- **Usages.** The matches after the definitions are a case-insensitive
  literal `grep` for the name, so they skip ignored, binary and oversized
  files like any `grep`, and stop at 200 (with the usual truncation note).
  An empty symbol lists the workspace's files as `glob` `**` does, capped at
  1000.
