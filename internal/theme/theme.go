// Package theme resolves and holds the colour palette the TUI renders with.
//
// Three selections ship: "dark", "light" and "auto". "auto" asks the terminal
// what its background is and picks accordingly, degrading to dark whenever the
// answer is missing, unparseable, or impossible to ask for. The selection is
// resolved once at startup — SPETTRO_THEME beats the persisted user config,
// which beats auto-detection, which beats the dark default — and may be
// revised once more if the terminal answers the background query late.
//
// The package is palette-only by design: it decides which colours are used,
// never how anything is laid out or drawn.
package theme

import (
	"image/color"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
)

// Kind is a theme selection. AutoKind is a request to detect, not a palette;
// Resolve turns it into DarkKind or LightKind.
type Kind string

const (
	AutoKind  Kind = "auto"
	DarkKind  Kind = "dark"
	LightKind Kind = "light"
)

func (k Kind) String() string { return string(k) }

// EnvVar selects the palette at startup, ahead of both the persisted config
// and detection. It is the escape hatch for a terminal that lies about its
// background, and for scripted runs that want a predictable palette.
//
// It is a startup override, not a lock: /theme still repaints and still saves
// while the variable is set, because a user staring at an unreadable terminal
// must be able to fix it without restarting. What the variable guarantees is
// that the next start with it set begins on that palette again, whatever was
// saved in between.
const EnvVar = "SPETTRO_THEME"

// Parse validates a user-supplied theme name. Empty input is AutoKind, which
// is the zero value the config file carries when the user has never chosen.
// Anything unrecognised is rejected rather than silently coerced, so a typo in
// SPETTRO_THEME or config.json falls through to the next source of truth
// instead of quietly selecting a palette nobody asked for.
func Parse(s string) (Kind, bool) {
	switch Kind(strings.ToLower(strings.TrimSpace(s))) {
	case "":
		return AutoKind, true
	case AutoKind:
		return AutoKind, true
	case DarkKind:
		return DarkKind, true
	case LightKind:
		return LightKind, true
	default:
		return AutoKind, false
	}
}

// Preferred returns what the user asked for, before any detection: the
// SPETTRO_THEME environment variable if it names a valid theme, otherwise the
// value persisted in the user config, otherwise auto.
func Preferred(configured string) Kind {
	if k, ok := Parse(os.Getenv(EnvVar)); ok && strings.TrimSpace(os.Getenv(EnvVar)) != "" {
		return k
	}
	k, ok := Parse(configured)
	if !ok {
		return AutoKind
	}
	return k
}

// Resolve turns a selection into a concrete palette kind.
//
// An explicit dark or light selection is returned untouched — detection must
// never flip a palette the user chose. For AutoKind the terminal's reported
// background wins when it is known, COLORFGBG is consulted next, and dark is
// the answer when neither can say. Dark is the safe default in both
// directions: light ink on a light terminal is unreadable, whereas the dark
// palette on a light terminal is merely unpleasant.
func Resolve(kind Kind, detectedBackground color.Color) Kind {
	switch kind {
	case DarkKind, LightKind:
		return kind
	}
	if detectedBackground != nil {
		if IsDarkColor(detectedBackground) {
			return DarkKind
		}
		return LightKind
	}
	if dark, ok := ColorFGBG(os.Getenv("COLORFGBG")); ok {
		if dark {
			return DarkKind
		}
		return LightKind
	}
	return DarkKind
}

// Seed resolves a selection for the first paint, before any terminal answer
// can be in hand. It is Resolve plus the one fact Resolve has no way to know:
// whether the terminal can be asked at all.
//
// COLORFGBG is inherited by child processes, so on a terminal that cannot
// answer an OSC 11 query — redirected output, a test binary, TERM=dumb — the
// value describes whatever launched us rather than us, and the documented dark
// fallback is a better answer than a stale environment variable. Every caller
// that installs a palette without a detected background goes through here, so
// startup and a runtime /theme cannot disagree about the same inputs.
func Seed(kind Kind) Kind {
	if kind == AutoKind && !CanQueryTerminal() {
		return DarkKind
	}
	return Resolve(kind, nil)
}

