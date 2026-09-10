package theme

import (
	"image/color"

	"charm.land/lipgloss/v2"
)

// Palette is the complete set of semantic colour roles the TUI renders with.
// Every visible colour in the app resolves to exactly one field here; nothing
// downstream should carry its own hex literal.
//
// The roles are named after what a colour *means*, not what it looks like, so
// the light theme can invert a relationship (TextOnAccent goes from near-black
// to white) without any call site knowing. Several roles hold byte-identical
// values in the dark theme — Warning and AccentAmber, Error and AccentRed,
// Info and AccentBlue — and are kept apart anyway because they are separate
// decisions the light theme is free to unmake.
//
// BorderFocus is deliberately absent: the focused border is the active agent's
// accent, resolved at render time through Accent, and duplicating it as a flat
// field would let the two drift.
type Palette struct {
	// Kind is the theme this palette belongs to. It also answers "is the
	// terminal dark?" for the handful of decisions that are polarity, not
	// colour — picking bubbles' dark vs light textarea chrome, for one.
	Kind Kind

	// --- Text ---------------------------------------------------------

	// Text is body copy, headings, table headers and selected-row labels.
	Text color.Color
	// TextMuted is secondary text: hints, metadata, descriptions, inactive
	// tabs, table border glyphs, quotes, the spinner.
	TextMuted color.Color
	// TextSubtle sits one step above TextMuted: real content that is
	// de-emphasised rather than incidental — diff context lines, the
	// neutral plan-tier badge.
	TextSubtle color.Color
	// TextDim is the faintest ink that still carries prose: thinking-block
	// italics, mined memory facts, session previews, side-panel detail. It
	// is read, so it clears the same contrast floor as the rest of the text
	// roles; the separators and fills it also drew in the dark theme live in
	// Rule, which is free to recede further than prose may.
	TextDim color.Color
	// TextFaint is prose that has receded as far as prose may go and still
	// be read: captured tool output under a tool call. It sits between
	// TextMuted and TextDim, and it exists as its own role because its dark
	// value collides with Border — a collision that is meaningless (one is
	// a rule, the other is italic body text) and that the light theme has
	// to break, since Border falls below AA for anything you actually read.
	TextFaint color.Color
	// TextOnAccent is foreground painted on top of an accent-filled chip
	// (the active agent tab). This is the one role whose polarity inverts:
	// near-black on a bright dark-theme accent, white on a dark light-theme
	// accent. It shares a hex with BgHeader in the dark theme by
	// coincidence, not by meaning.
	TextOnAccent color.Color

	// --- Structure ----------------------------------------------------

	// Border is the neutral box border and hairline rule: dialog frames,
	// the side panel, the header divider.
	Border color.Color
	// Rule is the faintest decorative ink, drawn but never read: the header
	// separator, markdown's horizontal rule, the "╱" fill in dialog titles,
	// empty progress cells, the "•" between side-panel fields, and the
	// whitespace lipgloss paints around a centred block.
	//
	// It shares TextDim's hex in the dark theme, and is held apart for the
	// same reason TextFaint is held apart from Border: the collision is
	// meaningless — one draws lines, the other carries prose — and the light
	// theme has to break it. Prose at a rule's lightness falls below AA,
	// while a rule at prose's lightness reads as heavy as body text.
	Rule color.Color
	// BgHeader is the painted band behind the top header and bottom status
	// bar — slightly off the terminal's own ground, in whichever direction
	// the theme runs.
	BgHeader color.Color
	// BgSelection is the painted background of the selected row in every
	// list, palette and dialog.
	BgSelection color.Color

	// --- Code ---------------------------------------------------------

	// BgCode and CodeFg are the fenced code block's surface and ink.
	BgCode color.Color
	CodeFg color.Color
	// BgCodeInline and CodeInlineFg are the inline `code span` chip, also
	// used by attachment chips. Kept apart from BgSelection (identical in
	// dark) because a chip inside a sentence wants a subtler tint than a
	// selection highlight.
	BgCodeInline color.Color
	CodeInlineFg color.Color

	// --- Status -------------------------------------------------------

	// Success is the completed/positive state: ✓ markers, finished workflow
	// steps, added diff lines, the healthy-cache indicator.
	Success color.Color
	// SuccessBright is a second, livelier green for counters and
	// "changed/added" affordances: git +N, running jobs and PTYs.
	SuccessBright color.Color
	// Error is the failure state: ✗ markers, failed steps, git -N, removed
	// diff lines, a critical context gauge.
	Error color.Color
	// Warning is caution: pending tools, the shell-approval accent, an
	// in-progress todo, a degraded cache.
	Warning color.Color
	// WarningSoft is a lighter amber for non-blocking marks: the favourite
	// ★ badge, the folder-trust caution copy.
	WarningSoft color.Color
	// Info is neutral-informational: running tools and workflow phases,
	// diff hunk headers.
	Info color.Color
	// Danger is destructive-action emphasis, hotter than Error: the remove
	// provider confirmation.
	Danger color.Color

	// --- Agent accents ------------------------------------------------
	//
	// One per manifest colour name. Resolve them through Accent rather than
	// reading the fields directly, so the legacy mode-name aliases ("plan",
	// "coding", …) keep working. AccentViolet is the manifest's "blue" and
	// AccentBlue is its "cyan" — the names have been wrong since the
	// manifest was written and are preserved here so the mapping stays
	// traceable.
	AccentViolet  color.Color
	AccentGreen   color.Color
	AccentBlue    color.Color
	AccentAmber   color.Color
	AccentMagenta color.Color
	AccentPurple  color.Color
	AccentRed     color.Color
	// AccentCyan is a true cyan, which the agent manifest has no name for —
	// its "cyan" is AccentBlue, and Accent keeps it that way. This role
	// exists for the legacy REPL renderer in internal/ui, whose own palette
	// has always held blue and cyan apart; without it, every cyan-accented
	// agent there collapses onto the blue ones as soon as the 256-colour
	// inks are dropped for the light theme.
	//
	// That renderer is not reachable from cmd/spettro (see the package
	// comment on internal/ui), so this is the one role no user's terminal
	// ever paints. It is here so the palette stays complete for the tests
	// that do reach it, not because it is on screen somewhere.
	AccentCyan color.Color

	// --- Diff intra-line emphasis -------------------------------------
	//
	// The changed span inside a modified line. These are judged against
	// their own painted background rather than the terminal's, and both
	// backgrounds flip from a deep box to a pale wash in the light theme.
	DiffAddHiFg color.Color
	DiffAddHiBg color.Color
	DiffDelHiFg color.Color
	DiffDelHiBg color.Color

	// DiffLineNo is the line-number gutter and DiffDivider the rule between
	// side-by-side columns. Both share a hex with a neutral role in the dark
	// theme (Border and TextDim respectively) and are held separate so the
	// light theme can keep the diff's own recede order intact: context text,
	// then gutter, then divider, each a clear step further back.
	DiffLineNo  color.Color
	DiffDivider color.Color

	// --- Subscription tier badges -------------------------------------
	//
	// "free" renders as TextSubtle and "lite" as Text; only these two have
	// colours of their own. "max" cycles RampRainbow.
	PlanPlus color.Color
	PlanPro  color.Color

	// --- Animation anchors --------------------------------------------

	// BgBase is the theme's notion of the terminal's own ground. Runtime
	// fades interpolate toward it, so a "faded out" cell disappears into
	// the page instead of toward a hardcoded near-black.
	BgBase color.Color
	// FgHighlight is the peak a shimmer travels toward — the cell furthest
	// from the surrounding page. On a dark ground "furthest" means whiter;
	// on a light ground it means darker, which is why this is a role and
	// not the literal white the glare helper used to lerp to.
	FgHighlight color.Color
	// GlowSpecular is FgHighlight for the ultracode glow specifically. It
	// carries a violet cast in the light theme so the specular band stays
	// part of RampGlow instead of punching a neutral hole through it.
	GlowSpecular color.Color

	// RampGlow is the ordered anchor list the ultracode shimmer is sampled
	// from: violet into magenta into cyan, looping.
	RampGlow []color.Color
	// RampRainbow is the ordered anchor list cycled one hue per character
	// for the animated "max" plan badge.
	RampRainbow []color.Color

	// EyesScan is the thinking scan-line falloff by row distance from the
	// lit row (distance 1, 2, 3+). The lit row itself is the agent accent.
	// Retained for the eye art's animated renderers; the TUI draws the logo
	// statically inside the scrollback and does not read this today.
	EyesScan [3]color.Color
	// EyesBlink is the blink cycle: squinting, half-closed, closed.
	//
	// Both eye ramps deliberately fade past the contrast floor at their far
	// steps — the effect *is* a fade to invisibility, and the dark theme's
	// own #1A1A2E is barely 1.1:1 on black. The light values walk the same
	// ramp toward white rather than toward black: each light step is tuned
	// to the contrast its dark counterpart has against black (scan
	// 5.9/2.8/1.5, blink 4.1/2.0/1.2), so the animation reads at the same
	// strength on either ground instead of washing out on the light one.
	// Retained alongside EyesScan; not read by the static inline logo.
	EyesBlink [3]color.Color

	// GlareStops are the four interpolation weights the glare sweep uses to
	// build its peak-to-base gradient toward FgHighlight. The light theme
	// scales them down: a full 0.88 toward a near-black highlight would
	// crush every agent accent to the same ink and destroy the hue identity
	// the glare is derived from.
	GlareStops [4]float64
	// GlowShine scales the ultracode specular band's pull toward
	// GlowSpecular; GlowTintBias and GlowTintGain are the constant and
	// shine-driven halves of the cell background's pull from BgBase toward
	// the sampled ramp colour.
	GlowShine    float64
	GlowTintBias float64
	GlowTintGain float64
}

