package tui

import (
	"fmt"
	"image/color"

	"charm.land/lipgloss/v2"

	"spettro/internal/theme"
)

// role is a colour that names a palette field instead of holding a hex. It
// implements color.Color by resolving against theme.Current() every time it is
// asked for its channels, which is what makes a /theme switch repaint on the
// next frame instead of on the next process.
//
// This indirection exists because lipgloss v2 stores the color.Color it is
// handed and only calls RGBA when a style is rendered (see getAsColor in
// style.go). A role parked in one of the package-level style vars below is
// therefore re-resolved per frame, so those vars — and the ~250 call sites
// across the package that reference them by name — need no rebuilding when the
// palette changes.
//
// Two roles may resolve to the same palette field (colorToolPend and colorWarn
// both land on Warning); they are kept apart here for the same reason the
// palette keeps the fields apart, so a future theme can diverge them.
type role uint8

// Colour palette. Each name resolves through the active theme at render time.
const (
	colorText role = iota
	colorMuted
	colorDim
	colorRule
	colorBorder
	colorSuccess
	colorError
	colorWarn

	colorToolPend
	colorToolRun
	colorToolOK
	colorToolErr
)

// RGBA resolves this role against the current palette. Unknown roles fall back
// to body text rather than to an invisible NoColor, so a mis-typed role shows
// up as a wrong colour instead of an unstyled hole in the UI.
func (r role) RGBA() (uint32, uint32, uint32, uint32) {
	p := theme.Current()
	var c color.Color
	switch r {
	case colorText:
		c = p.Text
	case colorMuted:
		c = p.TextMuted
	case colorDim:
		c = p.TextDim
	case colorRule:
		c = p.Rule
	case colorBorder:
		c = p.Border
	case colorSuccess, colorToolOK:
		c = p.Success
	case colorError, colorToolErr:
		c = p.Error
	case colorWarn, colorToolPend:
		c = p.Warning
	case colorToolRun:
		c = p.Info
	default:
		c = p.Text
	}
	return c.RGBA()
}

func modeColor(colorName string) color.Color {
	return theme.Current().Accent(colorName)
}

func modePrompt(agentID string) string {
	switch agentID {
	case "planning":
		return "◈"
	case "coding":
		return "◆"
	default:
		return "●"
	}
}

var (
	styleBold = lipgloss.NewStyle().Bold(true)

	styleMuted = lipgloss.NewStyle().Foreground(colorMuted)
	styleDim   = lipgloss.NewStyle().Foreground(colorDim)
	styleRule  = lipgloss.NewStyle().Foreground(colorRule)
	styleText  = lipgloss.NewStyle().Foreground(colorText)

	styleSuccess = lipgloss.NewStyle().Foreground(colorSuccess)
	styleError   = lipgloss.NewStyle().Foreground(colorError)
	styleWarn    = lipgloss.NewStyle().Foreground(colorWarn)
)

// glareGradient returns 5 color stops for the glare sweep, derived from base:
// [0]=peak (furthest from the page), [1..3]=fade toward base, [4]=base.
//
// The stops are resolved eagerly rather than as roles because base is the
// agent accent, which the caller already resolved for this frame.
func glareGradient(base color.Color) [5]color.Color {
	p := theme.Current()
	return [5]color.Color{
		lerpToHighlight(base, p.GlareStops[0]),
		lerpToHighlight(base, p.GlareStops[1]),
		lerpToHighlight(base, p.GlareStops[2]),
		lerpToHighlight(base, p.GlareStops[3]),
		base,
	}
}

// colorCacheKey returns a stable "#RRGGBB" string for c, used to compare
// colors when deciding whether the render cache is still valid. Roles resolve
// here too, so a theme switch changes the key and invalidates the cache
// without any extra bookkeeping.
func colorCacheKey(c color.Color) string {
	r, g, b, _ := c.RGBA()
	return fmt.Sprintf("#%02X%02X%02X", uint8(r>>8), uint8(g>>8), uint8(b>>8))
}

// lerpToHighlight moves base t of the way toward the theme's shimmer peak.
// The peak is white on a dark ground and near-black on a light one — lerping
// unconditionally toward white would erase the swept text on a light terminal.
func lerpToHighlight(base color.Color, t float64) color.Color {
	return theme.Lerp(base, theme.Current().FgHighlight, t)
}
