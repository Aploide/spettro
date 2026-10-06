#!/usr/bin/env python3
"""Drive the real Spettro TUI in a pseudo-terminal against fake_openai.py.

The binary runs in a pty of the requested size with an isolated HOME (under
OUTDIR), talking to the fake server, in ask-first mode so every approval
dialog appears. A pyte screen emulates the terminal, so each snapshot is the
text a user would see at that size. The scenario: submit a prompt, page and
expand the first approval's preview, approve everything, answer the
ask-user form, then toggle tool details (ctrl+o), full output (ctrl+g),
page the transcript, toggle the side panel (ctrl+b), open the slash menu and
the @ palette, and resize the terminal three times.

Each snapshot is saved to OUTDIR/NN-name.txt and checked structurally: the
header on the first row, the input box's bottom border on the second-last
and the status bar on the last. A frame taller or wider than the terminal
scrolls or wraps, which breaks exactly those rows. The findings go to
OUTDIR/report.json and a one-line summary is printed.

Set E2E_RAW=path to also record the raw byte stream the TUI wrote.

Usage: drive.py BINARY OUTDIR WIDTH HEIGHT
"""
import fcntl
import json
import os
import pty
import secrets
import select
import signal
import struct
import subprocess
import sys
import termios
import threading
import time

import pyte

HERE = os.path.dirname(os.path.abspath(__file__))


class Screen(pyte.Screen):
    """pyte.Screen with the few terminal features Bubble Tea's renderer uses
    and pyte lacks or gets wrong. Without them the emulated screen drifts
    from what a real terminal shows, which reads as rendering corruption that
    is not there.
    """

    def _margins(self):
        return self.margins or pyte.screens.Margins(0, self.lines - 1)

    def scroll_up(self, count=None):
        """SU (CSI n S): scroll the scroll region up n rows."""
        count = count or 1
        top, bottom = self._margins()
        self.dirty.update(range(self.lines))
        for y in range(top, bottom + 1):
            src = y + count
            if src <= bottom and src in self.buffer:
                self.buffer[y] = self.buffer[src]
            else:
                self.buffer.pop(y, None)

    def scroll_down(self, count=None):
        """SD (CSI n T): scroll the scroll region down n rows."""
        count = count or 1
        top, bottom = self._margins()
        self.dirty.update(range(self.lines))
        for y in range(bottom, top - 1, -1):
            src = y - count
            if src >= top and src in self.buffer:
                self.buffer[y] = self.buffer[src]
            else:
                self.buffer.pop(y, None)

    def insert_lines(self, count=None):
        """IL: pyte's version leaves a stale row where a blank one moved."""
        count = count or 1
        top, bottom = self._margins()
        if top <= self.cursor.y <= bottom:
            self.dirty.update(range(self.lines))
            for y in range(bottom, self.cursor.y - 1, -1):
                src = y - count
                if src >= self.cursor.y and src in self.buffer:
                    self.buffer[y] = self.buffer[src]
                else:
                    self.buffer.pop(y, None)
            self.carriage_return()

    def delete_lines(self, count=None):
        """DL: same stale-row fix as insert_lines."""
        count = count or 1
        top, bottom = self._margins()
        if top <= self.cursor.y <= bottom:
            self.dirty.update(range(self.lines))
            for y in range(self.cursor.y, bottom + 1):
                src = y + count
                if src <= bottom and src in self.buffer:
                    self.buffer[y] = self.buffer[src]
                else:
                    self.buffer.pop(y, None)
            self.carriage_return()

    def back_tab(self, count=None):
        """CBT (CSI n Z): back to the previous tab stop, n times."""
        for _ in range(count or 1):
            stops = [s for s in self.tabstops if s < self.cursor.x]
            self.cursor.x = max(stops) if stops else 0


class ByteStream(pyte.ByteStream):
    csi = dict(pyte.ByteStream.csi, S="scroll_up", T="scroll_down", Z="back_tab")


