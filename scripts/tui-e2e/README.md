# TUI end-to-end rendering check

The unit tests in `internal/tui/render_fit_test.go` render frames with the
model's own `View()`. This harness checks the other half: what the real
binary puts on a real terminal, through Bubble Tea's renderer, at several
sizes, while a model makes the largest and most hostile tool calls the TUI
has to draw.

```sh
pip install pyte            # a terminal emulator in Python; or use a virtualenv
scripts/tui-e2e/run.sh      # prints one summary line per size and the output directory
```

`run.sh` builds `spettro`, then for 40x15, 80x24, 120x40 and 200x50 runs
`drive.py`, which:

- starts `fake_openai.py`, an OpenAI-compatible server on localhost that
  streams a scripted conversation (no API key, no network, no credit);
- runs the binary in a pseudo-terminal of that size, with `HOME` isolated in
  the output directory, the fake server as a local endpoint, and `ask-first`
  permission so every approval dialog appears;
- submits a prompt, pages (`PgDn`) and expands (`Ctrl+O`) the first
  approval's preview, opens its full review (`v`, paged, `Esc` back),
  approves every call with "Allow once" (selected explicitly: when part of a
  call is hidden the dialog preselects "Review full …", so `Enter` alone
  would open the review instead), answers the ask-user form, then
  toggles tool details (`Ctrl+O`), full output (`Ctrl+G`), pages the
  transcript (`PgUp`), toggles the side panel (`Ctrl+B`), opens the slash
  menu and the `@` palette, and resizes the terminal three times;
- saves the screen after each step as `NN-name.txt` and checks that the
  header, the input box's bottom border and the status bar are on the first,
  second-last and last rows: a frame taller or wider than the terminal
  scrolls or wraps, which moves exactly those rows. The review screen gets
  its own check: its title on the first row, its key hints on the last.

The scripted conversation (see `SCRIPT` in `fake_openai.py`): a 2000-line
heredoc, a file-write of a whole generated file under a very deep path, a
command whose output has a 10k-character line, tabs, colour escapes, a
carriage-return progress meter and 400 lines, a file-edit with a
3000-character replacement, a ten-item todo list, an ask-user form with long
texts, and a final answer with an over-wide code block and table.

The findings for each size are in `report.json`; the exit status is non-zero
when any size has one. Read the snapshots too: the structural check catches
overflow, not a label that reads badly.

`pyte` does not implement a few sequences Bubble Tea's renderer uses (scroll
up/down, back tab) and mishandles inserted or deleted blank lines; `drive.py`
adds them, since otherwise the emulated screen drifts from a real terminal
and shows corruption that is not there. Set `E2E_RAW=file` to record the raw
output stream when investigating a snapshot.
