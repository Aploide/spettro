package tui

import (
	"fmt"
	"math/rand/v2"
	"time"

	"charm.land/lipgloss/v2"
)

// workingSymbols is the pulsing glyph at the head of the working indicator.
// Braille spinners are already spoken for by the login/onboarding screens;
// this cycle is deliberately softer so a long run does not strobe. It stops
// at "✳" rather than fading on down to "✢"/"·": those two read as the line
// having lost its glyph for a step, not as a pulse.
var workingSymbols = []string{"✻", "✽", "✶", "✳", "✶", "✽"}

// workingSymbolFrameDivisor slows the glyph to ~200 ms a step off the 50 ms
// tick, roughly half the pace of the glare sweep so the two read as one
// motion instead of competing.
const workingSymbolFrameDivisor = 4

// workingVerbs is the pool the per-run status word is drawn from: mostly
// playful English, with a few ghost/Italian ones for the Spettro house style.
// One verb is picked when a run starts and held for the whole turn — rolling
// it per frame would turn the line into a slot machine.
var workingVerbs = []string{
	"Unravelling",
	"Skidaddling",
	"Glimping",
	"Percolating",
	"Conjuring",
	"Puzzling",
	"Noodling",
	"Simmering",
	"Tinkering",
	"Wrangling",
	"Untangling",
	"Ruminating",
	"Scheming",
	"Spelunking",
	"Cogitating",
	"Marinating",
	"Whirring",
	"Finagling",
	"Pondering",
	"Rustling",
	"Haunting",
	"Materialising",
	"Spettrando",
	"Sussurrando",
	"Vanishing",
	"Manifesting",
}

// nextWorkingVerb draws a verb that is not the one the previous turn used, so
// two runs in a row never look like the indicator froze.
func nextWorkingVerb(prev string) string {
	if len(workingVerbs) < 2 {
		// Rejection sampling would never terminate on a one-entry pool.
		return workingVerbs[0]
	}
	for {
		verb := workingVerbs[rand.IntN(len(workingVerbs))]
		if verb != prev {
			return verb
		}
	}
}

// beginRunIndicator marks the start of a run for the working indicator: the
// elapsed clock restarts and a fresh verb is drawn. Every path that sets
// m.thinking calls it, otherwise the indicator would show the previous run's
// start time (agentStartAt is never cleared on completion).
func (m *Model) beginRunIndicator() {
	m.agentStartAt = time.Now()
	m.workingVerb = nextWorkingVerb(m.workingVerb)
}

// formatRunElapsed renders a run's age as "2m 38s" (hours only once it gets
// that far). Go's own duration string drops the space and the leading zero
// minute, which makes the line jitter in width as seconds tick over.
func formatRunElapsed(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	total := int(d.Round(time.Second).Seconds())
	h, m, s := total/3600, (total%3600)/60, total%60
	if h > 0 {
		return fmt.Sprintf("%dh %02dm %02ds", h, m, s)
	}
	return fmt.Sprintf("%dm %02ds", m, s)
}

// viewWorkingIndicator is the one-line "✶ Unravelling… (2m 38s · ↓ 2.9k
// tokens)" readout that sits directly above the input box while the agent
// works. It replaces the old status-bar ticker, which had to compete with the
// goal and loop lines for the same slot.
//
// It lives outside the viewport so View() repaints it on every 50 ms tick,
// and it returns "" when idle — recalcLayout measures this same function, so
// the reserved row and the drawn row can never disagree.
func (m Model) viewWorkingIndicator(width int) string {
	if !m.thinking || m.agentStartAt.IsZero() {
		return ""
	}
	verb := m.workingVerb
	if verb == "" {
		// A run started before a verb was drawn (older session, or a path
		// that set thinking directly) still deserves a readout.
		verb = workingVerbs[0]
	}
	mc := m.currentColor()
	symbol := workingSymbols[(m.eyeFrame/workingSymbolFrameDivisor)%len(workingSymbols)]

	// Only the verb is swept: renderGlare styles every rune individually, and
	// a shimmering clock would read as a second animation.
	line := lipgloss.NewStyle().Foreground(mc).Render(symbol) + " " +
		renderGlare(verb+"…", m.eyeFrame, mc) +
		styleMuted.Render(fmt.Sprintf(" (%s · ↓ %s tokens)",
			formatRunElapsed(time.Since(m.agentStartAt)),
			formatTokenCount(m.liveRunTokens)))

	// MaxWidth clips rather than wraps: a second row here would silently
	// overflow the budget recalcLayout reserved.
	return lipgloss.NewStyle().PaddingLeft(1).MaxWidth(width).Render(line)
}

// workingIndicatorHeight is the indicator's cost in rows, 0 when idle.
// lipgloss.Height reports 1 for the empty string, so the emptiness check has
// to happen here rather than at each call site.
func (m Model) workingIndicatorHeight() int {
	ind := m.viewWorkingIndicator(m.paneWidth())
	if ind == "" {
		return 0
	}
	return lipgloss.Height(ind)
}