class Term:
    """The TUI process in a pty, with its screen emulated by pyte."""

    def __init__(self, argv, env, cwd, width, height):
        self.width, self.height = width, height
        self.screen = Screen(width, height)
        self.stream = ByteStream(self.screen)
        self.lock = threading.Lock()
        self.raw = open(os.environ["E2E_RAW"], "wb") if os.environ.get("E2E_RAW") else None
        self.pid, self.fd = pty.fork()
        if self.pid == 0:
            os.chdir(cwd)
            os.execvpe(argv[0], argv, env)
        self._set_size(width, height)
        self.alive = True
        threading.Thread(target=self._reader, daemon=True).start()

    def _set_size(self, width, height):
        fcntl.ioctl(self.fd, termios.TIOCSWINSZ, struct.pack("HHHH", height, width, 0, 0))

    def resize(self, width, height):
        with self.lock:
            self.width, self.height = width, height
            self.screen.resize(height, width)
        self._set_size(width, height)
        os.kill(self.pid, signal.SIGWINCH)

    def _reader(self):
        while self.alive:
            ready, _, _ = select.select([self.fd], [], [], 0.1)
            if not ready:
                continue
            try:
                data = os.read(self.fd, 65536)
            except OSError:
                break
            if not data:
                break
            with self.lock:
                self.stream.feed(data)
                if self.raw is not None:
                    self.raw.write(data)

    def send(self, keys, pause=0.15):
        os.write(self.fd, keys.encode())
        time.sleep(pause)

    def text(self):
        with self.lock:
            return "\n".join(line.rstrip() for line in self.screen.display)

    def wait_for(self, needle, timeout=30):
        deadline = time.time() + timeout
        while time.time() < deadline:
            if needle in self.text():
                time.sleep(0.4)  # let the frame settle
                return True
            time.sleep(0.1)
        return False

    def close(self):
        self.alive = False
        try:
            os.kill(self.pid, signal.SIGKILL)
        except ProcessLookupError:
            pass


def layout_problems(text, height):
    """Structural checks every main-screen frame that fits the terminal passes."""
    rows = text.split("\n")
    problems = []
    if not rows[0].startswith("◈ spettro"):
        problems.append(f"row 0 is not the header: {rows[0]!r}")
    if "ctx" not in rows[-1]:
        problems.append(f"last row is not the status bar: {rows[-1]!r}")
    if not rows[-2].startswith("└"):
        problems.append(f"row {height - 2} is not the input box's bottom border: {rows[-2]!r}")
    return problems


def is_idle(text):
    """The run is over: the empty input shows its placeholder and the
    working indicator ("… (0m 05s · ↓ 1.1k tokens)") is gone."""
    return "enter message" in text and "tokens)" not in text


def setup(outdir):
    """Isolated HOME and work tree, and the fake server; returns (home, work, server)."""
    home = os.path.realpath(os.path.join(outdir, "home"))
    work = os.path.realpath(os.path.join(outdir, "work"))
    os.makedirs(os.path.join(home, ".spettro"), exist_ok=True)
    os.makedirs(work, exist_ok=True)
    subprocess.run(["git", "init", "-q", work], check=True)
    with open(os.path.join(work, "README.md"), "w") as f:
        f.write("# demo\n")

    port_file = os.path.join(outdir, "port")
    if os.path.exists(port_file):
        os.remove(port_file)
    server = subprocess.Popen([sys.executable, os.path.join(HERE, "fake_openai.py"), port_file,
                               os.path.join(outdir, "server.log")])
    for _ in range(100):
        if os.path.exists(port_file) and open(port_file).read():
            break
        time.sleep(0.05)
    base = "http://127.0.0.1:" + open(port_file).read()

    # A local endpoint needs no API key; ask-first makes every approval
    # dialog appear; trusting the work tree skips the first-run dialog.
    with open(os.path.join(home, ".spettro", "config.json"), "w") as f:
        json.dump({"active_provider": base, "active_model": "fake-model", "local_endpoints": [base],
                   "permission": "ask-first", "last_agent_id": "coding",
                   "notifications_disabled": True, "checkpointing_disabled": True}, f)
    with open(os.path.join(home, ".spettro", "trusted.json"), "w") as f:
        json.dump([work], f)
    return home, work, server


