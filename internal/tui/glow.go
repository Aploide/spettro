package tui

import (
	"image/color"
	"math"
	"strings"

	"charm.land/lipgloss/v2"

	"spettro/internal/agent"
	"spettro/internal/theme"
)

// Asking for a workflow lights the phrase up in the input box. The point is
// not decoration: "ultracode" — or "use a workflow to …" — silently changes
// what the next turn can do, and a phrase that looks like every other phrase
// gives no sign of that. Lighting it exactly when, and only when, it will
// actually activate makes the input tell the truth about the mode the run is
// about to enter, which is why the match comes from
// agent.WorkflowActivationSpans rather than a local strings.Contains.

// ultracodeRamp is the colour the shimmer travels through: violet into
// magenta into cyan, looping. Sampled as a continuous gradient, not as
// discrete steps, so the word reads as one moving surface instead of a row of
// separately-coloured letters.
//
// It is derived from the palette on every frame rather than cached at package
// level, because a theme switch has to reach an animation that is already
// running; five stops is far less work than the render it feeds.
func ultracodeRamp(p theme.Palette) []rgbColor {
	ramp := make([]rgbColor, len(p.RampGlow))
	for i, c := range p.RampGlow {
		ramp[i] = toRGB(c)
	}
	return ramp
}

type rgbColor struct{ r, g, b float64 }

// toRGB drops a palette colour into the float space the glow interpolates in.
func toRGB(c color.Color) rgbColor {
	r, g, b := theme.RGB(c)
	return rgbColor{float64(r), float64(g), float64(b)}
}

func (c rgbColor) lerp(o rgbColor, t float64) rgbColor {
	return rgbColor{
		r: c.r + (o.r-c.r)*t,
		g: c.g + (o.g-c.g)*t,
		b: c.b + (o.b-c.b)*t,
	}
}

func (c rgbColor) hex() string {
	clamp := func(v float64) int {
		return min(max(int(v+0.5), 0), 255)
	}
	const digits = "0123456789ABCDEF"
	out := []byte("#000000")
	for i, v := range []int{clamp(c.r), clamp(c.g), clamp(c.b)} {
		out[1+i*2] = digits[v>>4]
		out[2+i*2] = digits[v&0xF]
	}
	return string(out)
}

// sampleRamp reads the ramp at a looping position in [0,1).
func sampleRamp(ramp []rgbColor, pos float64) rgbColor {
	n := float64(len(ramp))
	pos = pos - math.Floor(pos)
	scaled := pos * n
	i := int(scaled)
	return ramp[i%len(ramp)].lerp(ramp[(i+1)%len(ramp)], scaled-float64(i))
}

// Animation speeds, in frames at the TUI's 60 Hz tick. Both are deliberately
// unhurried: this sits under the cursor while someone is typing, and anything
// quick enough to notice as motion is quick enough to be a distraction.
const (
	// ultracodeDriftFrames is one full trip through the colour ramp.
	ultracodeDriftFrames = 420.0 // 7s
	// ultracodeSweepFrames is one pass of the specular highlight.
	ultracodeSweepFrames = 192.0 // 3.2s
	// ultracodeSweepPad is how far past each end the highlight travels, so
	// there is a beat of calm between passes instead of a strobe.
	ultracodeSweepPad = 7.0
	// ultracodeSweepWidth is the highlight's falloff radius in cells.
	ultracodeSweepWidth = 2.6
)

// ultracodeCellColor is the colour of one cell of the lit word: a slow drift
// along the ramp, a dim tint behind it, and a bright specular band sweeping
// across both. The background is what makes it read as a glowing object rather
// than as coloured text — a foreground-only effect disappears against the
// surrounding prose at a glance.
func ultracodeCellColor(i, n, frame int) (fg, bg string, shine float64) {
	p := theme.Current()
	width := float64(max(n, 1))
	drift := float64(frame) / ultracodeDriftFrames
	base := sampleRamp(ultracodeRamp(p), drift+float64(i)/(width*1.6))

	// The highlight travels across the word and a little way past both ends.
	span := width + ultracodeSweepPad*2
	phase := math.Mod(float64(frame)/ultracodeSweepFrames, 1.0)
	head := phase*span - ultracodeSweepPad
	dist := math.Abs(float64(i) - head)

	if dist < ultracodeSweepWidth {
		// Cosine falloff: a linear one leaves a visible hard edge on the band.
		shine = 0.5 * (1 + math.Cos(math.Pi*dist/ultracodeSweepWidth))
	}
	lit := base.lerp(toRGB(p.GlowSpecular), shine*p.GlowShine)

	// The tint stays close to the theme's own ground — barely off it — so the
	// word never loses legibility against it, and so the glow reads as a lit
	// object on the page rather than as a box pasted over it. On a light
	// terminal that means a pale wash, not a dark slab, which is why the
	// anchor and the weights come from the palette instead of being fixed
	// here.
	glowBG := toRGB(p.BgBase).lerp(base, p.GlowTintBias+shine*p.GlowTintGain)
	return lit.hex(), glowBG.hex(), shine
}

// renderUltracodeRunes styles each cell of the keyword. Cells are returned
// individually because the caller substitutes them into already-rendered text
// one at a time, skipping any the terminal is using for something else.
func renderUltracodeRunes(word []rune, frame int) []string {
	out := make([]string, len(word))
	for i, r := range word {
		fg, bg, _ := ultracodeCellColor(i, len(word), frame)
		out[i] = lipgloss.NewStyle().Bold(true).
			Foreground(lipgloss.Color(fg)).
			Background(lipgloss.Color(bg)).
			Render(string(r))
	}
	return out
}

