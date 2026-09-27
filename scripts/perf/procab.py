#!/usr/bin/env python3
"""Interleaved wall time, peak RSS and exit code of short commands.

usage: procab.py [--n N] [--warmup W] LABEL='command args' [LABEL='command args' ...]

Each iteration runs every command once, the order rotating every iteration,
and times it around the process start and exit. Peak RSS and the exit code
come from a separate pass of five runs under /usr/bin/time (-l on macOS, -v
on Linux), so the wrapper does not inflate the wall numbers.

The commands run with HOME, USERPROFILE and the XDG directories in an empty
temporary directory, so a spettro binary under test never reads or writes
the real ~/.spettro. Prints min / median / p95 per label and the 1-minute
load average.

Example, comparing two builds:
  scripts/perf/procab.py base='/tmp/spettro-base --help' now='bin/spettro --help'
"""
import argparse, os, platform, re, shlex, shutil, statistics, subprocess, tempfile, time

HOME_VARS = {"HOME": "", "USERPROFILE": "", "XDG_CONFIG_HOME": ".config", "XDG_CACHE_HOME": ".cache",
             "XDG_DATA_HOME": ".local/share", "XDG_STATE_HOME": ".local/state"}


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("commands", nargs="+", metavar="LABEL=COMMAND")
    ap.add_argument("--n", type=int, default=30)
    ap.add_argument("--warmup", type=int, default=3)
    a = ap.parse_args()
    cmds = [(s.split("=", 1)[0], shlex.split(s.split("=", 1)[1])) for s in a.commands]
    home = os.path.realpath(tempfile.mkdtemp(prefix="spettro-perf-home-"))
    env = {k: v for k, v in os.environ.items() if not k.startswith("CLAUDE") and k != "SUDO_USER"}
    for name, rel in HOME_VARS.items():
        env[name] = os.path.join(home, rel) if rel else home
    try:
        report(cmds, env, a.n, a.warmup)
    finally:
        shutil.rmtree(home, ignore_errors=True)


def run(cmd, env):
    t = time.perf_counter()
    r = subprocess.run(cmd, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, stdin=subprocess.DEVNULL, env=env)
    return (time.perf_counter() - t) * 1000, r.returncode


def peak_rss_mb(cmd, env):
    """Median peak RSS over five runs, in MB, or None without /usr/bin/time."""
    darwin = platform.system() == "Darwin"
    flag = "-l" if darwin else "-v"
    pat = re.compile(r"(\d+)\s+maximum resident set size" if darwin else r"Maximum resident set size \(kbytes\): (\d+)")
    unit = 1048576 if darwin else 1024
    rss = []
    for _ in range(5):
        try:
            r = subprocess.run(["/usr/bin/time", flag] + cmd, stdout=subprocess.DEVNULL, stderr=subprocess.PIPE,
                               stdin=subprocess.DEVNULL, env=env, text=True)
        except FileNotFoundError:
            return None
        m = pat.search(r.stderr)
        if m:
            rss.append(int(m.group(1)) / unit)
    return statistics.median(rss) if rss else None


def report(cmds, env, n, warmup):
    res = {label: [] for label, _ in cmds}
    codes = {label: set() for label, _ in cmds}
    la0 = os.getloadavg()[0]
    for i in range(n + warmup):
        k = i % len(cmds)
        for label, cmd in cmds[k:] + cmds[:k]:
            ms, rc = run(cmd, env)
            codes[label].add(rc)
            if i >= warmup:
                res[label].append(ms)
    la1 = os.getloadavg()[0]
    for label, cmd in cmds:
        xs = sorted(res[label])
        p95 = xs[min(len(xs) - 1, int(0.95 * len(xs)))]
        rss = peak_rss_mb(cmd, env)
        rss_s = f"{rss:.1f} MB" if rss is not None else "n/a"
        print(f"{label:16s} n={len(xs)} min={xs[0]:.1f} med={statistics.median(xs):.1f} p95={p95:.1f} ms  "
              f"rss={rss_s} exit={sorted(codes[label])}")
    print(f"load (1m) {la0:.1f} -> {la1:.1f}")


if __name__ == "__main__":
    main()
