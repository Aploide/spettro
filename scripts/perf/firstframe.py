#!/usr/bin/env python3
"""Interleaved TUI first-frame timing for two or more spettro binaries.

usage: firstframe.py [--n N] [--warmup W] [--cwd DIR] [--home-template DIR]
                     [--marker REGEX] [--cols C] [--rows R] LABEL=BINARY ...

Each iteration starts every binary once (the order rotates every
iteration) in a pseudo-terminal of COLS x ROWS and measures the time from
the fork to the first output whose text, with escape sequences removed,
matches MARKER. Every binary gets its own copy of the home template in a
temporary directory, made once and reused by all its runs: HOME,
USERPROFILE and the XDG directories all point into it, so the binaries
never read or write the real ~/.spettro, a file one build migrates (keys.enc
v1 to v2, say) never reaches the other build, and the one-time migration
happens during the warm-up runs instead of in every measured one. The cwd is
added to the copy's trusted.json so no trust prompt covers the frame.

Without --home-template the home starts empty (first launch, onboarding).
For a keyed launch, prepare a template once: run spettro with HOME set to a
scratch directory, enter a dummy key for an OpenAI-compatible endpoint, quit,
and pass that directory. Never pass your real home.

The terminal queries Bubble Tea sends at start (device attributes,
background colour, cursor position) are answered, as a real terminal would.
Prints min / median / p95 per label and the 1-minute load average before
and after, because on a shared machine a single run proves nothing.
"""
import argparse, fcntl, json, os, pty, re, select, shutil, signal, statistics, struct, sys, tempfile, termios, time

ANSI = re.compile(rb"\x1b\[[0-9;?<>=]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(\x07|\x1b\\)|\x1bP[^\x1b]*\x1b\\|\x1b[@-_]")
# Variables that locate per-user files; all of them move into the fresh home.
HOME_VARS = {"HOME": "", "USERPROFILE": "", "XDG_CONFIG_HOME": ".config", "XDG_CACHE_HOME": ".cache",
             "XDG_DATA_HOME": ".local/share", "XDG_STATE_HOME": ".local/state"}
# Variables that would change what the binary does at start.
DROP = ("HTTPS_PROXY", "HTTP_PROXY", "https_proxy", "http_proxy", "ALL_PROXY", "NO_PROXY", "no_proxy",
        "SPETTRO_MASTER_KEY", "SUDO_USER")


def fresh_home(template, cwd):
    home = os.path.realpath(tempfile.mkdtemp(prefix="spettro-perf-home-"))
    if template:
        shutil.copytree(template, home, dirs_exist_ok=True)
    state = os.path.join(home, ".spettro")
    os.makedirs(state, exist_ok=True)
    trusted = os.path.join(state, "trusted.json")
    dirs = json.load(open(trusted)) if os.path.exists(trusted) else []
    if cwd not in dirs:
        dirs.append(cwd)
    json.dump(dirs, open(trusted, "w"))
    return home


def child_env(home):
    env = {k: v for k, v in os.environ.items() if k not in DROP and not k.startswith("CLAUDE")}
    for name, rel in HOME_VARS.items():
        env[name] = os.path.join(home, rel) if rel else home
    env.update(TERM="xterm-256color", COLORTERM="truecolor")
    return env


def first_frame(binary, env, cwd, marker, cols, rows, timeout=20.0):
    """Milliseconds from fork to the marker, or None on timeout or exit."""
    t0 = time.perf_counter()
    pid, fd = pty.fork()
    if pid == 0:
        os.chdir(cwd)
        os.execve(binary, [binary], env)
    fcntl.ioctl(fd, termios.TIOCSWINSZ, struct.pack("HHHH", rows, cols, 0, 0))
    buf, hit = b"", None
    while time.perf_counter() - t0 < timeout:
        r, _, _ = select.select([fd], [], [], 0.02)
        if not r:
            continue
        try:
            data = os.read(fd, 65536)
        except OSError:
            break
        if not data:
            break
        buf += data
        if b"\x1b[c" in data or b"\x1b[0c" in data:
            os.write(fd, b"\x1b[?62;22c")
        if b"\x1b]11;?" in data:
            os.write(fd, b"\x1b]11;rgb:0000/0000/0000\x1b\\")
        if b"\x1b[6n" in data:
            os.write(fd, b"\x1b[1;1R")
        if marker.search(ANSI.sub(b"", buf)):
            hit = (time.perf_counter() - t0) * 1000
            break
    try:
        os.kill(pid, signal.SIGTERM)
    except ProcessLookupError:
        pass
    for _ in range(80):
        if os.waitpid(pid, os.WNOHANG)[0]:
            break
        try:
            os.read(fd, 65536)
        except OSError:
            pass
        time.sleep(0.025)
    else:
        os.kill(pid, signal.SIGKILL)
        os.waitpid(pid, 0)
    os.close(fd)
    return hit


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("binaries", nargs="+", metavar="LABEL=BINARY")
    ap.add_argument("--n", type=int, default=15)
    ap.add_argument("--warmup", type=int, default=2)
    ap.add_argument("--cwd", default=os.getcwd())
    ap.add_argument("--home-template")
    ap.add_argument("--marker", default="ask-first", help="regex the first complete frame contains")
    ap.add_argument("--cols", type=int, default=120)
    ap.add_argument("--rows", type=int, default=40)
    a = ap.parse_args()
    if a.home_template and os.path.realpath(a.home_template) == os.path.realpath(os.path.expanduser("~")):
        sys.exit("refusing to use the real home as the template")
    bins = [(s.split("=", 1)[0], os.path.abspath(s.split("=", 1)[1])) for s in a.binaries]
    cwd = os.path.realpath(a.cwd)
    marker = re.compile(a.marker.encode())
    res = {label: [] for label, _ in bins}
    homes = {label: fresh_home(a.home_template, cwd) for label, _ in bins}
    la0 = os.getloadavg()[0]
    try:
        for i in range(a.warmup + a.n):
            k = i % len(bins)
            for label, binary in bins[k:] + bins[:k]:
                ms = first_frame(binary, child_env(homes[label]), cwd, marker, a.cols, a.rows)
                if i >= a.warmup:
                    res[label].append(ms)
    finally:
        for home in homes.values():
            shutil.rmtree(home, ignore_errors=True)
    la1 = os.getloadavg()[0]
    for label, _ in bins:
        xs = sorted(x for x in res[label] if x is not None)
        missed = len(res[label]) - len(xs)
        if not xs:
            print(f"{label:16s} no frame matched {a.marker!r}")
            continue
        p95 = xs[min(len(xs) - 1, int(0.95 * len(xs)))]
        print(f"{label:16s} n={len(xs)} min={xs[0]:.1f} med={statistics.median(xs):.1f} p95={p95:.1f} ms"
              + (f" missed={missed}" if missed else ""))
    print(f"load (1m) {la0:.1f} -> {la1:.1f}")


if __name__ == "__main__":
    main()
