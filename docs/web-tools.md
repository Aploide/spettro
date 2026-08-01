# Web Tools: web-search, web-fetch, download

Spettro ships three built-in tools that let agents reach the web: searching,
reading pages as markdown, and downloading files into the workspace. All three
share the same hardened HTTP client and the same network approval flow.

## web-search

Searches the web and returns result links.

```json
{"query": "go landlock ABI versions", "max_results": 5}
```

- `query` (required): the search terms.
- `max_results` (optional): number of results, default 10.

Output is one result per line: `Title — URL`. Result URLs are already decoded
to their real destinations, so they can be passed straight to `web-fetch`.

## web-fetch

Fetches a URL and returns its content as readable markdown. This is the tool
an agent uses to actually *read* a page found via `web-search`.

```json
{"url": "https://go.dev/doc/effective_go", "max_length": 20000}
```

- `url` (required): http/https URL.
- `max_length` (optional): output character budget. Default 20 000, hard cap
  50 000. Longer content is truncated to the per-tool history budget
  (see `toolOutputHistoryLimit` in `llm_runtime_prompt.go`) and the
  omitted portion is written to a session-scoped **spool** file so the
  model can page through it with `job-output {"job_id":"spool:N","offset":Z}`.

### Output format

HTML pages are converted to markdown and prefixed with a small front-matter
header so the model knows what it read and how fresh it is:

```
Title: Effective Go - The Go Programming Language
URL Source: https://go.dev/doc/effective_go
Published Time: 2026-01-02T03:04:05Z   (only when the page declares one)

Markdown Content:
# Effective Go
...
```

`URL Source` is the *final* URL after redirects. Non-HTML textual content
(JSON, XML, YAML, plain text, source code) is returned as-is, without front
matter.

### The HTML-to-markdown engine

Conversion is a three-stage pipeline (`internal/agent/webmarkdown.go`):

1. **DOM parse and pruning.** The page is parsed into a real DOM. Elements
   that never carry readable content are removed: scripts, styles, iframes,
   forms, SVG, templates, comments, and anything hidden (`hidden` attribute,
   `display:none`, `aria-hidden="true"`).
2. **Main-content extraction.** A semantic `<main>` or `<article>` element
   wins outright. Otherwise candidate containers are scored by the text they
   hold — paragraph length, comma count, positive/negative class and id hints
   (`article`, `content`, `post` vs `sidebar`, `nav`, `cookie`, `promo`, …) —
   and the score is scaled down by link density, so navigation menus and link
   farms lose. If the winning extraction keeps less than 30 % of the
   full-page conversion, it is considered too aggressive and the full page is
   used instead. If everything else fails, the tool falls back to stripped
   plain text.
3. **Markdown rendering.** A rule-based renderer produces GitHub-flavored
   markdown: ATX headings, nested ordered/unordered lists with correct
   numbering, GFM tables, fenced code blocks with language detection from
   `language-*`/`lang-*` classes, nested blockquotes, images as
   `![alt](url)`, bold/italic/strikethrough, and horizontal rules. All links
   and image sources are resolved to absolute URLs against the final page
   URL; `javascript:` and `data:` targets are dropped.

### Truncation and spooling

When `web-fetch` output exceeds the tool's history budget, the agent runtime
preserves the full result in a session-scoped spool file on disk. The model
receives a truncated head with a footer like:

```
[truncated: 480 of 1050 lines omitted; use job-output {"job_id":"spool:3","offset":4160} to read more]
```

The model can then call `job-output` with the `spool:N` ID and an `offset` to
page through the omitted portion. This is the same paging mechanism used by
other spooled tools (`file-read`, `grep`, `repo-search`, `shell-exec`, `bash`).

Spool files are session state: they are deleted when the session ends (TUI
exit, `/exit`, or goal completion).

### Limits and content types

- At most 2 MB of the response body is read before conversion.
- Binary content types (`application/octet-stream`, images, PDFs, archives,
  …) are rejected with a pointer to the `download` tool. Textual types
  (`text/*`, JSON, XML, YAML, `+json`/`+xml` suffixes) are accepted; a
  missing `Content-Type` is treated as text.
- Pages that render their content client-side with JavaScript come back
  mostly empty: the tool performs a plain HTTP GET, not a browser render. For
  those, the agent can drive a headless browser itself via the shell tools and
  *look* at the rendered page with `view-image` — see [vision.md](vision.md).

## download

Downloads a URL to a file inside the workspace. This is the escape hatch for
everything `web-fetch` refuses: binaries, archives, images, PDFs, datasets.

```json
{"url": "https://example.com/release.tar.gz", "path": "vendor/release.tar.gz", "max_bytes": 50000000}
```

- `url` (required): http/https URL.
- `path` (required): destination path. Must resolve inside the workspace;
  parent directories are created as needed. Symlink escapes are rejected
  under an active sandbox.
- `max_bytes` (optional): size limit. Default 20 MB, hard cap 200 MB. Both a
  declared `Content-Length` above the limit and an actual oversized body
  abort the download.

Downloads stream to a temporary file next to the destination and are renamed
into place only on success, so a failed or oversized transfer never leaves a
partial file. The tool reports the path, byte count, and content type.

`download` requires approval by default (`requires_approval = true`, risk
level `high` in the manifest): each call asks for both the network target and
the file write unless the session runs in yolo mode. Destination paths are
additionally checked against the OS sandbox write roots when the sandbox is
active — a read-only sandbox blocks downloads entirely.

## Security model (shared by all three tools)

- **SSRF protection**: connections are validated at dial time against the
  *resolved* IP, so loopback, RFC1918/ULA private ranges, link-local, and
  cloud-metadata addresses (169.254.169.254) are blocked — including after a
  redirect or DNS rebind.
- **Scheme validation**: only `http` and `https` URLs are accepted.
- **Redirect cap**: at most 5 redirects are followed, and every hop is
  re-validated.
- **Network approvals**: outside yolo mode each distinct target asks for
  approval. Choosing *allow always* persists the target in
  `.spettro/allowed_network.json` in the project root, so repeat visits don't
  re-prompt. Manifest `permission_rules` with the `network` permission can
  pre-allow or deny targets by pattern.

## Manifest registration

All three tools are registered in the default agent manifest with the
`network` permitted action:

| Tool | Actions | Approval | Risk | Timeout |
|---|---|---|---|---|
| `web-search` | `search`, `network` | required | medium | 30 s |
| `web-fetch` | `read`, `network` | required | medium | 45 s |
| `job-output` | `read` | no | low | 10 s |
| `job-kill` | `execute` | no | low | 10 s |
| `download` | `write`, `network` | required | high | 180 s |

By default the `coding` and `code` agents get `web-fetch` and `download`; the
read-only `ask` agent gets `web-search` and `web-fetch` (no `download`).

Projects with an existing `spettro.agents.toml` do **not** gain these three
tools automatically — no migration appends them. To enable the tools in such
a project, copy the `[[tools]]` entries for `web-fetch` and `download` from a
freshly generated manifest (or this repository's own `spettro.agents.toml`)
and add the IDs to the relevant agents' `allowed_tools`. (The `view-image`
vision tool is the exception: the manifest v5 migration does retrofit it —
see [vision.md](vision.md).)

## Testing the markdown rendering

The converter has focused unit tests:

```bash
go test ./internal/agent -run 'ConvertHTMLPage' -v
```

To eyeball the conversion of an arbitrary URL or local HTML file, drop a
scratch `_test.go` into `internal/agent` that calls
`convertHTMLPage(src, baseURL).render(baseURL)` and prints the result — the
function is package-internal, so a test file is the shortest hook. Delete the
file afterwards.
