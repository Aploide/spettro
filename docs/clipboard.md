# Clipboard, Attachments, and Media

Spettro supports pasting images from the system clipboard, attaching files to
your prompt, and copying assistant responses — all directly from the keyboard
inside the TUI.

## Paste image from clipboard (`Ctrl+V`)

When the active model supports vision (image input), you can paste an image
directly from the system clipboard into your prompt.

```text
1. Copy an image to your clipboard (screenshot, download, etc.)
2. Inside Spettro, press Ctrl+V
3. An attachment chip appears: "Image #1"
4. Type your prompt and press Enter — the image is sent alongside your text
```

Supported formats: **PNG**, **JPEG**, **WebP**.

The image is saved to a temporary directory under
`~/.spettro/clipboard/<session>/` and referenced from there for the duration
of the session. Pasted images are **not** persisted across sessions — they are
ephemeral.

If the model does not support vision, Spettro shows a banner: `current model
does not support vision` and the paste is ignored.

### Platform support

| Platform | `Ctrl+V` clipboard image paste |
|----------|-------------------------------|
| macOS    | ✅ Supported (native clipboard) |
| Linux    | ✅ Supported (X11/Wayland via `xclip` or `wl-paste`) |
| Windows  | ❌ Not yet supported |

Pasting on an unsupported platform prints a one-time error banner.

## Attach files (`Ctrl+F`)

You can attach workspace files and text snippets to your prompt so the agent
receives them as context alongside your message.

### Attach a workspace file

1. Press `Ctrl+F` to open the attach prompt.
2. Type the **absolute path** or **relative path** of a file in the workspace.
3. Press `Enter` to attach it.

An attachment chip appears below the input area:

```
[file: src/main.go]
```

### Remove an attachment

- Press `Ctrl+R` to remove the most recent attachment. Repeating removes
  attachments in reverse order (LIFO).

### How attachments are sent

When you submit your prompt, every attached file is:

1. Read from disk.
2. Included in the LLM request alongside your prompt text.
3. The attachment chips are cleared after submission (one-shot).

If you have 2 or more attachments, a system message is added to the transcript:

```
(with 2 attachments)
```

Attachments are a convenience for `@`-mention-style file context. They do not
replace the `file-read` tool — the agent can still read files on its own.

## Copy last response (`Ctrl+Y`)

Press `Ctrl+Y` to copy the most recent assistant response to your system
clipboard:

```text
Ctrl+Y  →  "last response copied to clipboard"
```

This works even after the agent has finished streaming — it copies the
authoritative final message, not any intermediate draft.

If there is no assistant message yet, the banner shows `no response to copy
yet`.

## Text Select Mode (`Ctrl+T`)

When you need to select text inside the terminal (for copy-paste outside
Spettro), press `Ctrl+T` to toggle **text-select mode**:

- **Mouse capture off** — you can now click-and-drag to select text in your
  terminal emulator.
- The banner shows `text-select mode — mouse off, ctrl+t to re-enable`.
- Side panel clicks and scroll-wheel scrolling are disabled until you press
  `Ctrl+T` again to re-enable mouse capture.

This is useful when you want to copy a snippet from a tool output or an
assistant's response to another application.

## Keybinding summary

| Key | Action |
|-----|--------|
| `Ctrl+V` | Paste image from clipboard (vision-capable models only). |
| `Ctrl+F` | Attach a workspace file path. |
| `Ctrl+R` | Remove the most recent attachment. |
| `Ctrl+Y` | Copy the last assistant response to clipboard. |
| `Ctrl+T` | Toggle text-select mode (mouse capture on/off). |
| `Ctrl+O` | Toggle expanded tool detail in the side panel. |