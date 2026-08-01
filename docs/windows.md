# Windows

Spettro runs natively on Windows (amd64 and arm64). Everything documented
elsewhere works the same way unless it is listed here.

## Install

```powershell
irm https://raw.githubusercontent.com/aploide/spettro/main/install.ps1 | iex
```

The installer verifies the release archive's SHA-256 against the release's
`checksums.txt` and refuses to install if it does not match. It writes to
`%LOCALAPPDATA%\Programs\spettro`, which needs no elevation — so the in-place
self-update (`/update`) does not need it either.

| Flag | Effect |
| --- | --- |
| `-Version v1.2.3` | Install a specific release instead of the latest. |
| `-InstallDir <path>` | Install somewhere else. |
| `-NoPathUpdate` | Do not touch the user `PATH`. |
| `-BaseUrl <url>` | Fetch the release assets from a mirror. |

Re-running the installer over a **running** spettro works: Windows will not let
an executing image be overwritten, but it does allow it to be renamed, so the
current build is moved aside and deleted on the next run. `/update` uses the
same mechanism.

## The shell the agent drives

There is no `bash` in a default Windows install, so agent commands, runtime
hooks and custom-command interpolation run under **PowerShell** — `pwsh` when it
is on `PATH`, otherwise the `powershell.exe` that ships with Windows.

The active interpreter is stated in the agent's system prompt, so the model
writes PowerShell rather than POSIX pipelines. If you would rather use a POSIX
shell you already have (Git for Windows, MSYS2, WSL interop), point
`SPETTRO_SHELL` at it:

```powershell
$env:SPETTRO_SHELL = 'C:\Program Files\Git\bin\bash.exe'
```

Command lines are handed to PowerShell as an encoded blob, so quoting is exact —
no metacharacter in a model-authored command can reshape the argument vector.
Native exit codes are propagated (a failing `go build` reports `2`, not a generic
failure), output is forced to UTF-8, and diagnostics arrive as plain text rather
than the CLIXML document PowerShell writes to a redirected stderr by default.

## Sandboxing

Implemented with Mandatory Integrity Control. Writes are confined by the kernel;
**reads are not**, and **network policies are not supported** and fail closed
rather than running unconfined. `workspace-write` makes a persistent change to
the workspace directory's security descriptor. See
[`docs/sandbox.md`](sandbox.md#windows-mandatory-integrity-control) for the full
picture before relying on it.

## Notifications

Desktop notifications are delivered as Windows toasts. Because spettro is a
portable console binary with no installer to register an Application User Model
ID, the toast is published under the built-in Windows PowerShell AppID — so the
banner is attributed to PowerShell. Notifications must be enabled for
"Windows PowerShell" in **Settings → System → Notifications** for them to appear.

## Interactive PTY sessions

Not available. The `pty-*` tools report unsupported; see
[`docs/pty.md`](pty.md#platform-support). Long-running commands still work
through `shell-exec` with `run_in_background`.

## Where files live

`%USERPROFILE%\.spettro` holds the global config, credentials and session state,
exactly like `~/.spettro` elsewhere. If `HOME` is set to an absolute Windows path
it takes precedence, which is what lets tooling and tests relocate the store; the
POSIX-style `HOME` that Git Bash exports (`/c/Users/...`) is ignored.

Credentials are protected with an explicit DACL naming only your account, with
inheritance disabled — the Unix `0600`/`0700` modes have no effect on Windows, so
the guarantee is expressed directly instead.

## Building from source

```powershell
go build ./cmd/spettro
```

`make build` also works under Git Bash or MSYS2 and produces `bin/spettro.exe`.