// IsDarkColor classifies a background colour as dark.
//
// The rule is HSL lightness below 50%, which is byte-for-byte what Bubble Tea
// and Lip Gloss apply to a reported terminal background. Matching them exactly
// matters more than picking a perceptually better formula: a colour classified
// here and the same colour classified by BackgroundColorMsg.IsDark must never
// disagree. A nil colour is dark, again matching the libraries.
func IsDarkColor(c color.Color) bool {
	if c == nil {
		return true
	}
	r, g, b := RGB(c)
	hi := float64(max(r, max(g, b))) / 255
	lo := float64(min(r, min(g, b))) / 255
	return (hi+lo)/2 < 0.5
}

// ColorFGBG classifies the terminal background from the COLORFGBG variable,
// which rxvt, urxvt and Konsole set to "fg;bg" — or, in some rxvt builds, to
// "fg;default;bg", where only the last field is the background.
//
// It is a hint and not a source of truth: the value is inherited by child
// processes, so it goes stale across a tmux attach from a different terminal
// or a theme switch within the same session. Use it to seed the first paint;
// let a terminal's own answer overwrite it.
func ColorFGBG(v string) (dark, ok bool) {
	parts := strings.Split(v, ";")
	if len(parts) < 2 {
		return false, false
	}
	n, err := strconv.Atoi(strings.TrimSpace(parts[len(parts)-1]))
	if err != nil || n < 0 || n > 15 {
		return false, false
	}
	// 0-6 are the dark half of the base ANSI set and 8 is "bright black"
	// (a mid grey); 7 is silver and 9-15 are the bright set.
	return n <= 6 || n == 8, true
}

// CanQueryTerminal reports whether it is safe to put an OSC 11 background
// query on the wire.
//
// Bubble Tea writes escape sequences to its output whether or not that output
// is a terminal, so a redirected stdout would otherwise collect a literal
// "\x1b]11;?\a" in the middle of the captured text. A terminal that cannot
// answer only costs a wasted round trip, but one that cannot even receive the
// question costs correctness.
func CanQueryTerminal() bool {
	if !isCharDevice(os.Stdin) || !isCharDevice(os.Stdout) {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(os.Getenv("TERM"))) {
	case "", "dumb":
		return false
	}
	return true
}

func isCharDevice(f *os.File) bool {
	if f == nil {
		return false
	}
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// current holds the process-wide palette.
//
// An atomic.Pointer rather than an RWMutex: reads vastly outnumber writes —
// every style built during every frame reads it, while writes happen at most
// twice per session (startup, then a late answer to the background query) —
// and a pointer load is wait-free, so the render path never contends with the
// goroutine that delivers detection or with a test swapping themes. Storing
// the whole palette behind one pointer also makes a theme switch atomic:
// nothing can observe half of one palette and half of another mid-frame.
var current atomic.Pointer[Palette]

func init() {
	p := Dark()
	current.Store(&p)
}

// Current returns the palette in force. Safe to call from any goroutine.
func Current() Palette { return *current.Load() }

// CurrentKind returns the resolved kind in force — never AutoKind.
func CurrentKind() Kind { return current.Load().Kind }

// Set installs the palette for a resolved kind and returns it. AutoKind is
// resolved with no detected background, i.e. via COLORFGBG or the dark
// default; callers that have a detected background should run Resolve first.
func Set(k Kind) Palette {
	p := For(Resolve(k, nil))
	current.Store(&p)
	return p
}

// SetPalette installs an explicit palette. Tests use it to pin a theme; the
// TUI uses Set.
func SetPalette(p Palette) {
	current.Store(&p)
}
