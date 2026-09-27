# Performance drivers

Small, dependency-free Python 3 drivers for comparing two Spettro builds on
the same machine. [docs/performance.md](../../docs/performance.md) explains
the method and lists every measured result; this file only covers how to run
the drivers.

Both drivers start every binary with `HOME`, `USERPROFILE` and the XDG
directories in a temporary directory, so a build under test never reads or
writes your real `~/.spettro` (config, keys, Telegram settings, sessions).
Keep it that way when you add a driver.

## Build the two binaries

Build both with the release flags, one from the commit you compare against
and one from your branch:

```sh
git worktree add /tmp/spettro-base <base-commit>
(cd /tmp/spettro-base && GOWORK=off go build -trimpath -ldflags='-s -w' -o /tmp/spettro-base.bin ./cmd/spettro)
GOWORK=off go build -trimpath -ldflags='-s -w' -o /tmp/spettro-now.bin ./cmd/spettro
```

## `procab.py`: process start, `--help`, `--version`

```sh
scripts/perf/procab.py --n 30 base='/tmp/spettro-base.bin --help' now='/tmp/spettro-now.bin --help'
```

Prints min / median / p95 wall time, the median peak RSS of five extra runs
under `/usr/bin/time`, the exit codes seen, and the load average.

## `firstframe.py`: TUI first frame

```sh
scripts/perf/firstframe.py --cwd ~/code/some-repo --home-template /tmp/keyed-home \
    base=/tmp/spettro-base.bin now=/tmp/spettro-now.bin
```

Times fork-to-first-complete-frame in a 120x40 pseudo-terminal. The frame
counts as complete when its text matches `--marker` (default `ask-first`,
the permission mode shown in the status bar once the frame is fully laid
out). Each binary gets its own copy of `--home-template`, reused by all
its runs so one-time migrations happen during the warm-ups; to make a keyed one,
run Spettro once with `HOME=/tmp/keyed-home`, enter a dummy key for an
OpenAI-compatible endpoint, and quit. Never pass your real home.

## Rules for a result you publish

- Interleave the builds (both drivers rotate the order every iteration) and
  use n >= 15 after at least two warm-ups. Compare medians.
- Record the load average next to the numbers. On a shared machine a single
  run proves nothing, and a row measured at load 15 is not comparable with
  one measured at load 1.
- Build both binaries the same way (`-trimpath -ldflags='-s -w'`).
