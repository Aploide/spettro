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
  `<project>/.spettro/cache/symbols.gob` and reloaded by the next process
  (an older `symbols.json` cache is removed).
- **Freshness.** A lookup re-syncs with the filesystem when the last sync is
  more than 5 seconds old: files whose mtime or size changed are re-parsed,
  deleted files are dropped. Within those 5 seconds it answers from memory
  (a warm lookup takes milliseconds on a 60k-file tree), except that the
  agent's own `file-write` / `file-edit` tools queue the touched file for
  re-parsing and each foreground `bash` command forces a full re-sync on the
  next lookup. So only an edit made outside Spettro can be missed, for at
  most 5 seconds.
- **Bounded.** Indexing stops at 100k source files or 10 seconds (reading
  and parsing run in parallel), skips files over 1 MiB, and respects
  `.gitignore` files the way `grep` does (nested ones included) plus the
  usual junk directories (`.git`, `node_modules`, `vendor`, `dist`,
  `build`, virtualenvs). When the file cap or the time bound cuts the
  index short, the `definitions:` block says so.
- **Usages.** The matches after the definitions are a case-insensitive
  literal `grep` for the name, so they skip ignored, binary and oversized
  files like any `grep`, and stop at 200 (with the usual truncation note).
  An empty symbol lists the workspace's files as `glob` `**` does, capped at
  1000.
