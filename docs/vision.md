# Vision: view-image and agent-driven screenshots

Spettro lets the agent *see* images mid-run. The single primitive is the
`view-image` tool: it attaches an image file from the workspace to the
conversation as real vision input, so the model looks at the pixels instead of
reading a path.

There is deliberately **no** dedicated screenshot tool. Capturing is ordinary
work the agent already knows how to do with the shell — a headless browser, a
plotting script, ImageMagick — and hard-coding one capture method would just
limit it. The pattern is always:

1. produce an image file by any means (shell, script, existing asset);
2. call `view-image` on it;
3. reason about what it saw and continue.

## view-image

```json
{"path": "shots/home.png"}
```

- `path` (required): image file **inside the workspace** (the same containment
  as `file-read`; symlink escapes are rejected).
- Formats: `png`, `jpg`/`jpeg`, `webp`, `gif`.
- Size limit: 4 MB. Larger files are refused with a hint to resize or
  re-capture smaller (providers reject oversized base64 payloads).
- Requires a vision-capable model. On a text-only model the tool fails with an
  explanatory error instead of silently attaching nothing; the model catalog's
  `Vision` capability flag decides (`/models` shows it).

Output is a small JSON receipt (`file`, `media_type`, `size_bytes`,
`attached: true`); the image itself rides back to the model out-of-band (see
*How images reach the model* below). The file is also marked as read, so a
follow-up `file-write` to the same path needs no separate read.

## The self-screenshot workflow

To review a website the agent runs its own headless browser and then looks at
the result:

```bash
# any Chromium-family browser
chromium --headless --disable-gpu --window-size=1280,800 \
  --screenshot=shot.png http://localhost:3000

# or Playwright (full-page, waits, device emulation, ...)
npx playwright screenshot --viewport-size=1280,800 --full-page \
  http://localhost:3000 shot.png
```

then:

```json
{"tool": "view-image", "arguments": {"path": "shot.png"}}
```

Because capture goes through the normal shell tools, it inherits the whole
existing policy surface for free: command approvals, the OS sandbox, allowed
command persistence, background jobs for a dev server (`run_in_background`),
and so on. Nothing about the browser invocation is special-cased.

The same two steps cover every other "look at this" need: render a matplotlib
chart and check the axes, generate an asset with `grok-image` and inspect it,
open a design PNG the user dropped into the repo.

## How images reach the model

`view-image` never talks to a provider. It registers the file on a per-call
**image sink** (`internal/agent/view_image.go`); the tool loop collects sink
paths into the tool's result, and the provider layer renders them per backend:

- **Native tool calling, Anthropic** — the image is embedded *inside* the
  `tool_result` block as media (base64 + mime), alongside the JSON receipt, so
  the model associates the image with the exact call that produced it.
- **Native tool calling, other providers** (OpenAI, OpenAI-compatible, local)
  — their tool results are text-only, so the image is re-attached as an
  immediately following user turn (`[image attached from the tool result
  above]`). Message-level images work on every provider path.
- **Text protocol** (models without native tool support) — the image rides on
  the tool-feedback user message itself.

In all cases the attachment is append-only, so the provider prompt cache keeps
hitting. A vanished or unreadable file degrades to the text-only result rather
than failing the request. Images inside carried history are re-sent with each
step (and survive into future turns) until in-loop compaction summarizes them
away.

## ACP (editor) integration

Over the Agent Client Protocol the tool's images are also forwarded to the
client: the `tool_call_update` for `view-image` carries an image content block
(base64 + mime) next to the text output, so editors that render ACP content
show the screenshot inline in the tool-call card. Prompt-side images (the user
pasting a screenshot in the editor) were already supported; this closes the
loop in the other direction. See [acp.md](acp.md).

## Manifest registration

`view-image` is a builtin with `read` as its only permitted action, no
approval requirement, risk `low`, timeout 15 s. Manifest **v5** migrates
existing projects automatically: loading a pre-v5 `spettro.agents.toml` adds
the tool definition and appends `view-image` to every agent that already has
`file-read` (an agent trusted to read the workspace is trusted to look at its
images). Agents without `file-read` are left untouched, and a user-defined
`view-image` tool is never overwritten.

## Testing

```bash
go test ./internal/agent  -run 'ViewImage|ImageSink'         # tool + sink
go test ./internal/provider -run 'ToolResultImages'          # provider mapping
go test ./internal/acp    -run 'ToolOutputContent|ToolKind'  # ACP content
go test ./internal/config -run 'V5Migration|ViewImage'       # manifest migration
```