// ansiToken is either an escape sequence or one visible rune, with the SGR
// state that was in effect when the rune was emitted.
type ansiToken struct {
	esc     string
	r       rune
	reverse bool
}

// tokenizeANSI splits already-rendered text into escape sequences and visible
// runes, tracking whether reverse video was active for each rune.
func tokenizeANSI(s string) []ansiToken {
	var out []ansiToken
	reverse := false
	for i := 0; i < len(s); {
		if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '[' {
			j := i + 2
			for j < len(s) && (s[j] < '@' || s[j] > '~') {
				j++
			}
			if j < len(s) {
				seq := s[i : j+1]
				if s[j] == 'm' {
					reverse = applyReverse(reverse, s[i+2:j])
				}
				out = append(out, ansiToken{esc: seq})
				i = j + 1
				continue
			}
		}
		r, size := decodeFirstRune(s[i:])
		out = append(out, ansiToken{r: r, reverse: reverse})
		i += size
	}
	return out
}

// applyReverse folds an SGR parameter list into the reverse-video flag. Only
// reverse is tracked: it is how the textarea draws its cursor, and a cursor
// cell is the one cell inside the word that must keep its own styling.
func applyReverse(cur bool, params string) bool {
	if params == "" {
		return false // bare CSI m is a full reset
	}
	for _, f := range strings.Split(params, ";") {
		switch f {
		case "", "0":
			cur = false
		case "7":
			cur = true
		case "27":
			cur = false
		}
	}
	return cur
}

func decodeFirstRune(s string) (rune, int) {
	for _, r := range s {
		return r, len(string(r))
	}
	return ' ', 1
}

// highlightUltracode re-colours every activating occurrence of the keyword in
// already-rendered text, leaving every other byte — including the cursor cell
// — exactly as it was.
func highlightUltracode(rendered string, frame int) string {
	return highlightWorkflowInput(rendered, frame, false)
}

// highlightWorkflowInput is highlightUltracode plus token-budget directives
// ("+500k", "+1.5m"). A directive only does something when workflows are on
// for the message, so it lights up only then: budgets is that verdict, made
// by the caller from the whole input — the keyword may have scrolled out of
// the rendered view — or from the session's ultracode toggle. Lighting a
// "+500k" the run will ignore would promise a budget nobody enforces.
func highlightWorkflowInput(rendered string, frame int, budgets bool) string {
	if rendered == "" {
		return rendered
	}
	// Cheap gate before tokenising: every activating phrase contains one of
	// these, and every directive a "+", so a line with none cannot match.
	lower := strings.ToLower(rendered)
	if !strings.Contains(lower, agent.WorkflowKeyword) && !strings.Contains(lower, "workflow") &&
		!strings.Contains(lower, "agent") && (!budgets || !strings.Contains(rendered, "+")) {
		return rendered
	}
	tokens := tokenizeANSI(rendered)

	var plain []rune
	runeToken := make([]int, 0, len(tokens))
	for i, t := range tokens {
		if t.esc == "" {
			plain = append(plain, t.r)
			runeToken = append(runeToken, i)
		}
	}
	spans := agent.WorkflowActivationSpans(string(plain))
	if budgets {
		// Same contract as the keyword: the spans come from the parser that
		// decides the budget, so what glows is exactly what will be honoured.
		spans = append(spans, agent.BudgetDirectiveSpans(string(plain))...)
	}
	if len(spans) == 0 {
		return rendered
	}

	// Style each occurrence from its own runes, not from the keyword constant:
	// the match is case-insensitive, so "ULTRACODE" must come back shouting.
	lit := make(map[int]string, len(spans)*len(agent.WorkflowKeyword))

	for _, sp := range spans {
		if sp[1] > len(runeToken) {
			continue
		}
		cells := renderUltracodeRunes(plain[sp[0]:sp[1]], frame)
		for k := sp[0]; k < sp[1]; k++ {
			lit[runeToken[k]] = cells[k-sp[0]]
		}
	}

	// Injecting our own SGR clobbers whatever style the surrounding text had,
	// so the active sequence is replayed after each lit run.
	var out strings.Builder
	var active strings.Builder
	inLit := false
	for i, t := range tokens {
		if t.esc != "" {
			if isSGRReset(t.esc) {
				active.Reset()
			} else if strings.HasSuffix(t.esc, "m") {
				active.WriteString(t.esc)
			}
			out.WriteString(t.esc)
			inLit = false
			continue
		}
		cell, isLit := lit[i]
		if !isLit || t.reverse {
			if inLit {
				out.WriteString("\x1b[0m")
				out.WriteString(active.String())
				inLit = false
			}
			out.WriteRune(t.r)
			continue
		}
		out.WriteString(cell)
		inLit = true
	}
	if inLit {
		out.WriteString("\x1b[0m")
		out.WriteString(active.String())
	}
	return out.String()
}

func isSGRReset(seq string) bool {
	if !strings.HasSuffix(seq, "m") {
		return false
	}
	params := strings.TrimSuffix(strings.TrimPrefix(seq, "\x1b["), "m")
	return params == "" || params == "0"
}
