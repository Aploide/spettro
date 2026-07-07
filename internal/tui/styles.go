package tui

import (
	"fmt"
	"image/color"

	"charm.land/lipgloss/v2"
)

// Color palette
var (
	colorText    = lipgloss.Color("#F9FAFB")
	colorMuted   = lipgloss.Color("#6B7280")
	colorDim     = lipgloss.Color("#374151")
	colorBorder  = lipgloss.Color("#4B5563")
	colorSuccess = lipgloss.Color("#10B981")
	colorError   = lipgloss.Color("#EF4444")
	colorWarn    = lipgloss.Color("#F59E0B")

	colorToolPend = lipgloss.Color("#F59E0B")
	colorToolRun  = lipgloss.Color("#60A5FA")
	colorToolOK   = lipgloss.Color("#10B981")
	colorToolErr  = lipgloss.Color("#EF4444")

	colorHeaderBg = lipgloss.Color("#0D0D0D")
	colorSelBg    = lipgloss.Color("#1F2937") // selection highlight bg (new)
)

func modeColor(colorName string) color.Color {
	switch colorName {
	case "blue":
		return lipgloss.Color("#A78BFA")
	case "green":
		return lipgloss.Color("#34D399")
	case "cyan":
		return lipgloss.Color("#60A5FA")
	case "yellow":
		return lipgloss.Color("#F59E0B")
	case "magenta":
		return lipgloss.Color("#C084FC")
	case "purple":
		return lipgloss.Color("#BD93F9")
	case "red":
		return lipgloss.Color("#EF4444")
	// Legacy mode-name fallbacks for backward compatibility
	case "plan":
		return lipgloss.Color("#BD93F9")
	case "planning":
		return lipgloss.Color("#A78BFA")
	case "coding":
		return lipgloss.Color("#34D399")
	case "chat":
		return lipgloss.Color("#60A5FA")
	default:
		return lipgloss.Color("#60A5FA")
	}
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

func modeLabel(agentID string) string {
	return agentID
}

var (
	styleBold = lipgloss.NewStyle().Bold(true)

	styleMuted = lipgloss.NewStyle().Foreground(colorMuted)
	styleDim   = lipgloss.NewStyle().Foreground(colorDim)
	styleText  = lipgloss.NewStyle().Foreground(colorText)

	styleSuccess = lipgloss.NewStyle().Foreground(colorSuccess)
	styleError   = lipgloss.NewStyle().Foreground(colorError)
	styleWarn    = lipgloss.NewStyle().Foreground(colorWarn)

	styleToolPend = lipgloss.NewStyle().Foreground(colorToolPend)
	styleToolRun  = lipgloss.NewStyle().Foreground(colorToolRun)
	styleToolOK   = lipgloss.NewStyle().Foreground(colorToolOK)
	styleToolErr  = lipgloss.NewStyle().Foreground(colorToolErr)
)

func modeStyle(mode string) lipgloss.Style {
	return lipgloss.NewStyle().Foreground(modeColor(mode)).Bold(true)
}

func modeBorderStyle(mode string) lipgloss.Style {
	return lipgloss.NewStyle().
		BorderStyle(lipgloss.RoundedBorder()).
		BorderForeground(modeColor(mode)).
		PaddingLeft(1).PaddingRight(1)
}

func dimBorderStyle() lipgloss.Style {
	return lipgloss.NewStyle().
		BorderStyle(lipgloss.RoundedBorder()).
		BorderForeground(colorBorder).
		PaddingLeft(1).PaddingRight(1)
}

// glareGradient returns 5 color stops for the glare sweep, derived from base:
// [0]=peak (near-white tint), [1..3]=fade toward base, [4]=base.
func glareGradient(base color.Color) [5]color.Color {
	r16, g16, b16, _ := base.RGBA()
	r, g, b := uint8(r16>>8), uint8(g16>>8), uint8(b16>>8)
	return [5]color.Color{
		lerpToWhite(r, g, b, 0.88),
		lerpToWhite(r, g, b, 0.60),
		lerpToWhite(r, g, b, 0.30),
		lerpToWhite(r, g, b, 0.12),
		base,
	}
}

// colorCacheKey returns a stable "#RRGGBB" string for c, used to compare
// colors when deciding whether the render cache is still valid.
func colorCacheKey(c color.Color) string {
	r, g, b, _ := c.RGBA()
	return fmt.Sprintf("#%02X%02X%02X", uint8(r>>8), uint8(g>>8), uint8(b>>8))
}

func lerpToWhite(r, g, b uint8, t float64) color.Color {
	lerp := func(c uint8) uint8 { return uint8(float64(c) + t*(255-float64(c))) }
	return lipgloss.Color(fmt.Sprintf("#%02X%02X%02X", lerp(r), lerp(g), lerp(b)))
}
