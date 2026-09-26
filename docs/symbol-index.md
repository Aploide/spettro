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
- **Lazy and cached.** Nothing is scanned until the first symbol lookup of a
  session. The index is persisted at `<project>/.spettro/cache/symbols.json`
  and reloaded on the next session.
- **Invalidation.** Every lookup re-syncs with the filesystem: files whose
  mtime or size changed are re-parsed, deleted files are dropped. The agent's
  own `file-write` / `file-edit` tools additionally invalidate
  the touched file directly.
- **Bounded.** Indexing stops at 20k files or 5 seconds, skips files over
  1 MiB, and respects `.gitignore` (shared matcher in `internal/ignore`) plus
  the usual junk directories (`.git`, `node_modules`, `vendor`, `dist`,
  `build`, virtualenvs). Queries on a ~1k-file repo answer in milliseconds.