// IsDark reports whether this palette is drawn for a dark terminal. Use it for
// the few decisions that are polarity rather than colour, such as choosing
// between bubbles' dark and light textarea chrome.
func (p Palette) IsDark() bool { return p.Kind != LightKind }

// Accent maps a manifest colour name — or one of the legacy mode names kept
// for backward compatibility — to this palette's agent accent. Unknown names
// fall back to AccentBlue, matching the behaviour the TUI has always had.
func (p Palette) Accent(name string) color.Color {
	switch name {
	case "blue":
		return p.AccentViolet
	case "green":
		return p.AccentGreen
	case "cyan":
		return p.AccentBlue
	case "yellow":
		return p.AccentAmber
	case "magenta":
		return p.AccentMagenta
	case "purple":
		return p.AccentPurple
	case "red":
		return p.AccentRed
	// Legacy mode-name fallbacks for backward compatibility.
	case "plan":
		return p.AccentPurple
	case "planning":
		return p.AccentViolet
	case "coding":
		return p.AccentGreen
	case "chat":
		return p.AccentBlue
	default:
		return p.AccentBlue
	}
}

// hex builds a palette colour from a "#RRGGBB" literal. lipgloss.Color
// silently degrades a malformed literal to NoColor, which would show up as an
// unstyled cell somewhere deep in the UI rather than as an error, so the
// palettes below are checked once at init instead.
func hex(s string) color.Color {
	c := lipgloss.Color(s)
	if _, ok := c.(lipgloss.NoColor); ok {
		panic("theme: malformed colour literal " + s)
	}
	return c
}

