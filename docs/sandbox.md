# OS Sandboxing

Spettro can lock down the commands the agent runs using the operating system's native sandbox mechanisms: **Seatbelt** on macOS and **Landlock** on Linux. It is opt-in, immutable for the lifetime of the session, and invisible to the model. The policy is set only by the operator through CLI flags or `spettro.agents.toml`.

## Why sandboxing matters

A coding agent executes shell commands, writes files, and sometimes makes network requests. Sandboxing limits the blast radius when the model produces an unexpected command:

- **File-system damage** is contained: a read-only policy prevents modifying project or user files; a workspace-write policy confines writes to the workspace plus explicitly allowed roots.
- **Data exfiltration** is harder: network policies can deny all outbound traffic, restrict it to loopback, or allow only specific TCP ports.
- **Credential exposure** is reduced: the home directory is read-blocked, so other projects, `~/.ssh`, and `~/.spettro` keys stay out of reach unless explicitly allowed.
- **Defense in depth** is added: even if the model tricks an approval gate, the kernel still enforces the policy.

> **Note:** sandboxing complements, but never replaces, the existing approval gates and permission rules. Enable it when you run an agent on sensitive code or in autonomous `/goal` mode.

## What is confined

| Layer | Confined by | Coverage |
|-------|-------------|----------|
| Shell commands | OS kernel | Every command spawned through `shell-exec`/`bash` is wrapped. |
| File tools | In-process check | `file-write`/`file-edit` honor the same write scope, so `read-only` cannot be bypassed by writing through a file tool instead of a shell redirect. |
| Spettro itself | OS kernel (write-only backstop) | The parent process cannot write outside the workspace, config/project directories, and temp dirs. Reads and network stay open so the app can talk to the LLM API and read skills. |

The model **cannot** inspect or change the policy. There is no sandbox tool exposed, no prompt hint, and blocked operations surface as ordinary command failures.

## Activation precedence

Settings are merged at startup with the following precedence:

```text
CLI flags > spettro.agents.toml > disabled
```

An explicit `--sandbox off` on the command line beats a manifest that enables it.

## Modes

### Filesystem modes (`--sandbox`)

| Mode | Effect |
|------|--------|
| `off` / `full-access` | No filesystem confinement (default). |
| `workspace-write` | Writes are allowed in the workspace, temp dirs, `/dev`, and any extra writable roots. Reads are confined to system paths plus the workspace and allowed roots. |
| `read-only` | Workspace writes are blocked; only temp dirs, `/dev`, and extra writable roots remain writable. Reads are confined as in `workspace-write`. |

### Network modes (`--sandbox-net`)

| Policy | Effect |
|--------|--------|
| `all` | No network confinement (default). |
| `localhost` | Loopback traffic only. **Linux caveat:** Landlock cannot scope rules to loopback, so this degrades to deny-all TCP (fail-closed). |
| `none` | Denies all network access the platform mechanism can govern. The Spettro process still reaches the LLM API, because parent traffic is never sandboxed. |
| `ports:443,8080` | Allows TCP on the listed ports only, any host. DNS may need a local socket on macOS; on Linux UDP/unix sockets are not covered. |

## Configuration

### Command-line flags

```bash
spettro --sandbox read-only --sandbox-net none
spettro --sandbox workspace-write --sandbox-allow-dir /data --sandbox-allow-read-dir ~/go/pkg/mod
spettro --sandbox-net ports:443,8080
```

- `--sandbox <mode>`: set filesystem policy.
- `--sandbox-net <policy>`: set network policy.
- `--sandbox-allow-dir <dir>`: extra writable root (repeatable).
- `--sandbox-allow-read-dir <dir>`: extra readable root, useful for tool-chain caches outside the workspace (repeatable).

### Agent manifest

```toml
[runtime]
sandbox_mode = "workspace-write"      # off | read-only | workspace-write | full-access
sandbox_net = "none"                  # all | localhost | none | ports:443,8080
sandbox_allow_dirs = ["/data"]
sandbox_allow_read_dirs = ["~/go/pkg/mod"]
```

See [`AGENTS.md`](../AGENTS.md) for the full manifest schema.

## Platform details

### macOS (Seatbelt / `sandbox-exec`)

- Uses a generated SBPL profile applied through `sandbox-exec`.
- Filesystem and network filters are enforced directly in the child.
- Network filters are IP/port based; hostnames cannot be allow-listed.
- `(deny network*)` also blocks unix sockets, so under `NetNone` DNS via `mDNSResponder` is blocked too.
- The parent process re-execs itself under `sandbox-exec` once for write-confinement; `SPETTRO_SANDBOX_PARENT=1` prevents an exec loop.

### Linux (Landlock)

- Uses the kernel's Landlock LSM.
- Filesystem confinement requires Linux 5.13+ (ABI v1+).
- Network confinement requires Linux 6.7+ (ABI v4+) and governs TCP connect/bind by port only.
- UDP, MPTCP, and unix sockets are not covered.
- `localhost` degrades to deny-all TCP because Landlock cannot scope rules to loopback.
- Unlike namespace tools such as `bwrap`, Landlock needs no privileges or user namespaces, so it works in locked-down containers.
- On Linux, sandboxed commands re-exec the Spettro binary itself, which applies Landlock and then `exec()`s the real command. Programs using this package must call `sandbox.RunChildIfRequested()` as the very first thing in `main()`.
- If the kernel cannot enforce the requested policy, the child exits **126** so an opt-in sandbox never silently runs unconfined.

### Other platforms

Sandboxing is not implemented on Windows or other Unixes. `sandbox.Available()` returns `false` and commands run unconfined.

## How it looks in the UI

When a sandbox policy is active, the TUI header shows a compact tag such as:

```text
sandbox:ws+net:none
sandbox:ro
sandbox:ws+net:443,8080
```

The tag is purely informational and is hidden when the sandbox is disabled.

## Examples

### Read-only review session

```bash
spettro --sandbox read-only --sandbox-net none
```

The agent can read the project and run read-only tooling, but it cannot modify files or make network requests.

### Safe autonomous run

```bash
spettro --goal "refactor the auth package" --sandbox workspace-write --sandbox-net ports:443
```

The agent can write in the workspace and reach the LLM API, but it cannot read other projects or exfiltrate data to arbitrary hosts.

### Build with external cache

```bash
spettro --sandbox read-only --sandbox-allow-read-dir ~/go/pkg/mod
```

A read-only policy blocks workspace writes, yet the Go module cache under `$HOME` is readable so builds can resolve dependencies.

## Important caveats

- **LLM API traffic is never sandboxed.** The Spettro parent process must reach the model provider, so `NetNone` still lets the agent call the API.
- **Reads are confined.** System paths stay readable so binaries and libraries load, but the rest of the home tree is blocked. Add toolchain caches with `--sandbox-allow-read-dir` when needed.
- **`read-only` still allows temp writes.** Temp dirs and `/dev` remain writable so ordinary commands (`>/dev/null`, compiler scratch files) keep working. The guarantee is that the agent cannot modify project or user files.
- **Failure is closed.** On Linux an unenforceable policy causes the child to exit 126, so you will notice if the sandbox could not be applied.

## Further reading

- [`AGENTS.md`](../AGENTS.md) — agent manifest schema, including `sandbox_mode`, `sandbox_net`, `sandbox_allow_dirs`, and `sandbox_allow_read_dirs`.
- [`docs/configuration.md`](configuration.md) — general configuration and storage.
- [`docs/goal.md`](goal.md) — autonomous `/goal` runs, where sandboxing is especially useful.
- [`internal/sandbox/`](../internal/sandbox) — source code for the sandbox implementation.