def main():
    binary, outdir, width, height = sys.argv[1], sys.argv[2], int(sys.argv[3]), int(sys.argv[4])
    os.makedirs(outdir, exist_ok=True)
    home, work, server = setup(outdir)
    env = {"HOME": home, "PATH": os.environ["PATH"], "TERM": "xterm-256color", "LANG": "en_US.UTF-8",
           "COLORTERM": "truecolor",
           # Keeps the key store off the system keychain.
           "SPETTRO_MASTER_KEY": secrets.token_hex(32)}
    term = Term([binary], env, work, width, height)
    report = {"size": f"{width}x{height}", "snapshots": [], "problems": []}

    def shot(name):
        text = term.text()
        path = os.path.join(outdir, f"{len(report['snapshots']):02d}-{name}.txt")
        with open(path, "w") as f:
            f.write(text + "\n")
        problems = layout_problems(text, term.height)
        report["snapshots"].append({"name": name, "file": path, "problems": problems})
        report["problems"].extend(f"{name}: {p}" for p in problems)

    def need(needle, name, timeout=30):
        if term.wait_for(needle, timeout):
            return True
        shot(name + "-TIMEOUT")
        report["problems"].append(f"{name}: never saw {needle!r}")
        return False

    try:
        need("ctx", "startup")
        time.sleep(1.5)
        shot("startup")
        term.send("write the files")
        term.send("\r", 1.0)

        # The first approval: a 2000-line heredoc. Page and expand its preview.
        if need("allow this command?", "approval-heredoc"):
            shot("approval-heredoc")
            for _ in range(3):
                term.send("\x1b[6~")  # pgdn
            shot("approval-heredoc-scrolled")
            term.send("\x0f", 0.4)  # ctrl+o
            shot("approval-heredoc-expanded")
            term.send("\x0f", 0.4)
            term.send("\r", 1.0)  # allow once

        # Approve every further call and answer the question, until idle.
        deadline = time.time() + 60
        approvals = 1
        while time.time() < deadline and not is_idle(term.text()):
            text = term.text()
            if "enter answers" in text or "enter records" in text:
                shot("ask-user")
                term.send("\r", 1.0)
            elif "allow this command?" in text:
                approvals += 1
                shot(f"approval-{approvals}")
                term.send("\r", 1.0)
            else:
                time.sleep(0.2)
        if not is_idle(term.text()):
            shot("run-TIMEOUT")
            report["problems"].append("the run never finished")
        time.sleep(1.0)

        shot("done-collapsed")
        term.send("\x0f", 0.6)  # ctrl+o: tool details
        shot("done-details")
        term.send("\x07", 0.6)  # ctrl+g: full output
        shot("done-full-output")
        for _ in range(8):
            term.send("\x1b[5~", 0.1)  # pgup through the transcript
        shot("done-full-output-paged-up")
        term.send("\x07", 0.4)
        term.send("\x0f", 0.4)
        term.send("\x02", 0.8)  # ctrl+b: the side panel
        shot("side-panel-toggled")
        term.send("\x02", 0.8)
        term.send("/", 0.6)
        shot("slash-menu")
        term.send("\x1b", 0.4)
        term.send("\x15", 0.3)  # ctrl+u clears the line
        term.send("@", 0.8)
        shot("mention-palette")
        term.send("\x1b", 0.4)
        for w, h in [(40, 15), (80, 24), (150, 45)]:
            term.resize(w, h)
            time.sleep(1.2)
            shot(f"resized-{w}x{h}")
    finally:
        term.close()
        server.terminate()
        with open(os.path.join(outdir, "report.json"), "w") as f:
            json.dump(report, f, indent=2)
    print(json.dumps({"size": report["size"], "snapshots": len(report["snapshots"]),
                      "problems": report["problems"]}))
    sys.exit(1 if report["problems"] else 0)


if __name__ == "__main__":
    main()