// Dark is the palette for a dark terminal. Every value here is byte-for-byte
// what the TUI rendered before themes existed: the dark theme is not a
// redesign, it is the status quo given names.
func Dark() Palette {
	return Palette{
		Kind: DarkKind,

		Text:         hex("#F9FAFB"),
		TextMuted:    hex("#6B7280"),
		TextSubtle:   hex("#9CA3AF"),
		TextDim:      hex("#374151"),
		TextFaint:    hex("#4B5563"),
		TextOnAccent: hex("#0D0D0D"),

		Border:      hex("#4B5563"),
		Rule:        hex("#374151"),
		BgHeader:    hex("#0D0D0D"),
		BgSelection: hex("#1F2937"),

		BgCode:       hex("#111827"),
		CodeFg:       hex("#E5E7EB"),
		BgCodeInline: hex("#1F2937"),
		CodeInlineFg: hex("#D1D5DB"),

		Success:       hex("#10B981"),
		SuccessBright: hex("#22C55E"),
		Error:         hex("#EF4444"),
		Warning:       hex("#F59E0B"),
		WarningSoft:   hex("#FBBF24"),
		Info:          hex("#60A5FA"),
		Danger:        hex("#FF5555"),

		AccentViolet:  hex("#A78BFA"),
		AccentGreen:   hex("#34D399"),
		AccentBlue:    hex("#60A5FA"),
		AccentAmber:   hex("#F59E0B"),
		AccentMagenta: hex("#C084FC"),
		AccentPurple:  hex("#BD93F9"),
		AccentRed:     hex("#EF4444"),
		AccentCyan:    hex("#00D7FF"),

		DiffAddHiFg: hex("#6EE7B7"),
		DiffAddHiBg: hex("#064E3B"),
		DiffDelHiFg: hex("#FCA5A5"),
		DiffDelHiBg: hex("#7F1D1D"),
		DiffLineNo:  hex("#4B5563"),
		DiffDivider: hex("#374151"),

		PlanPlus: hex("#86EFAC"),
		PlanPro:  hex("#C4B5FD"),

		BgBase:       hex("#0B0B0D"),
		FgHighlight:  hex("#FFFFFF"),
		GlowSpecular: hex("#FFFFFF"),

		RampGlow: []color.Color{
			hex("#7C3AED"), hex("#A855F7"), hex("#E879F9"), hex("#38BDF8"), hex("#22D3EE"),
		},
		RampRainbow: []color.Color{
			hex("#FF6B6B"), hex("#FF9E4F"), hex("#FFD93D"), hex("#6BCB77"), hex("#4D96FF"), hex("#C77DFF"),
		},

		EyesScan:  [3]color.Color{hex("#888888"), hex("#555555"), hex("#2A2A2A")},
		EyesBlink: [3]color.Color{hex("#6B6B8A"), hex("#3D3D5C"), hex("#1A1A2E")},

		GlareStops:   [4]float64{0.88, 0.60, 0.30, 0.12},
		GlowShine:    0.90,
		GlowTintBias: 0.16,
		GlowTintGain: 0.26,
	}
}

