# Interactive PTY sessions

Plain `shell-exec` runs commands through pipes: anything that needs a real
terminal — REPLs (`python`, `node`), debuggers (`gdb`, `dlv`), `ssh`,
password prompts, watch-mode dev servers, TUI programs — either hangs until
the tool timeout or misbehaves (no line editing, `isatty` false paths). PTY
sessions give the agent a real pseudo-terminal it can type into.

## Tools

Three model-facing tools (granted by default to every agent that already has
`shell-exec`):

| Tool | Description |
|------|-------------|
| `pty-start {command, cols?, rows?}` | Allocates a pseudo-terminal (default 120×32), runs `command` through `bash -lc` under it, and returns a session ID (`pty-N`) plus the initial screen. |
| `pty-write {id, input?, submit?, wait_for?, wait_ms?}` | Sends input and returns output produced since the last read. `submit: true` appends `\r` (line submit). `wait_for` polls until a literal string (e.g. the prompt `">>> "`) appears in new output, up to 10 s by default — prefer it over guessing `wait_ms` (single wait, default 700 ms, up to 30 s). Empty `input` just polls. |
| `pty-kill {id}` | Terminates the session's process group (SIGTERM, then SIGKILL after a 2 s grace) and frees the terminal. |

**Input encoding — the canonical rule:** backslash escape sequences in
`input` are decoded server-side into real bytes: `\r` `\n` `\t` `\e` (ESC),
`\xHH`, `\uHHHH`, and `\\` for a literal backslash. It does not matter
whether a real control byte or the literal text `\x03` lands in the JSON —
both arrive at the terminal as Ctrl-C, so the break character always works.
Unknown escapes pass through unchanged.

A typical session:

```
pty-start {"command": "python3"}                → pty-1 + the >>> banner
pty-write {"id": "pty-1", "input": "2**64", "submit": true, "wait_for": ">>> "}
                                                → 18446744073709551616
pty-write {"id": "pty-1", "input": "\x04"}      → Ctrl-D, interpreter exits
pty-kill  {"id": "pty-1"}                          (or rely on session cleanup)
```

## Security and approval

- `pty-start` goes through the exact same approval path as `shell-exec`: the
  same blocked-command list, permission rules, allowlist, and permission
  hooks apply.
- **Approval policy for input:** approving a `pty-start` command covers all
  subsequent `pty-write` input into that session. Free-form input into an
  approved interactive process is equivalent to running follow-up commands,
  so under `ask-first` treat a pty-start approval as "this program may be
  driven unattended". Deny the start if that is not acceptable.
- The child runs under the same [OS sandbox policy](sandbox.md) as
  `shell-exec` — the PTY is not a sandbox escape; kernel-level confinement
  wraps the process identically.

## Output handling

- Each session keeps a 256 KiB scrollback ring buffer; older bytes are
  dropped. The model receives settled text: ANSI escapes are stripped and
  carriage returns / backspaces are applied as a terminal would (so
  readline's per-keystroke line redraws collapse into the final line instead
  of partial-frame soup). The raw bytes stay in the buffer.
- Large reads are spooled through the standard
  [tool-output spooling](session.md#tool-output-spooling) path, so huge
  scrollback never floods the context (page the rest with `job-output` and
  the returned `spool:N` ID).

## Lifecycle

- Sessions are session state: they survive between agent turns and are all
  killed when spettro exits (TUI quit or headless run end).
- The status bar shows `▣ N pty` while sessions are live.
- While a pty tool call is running, its transcript entry shows a live tail
  of the session's settled scrollback (last few lines); the ctrl+g
  full-output toggle lifts the cap to the whole scrollback.

## Platform support

Unix only (Linux/macOS, via [creack/pty](https://github.com/creack/pty)).
On Windows the tools return "unsupported on this platform"; ConPTY support
is a planned follow-up.
