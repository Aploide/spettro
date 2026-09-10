# Themes (dark, light, auto)

Spettro's TUI was drawn for a dark terminal. On a light-background terminal the
same palette prints pale grey text on white and paints black slabs behind code
blocks, which is unreadable rather than merely ugly. The **theme** setting fixes
that: pick a palette, or let Spettro detect one.

Three selections ship, and no more:

| Selection | Meaning |
| --- | --- |
| `dark` | The palette Spettro has always rendered, unchanged. |
| `light` | A re-tuned palette for a light terminal: dark ink, pale surfaces. |
| `auto` | Detect the terminal's background and resolve to `dark` or `light`. **Default.** |

The scope is the palette and nothing else. Layout, glyphs, spacing and every
rendering decision are identical in both themes, and dark mode is byte-for-byte
what shipped before themes existed — the dark theme is the status quo given
names, not a redesign.

## Choosing a theme

```
/theme              # open the picker: preview each palette, enter applies
/theme dark         # switch and persist
/theme light
/theme auto
```

`/theme <value>` repaints immediately — no restart — and writes the choice to
`~/.spettro/config.json`. Anything other than `dark`, `light` or `auto` is
refused with the valid values listed; nothing is saved and the palette does not
move.

A bare `/theme` opens the picker. It lists the three options with a panel
previewing the one under the cursor — the header band, prose, a selected row, a
code block and a diff, drawn in the candidate palette on its own background, so
a dark theme previews as dark even on a light terminal. `↑↓` moves, `1`-`3`
pick directly, `enter` applies and persists, `esc` closes without changing
anything.

It also reports three separate facts into the transcript, because in `auto` the selection
alone tells you nothing about what is on screen:

```
theme: auto (rendering: light)
selected by: default — neither SPETTRO_THEME nor the "theme" config key is set
resolved by: the terminal's reported background colour

usage: /theme <dark|light|auto>
```

`resolved by:` names the terminal's own reply only when a reply actually
arrived and could be decoded; a malformed one falls through to `COLORFGBG` or
the dark fallback and says so.

## Precedence

Highest wins:

1. **`SPETTRO_THEME`** — `dark`, `light` or `auto`. Selects the palette at
   startup, ahead of both the config and detection, and is never written to
   disk. This is the escape hatch for a terminal that lies about its
   background, and for scripted runs that want a predictable palette:

   ```bash
   SPETTRO_THEME=light spettro
   ```

   It is a startup override, not a lock. `/theme` still repaints and still
   saves while the variable is set — a user staring at an unreadable terminal
   has to be able to fix it without restarting — and says so. What the variable
   guarantees is that the *next* start with it set begins on that palette
   again, whatever was saved in between.

   An unrecognised value is ignored rather than coerced, and the selection
   falls through to the config; bare `/theme` reports the value that was
   skipped alongside the source that actually decided.

2. **`theme` in `~/.spettro/config.json`** — what `/theme` writes. An
   unrecognised value is cleared to the default on the next load rather than
   silently selecting a palette nobody asked for.

3. **Auto-detection** — see below.

4. **Dark**, the fallback. Dark is the safe default in both directions: light
   ink on a light terminal is unreadable, whereas the dark palette on a light
   terminal is merely unpleasant.

## How `auto` detects

Detection runs in two stages, because the accurate answer is also the slow one.

**At startup**, before the first frame, Spettro reads `COLORFGBG` — a pure
environment read, no I/O — so the first paint is usually already right.
rxvt, urxvt and Konsole set it to `fg;bg` (some rxvt builds to
`fg;default;bg`); background indices 0-6 and 8 are dark, 7 and 9-15 light.
It is only a hint: the variable is inherited by child processes, so it goes
stale across a `tmux attach` from a different terminal, or a theme switch inside
the same session.

**Then** Spettro asks the terminal itself, with an OSC 11 background query, and
revises the palette when the answer arrives. Most modern terminals answer;
those that do not simply cost a wasted round trip. The reply is classified by
HSL lightness below 50%, matching Bubble Tea and Lip Gloss exactly, so a colour
Spettro calls dark and a colour the libraries call dark can never disagree.

The query is skipped entirely unless stdin *and* stdout are both character
devices and `TERM` is set to something other than `dumb`. Bubble Tea writes
escape sequences whether or not the far end can answer, so an unguarded query
would land as literal `\x1b]11;?` bytes in a redirected transcript. Piped
output, a test binary, and a dumb terminal therefore all get dark.

An explicit `dark` or `light` never puts a query on the wire, and a terminal
that reports its background unsolicited mid-session cannot flip a palette you
chose by hand — only `auto` stays open to being revised.

## What the light theme changes

Every colour in the UI resolves to one of about forty semantic roles
(`Text`, `Border`, `Success`, `BgCode`, the agent accents, the animation
ramps…), defined once in `internal/theme`. The light theme re-tunes each role
rather than inverting anything mechanically:

- **Contrast.** Every role that carries prose clears WCAG AA (4.5:1) and the
  roles that are only ever drawn clear 3:1, measured against `#F5F5F5` rather
  than pure white — an off-white terminal has less headroom, so it is the
  harder of the two grounds. Two roles that share a hex in dark are split for
  this: `Rule` takes over the separators, fills and empty progress cells that
  `TextDim` used to draw, so `TextDim` can be darkened to AA for the
  thinking-block prose, memory facts and session previews it carries without
  turning every hairline into something as heavy as body text.
  The animations are the documented exemptions — they fade to invisibility on
  purpose.
- **Surfaces flip direction.** The header band, selection highlight, code block
  and inline-code chip are painted *darker than white* instead of darker than
  black. The intra-line diff highlights become pale mint and pink washes instead
  of deep green and red boxes.
- **Two roles invert rather than shift.** The active agent tab's label is
  near-black on a bright accent in dark and white on a dark accent in light. The
  shimmer effects (the glare sweep over running work, the `ultracode` glow) lerp
  toward white in dark and toward ink in light — the peak of a sweep is the cell
  *furthest from the page*, and on a white page furthest means darker.
- **Agent accents stay recognisable.** Each manifest colour keeps its hue at a
  darker lightness. Two pairs collapse in light mode (the `green` accent onto
  `success`, `yellow` onto `warning`) because holding two dark greens apart at
  AA on white produces two colours nobody can tell apart.
- **Animations fade the right way.** The eye art's scan-line and blink ramps
  fade toward the terminal's own ground, so a "faded" row disappears into the
  page in both themes instead of toward a hardcoded near-black. Each light step
  is tuned to the contrast its dark counterpart has against black, so the
  animation reads at the same strength on either ground rather than washing out
  on the light one.

## Notes

- The theme is process-global and read at render time, so `/theme` repaints
  already-rendered transcript blocks too; the message render cache is dropped on
  every switch.
- Headless and [ACP](acp.md) runs never construct a TUI, so no background query
  can reach their stdio JSON-RPC streams.
- The light palette assumes a truecolor terminal, which the TUI already required.