// Light is the palette for a light terminal. It is a re-tune of Dark, not a
// different design: same roles, same relationships, hues held as close as
// contrast allows. Values were picked against a #F5F5F5 ground rather than
// pure white, since that is the harder of the two for dark ink.
//
// Every role that carries prose — down to and including TextDim — clears WCAG
// AA (4.5:1) on that ground, and the roles that are only ever drawn clear 3:1.
// The two documented exemptions are the animations, which fade to
// invisibility on purpose: the eye ramps (see EyesBlink) and the ultracode
// glow, whose lit ink is judged against its own moving tint rather than
// against the page.
//
// Two pairs collapse here that are distinct in Dark: AccentGreen onto Success
// and AccentAmber onto Warning. Their dark values are already near-duplicates,
// and holding two dark greens apart at AA on white produces two colours nobody
// can tell apart.
func Light() Palette {
	return Palette{
		Kind: LightKind,

		Text:         hex("#111827"),
		TextMuted:    hex("#55606E"),
		TextSubtle:   hex("#4B5563"),
		TextDim:      hex("#67707D"),
		TextFaint:    hex("#5C6674"),
		TextOnAccent: hex("#FFFFFF"),

		Border:      hex("#6B7688"),
		Rule:        hex("#7C8797"),
		BgHeader:    hex("#EEF1F6"),
		BgSelection: hex("#DDE4F0"),

		BgCode:       hex("#E6EAF0"),
		CodeFg:       hex("#111827"),
		BgCodeInline: hex("#EDF0F5"),
		CodeInlineFg: hex("#1F2937"),

		Success:       hex("#047857"),
		SuccessBright: hex("#15803D"),
		Error:         hex("#B91C1C"),
		Warning:       hex("#96590A"),
		WarningSoft:   hex("#7A6300"),
		Info:          hex("#1D4ED8"),
		Danger:        hex("#C81E1E"),

		AccentViolet:  hex("#6D28D9"),
		AccentGreen:   hex("#047857"),
		AccentBlue:    hex("#1D4ED8"),
		AccentAmber:   hex("#96590A"),
		AccentMagenta: hex("#9333EA"),
		AccentPurple:  hex("#7C3AED"),
		AccentRed:     hex("#B91C1C"),
		AccentCyan:    hex("#0E7490"),

		DiffAddHiFg: hex("#065F46"),
		DiffAddHiBg: hex("#D6F5E5"),
		DiffDelHiFg: hex("#991B1B"),
		DiffDelHiBg: hex("#FBDDDD"),
		DiffLineNo:  hex("#78828F"),
		DiffDivider: hex("#838E9D"),

		PlanPlus: hex("#15803D"),
		PlanPro:  hex("#6D28D9"),

		BgBase:       hex("#FCFCFE"),
		FgHighlight:  hex("#111827"),
		GlowSpecular: hex("#14061F"),

		RampGlow: []color.Color{
			hex("#6D28D9"), hex("#8B21C4"), hex("#A21CAF"), hex("#0369A1"), hex("#0E7490"),
		},
		RampRainbow: []color.Color{
			hex("#C62828"), hex("#A24E08"), hex("#7A6300"), hex("#2E7D32"), hex("#2563EB"), hex("#8B37D9"),
		},

		EyesScan:  [3]color.Color{hex("#5D656F"), hex("#929BA7"), hex("#CFD6DE")},
		EyesBlink: [3]color.Color{hex("#797D90"), hex("#B3B6C6"), hex("#E5E7EF")},

		GlareStops:   [4]float64{0.55, 0.37, 0.19, 0.07},
		GlowShine:    0.60,
		GlowTintBias: 0.14,
		GlowTintGain: 0.22,
	}
}

// For returns the palette for a resolved kind. AutoKind has no palette of its
// own; it degrades to Dark, which is the fallback everywhere detection fails.
func For(k Kind) Palette {
	if k == LightKind {
		return Light()
	}
	return Dark()
}

// Lerp interpolates from a toward b by t (clamped to [0,1]) in sRGB space. It
// is the single interpolation behind the glare sweep, so "toward the
// highlight" means the same thing in both themes.
//
// Channels are truncated rather than rounded, matching the arithmetic the
// glare gradient used when its target was a hardcoded white. That keeps the
// dark theme's output byte-identical to what shipped before themes existed.
func Lerp(a, b color.Color, t float64) color.Color {
	if t < 0 {
		t = 0
	} else if t > 1 {
		t = 1
	}
	ar, ag, ab := RGB(a)
	br, bg, bb := RGB(b)
	mix := func(x, y uint8) uint8 { return uint8(float64(x) + t*(float64(y)-float64(x))) }
	return color.RGBA{R: mix(ar, br), G: mix(ag, bg), B: mix(ab, bb), A: 0xFF}
}

// RGB flattens any palette colour to its three 8-bit channels, for the
// animation code that does its own interpolation in float space.
func RGB(c color.Color) (r, g, b uint8) {
	r16, g16, b16, _ := c.RGBA()
	return uint8(r16 >> 8), uint8(g16 >> 8), uint8(b16 >> 8)
}
