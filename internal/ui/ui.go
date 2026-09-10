// Package ui is the legacy line-oriented REPL renderer.
//
// It is not reachable from the shipped binary: cmd/spettro dispatches to
// `clean`, -headless, -acp or the Bubble Tea TUI, and none of them constructs
// an internal/app.App, which is this package's only importer. It is kept, and
// themed alongside the TUI, so tests/app keeps exercising the non-TUI command
// surface — but a colour changed here reaches no user, and a light-theme
// behaviour verified here is verified by unit test alone.
package ui

import (
	"fmt"
	"image/color"
	"strings"

	"spettro/internal/theme"
	"spettro/internal/version"
)

const (
	reset = "\033[0m"
	bold  = "\033[1m"
	dim   = "\033[2m"
)

// The original 256-colour table. These are bright inks picked for a black
// ground, so they are kept only for the dark theme, where they are emitted
// byte-for-byte as they always have been.
const (
	darkBlue    = "\033[38;5;39m"
	darkCyan    = "\033[38;5;45m"
	darkGreen   = "\033[38;5;42m"
	darkYellow  = "\033[38;5;220m"
	darkGray    = "\033[38;5;246m"
	darkRed     = "\033[38;5;203m"
	darkMagenta = "\033[38;5;201m"
)

// accent names a palette role instead of holding an escape sequence. This
// renderer writes straight to stdout rather than through lipgloss, so the
// sequence has to be built when a line is printed for a /theme switch to be
// visible without a restart.
type accent uint8

const (
	blue accent = iota
	cyan
	green
	yellow
	gray
	red
	magenta
)

// seq returns the SGR sequence for a in the active theme.
//
// None of the 256-colour codes above survives a light background — every one
// of them is a light ink — and no 256-colour approximation of the light
// palette is close enough to stay legible, so the light theme is emitted as
// truecolor straight from the resolved roles. The dark theme keeps its
// original codes so its output is unchanged.
func (a accent) seq() string {
	p := theme.Current()
	if p.IsDark() {
		switch a {
		case cyan:
			return darkCyan
		case green:
			return darkGreen
		case yellow:
			return darkYellow
		case gray:
			return darkGray
		case red:
			return darkRed
		case magenta:
			return darkMagenta
		default:
			return darkBlue
		}
	}
	switch a {
	case cyan:
		// This renderer has always distinguished cyan agents (debugger,
		// docs-writer, init) from blue ones, so it takes the palette's true
		// cyan rather than the TUI's AccentBlue — which is what the manifest
		// name "cyan" resolves to there, and would erase the distinction.
		return truecolor(p.AccentCyan)
	case green:
		return truecolor(p.AccentGreen)
	case yellow:
		return truecolor(p.AccentAmber)
	case gray:
		return truecolor(p.TextMuted)
	case red:
		return truecolor(p.Error)
	case magenta:
		return truecolor(p.AccentMagenta)
	default:
		return truecolor(p.AccentBlue)
	}
}

func truecolor(c color.Color) string {
	r, g, b := theme.RGB(c)
	return fmt.Sprintf("\033[38;2;%d;%d;%dm", r, g, b)
}

type Theme struct {
	Label  string
	Accent accent
	Prompt string
	Icon   string
	Stage  string
}

type Renderer struct {
	themes map[string]Theme
}

func NewRenderer() *Renderer {
	return &Renderer{
		themes: map[string]Theme{
			"planning":    {Label: "Planning Agent", Accent: blue, Prompt: "◈", Icon: "◈", Stage: "planning (planning agent)"},
			"coding":      {Label: "Coding Agent", Accent: green, Prompt: "◆", Icon: "⚙", Stage: "acting (coding agent)"},
			"architect":   {Label: "Architect", Accent: magenta, Prompt: "▲", Icon: "▲", Stage: "orchestrate (architect)"},
			"chat":        {Label: "Chat Agent", Accent: yellow, Prompt: "●", Icon: "❯", Stage: "chat (chat agent)"},
			"research":    {Label: "Research Agent", Accent: blue, Prompt: "◉", Icon: "◎", Stage: "research (research agent)"},
			"reviewer":    {Label: "Code Review Agent", Accent: red, Prompt: "◈", Icon: "◉", Stage: "review (reviewer agent)"},
			"debugger":    {Label: "Debugger Agent", Accent: cyan, Prompt: "◆", Icon: "⚑", Stage: "debug (debugger agent)"},
			"tester":      {Label: "Testing Agent", Accent: yellow, Prompt: "◉", Icon: "✓", Stage: "test (tester agent)"},
			"git-expert":  {Label: "Git Expert Agent", Accent: yellow, Prompt: "◇", Icon: "⎇", Stage: "git (git-expert agent)"},
			"docs-writer": {Label: "Documentation Agent", Accent: cyan, Prompt: "◫", Icon: "✎", Stage: "docs (docs-writer agent)"},
			"explore":     {Label: "Explore Agent", Accent: blue, Prompt: "◉", Icon: "⌖", Stage: "explore (explore agent)"},
			"init":        {Label: "Init Agent", Accent: cyan, Prompt: "◈", Icon: "⚡", Stage: "init (init agent)"},
		},
	}
}

func (r *Renderer) Welcome() string {
	lines := []string{
		fmt.Sprintf("%s%sSPETTRO %s%s  %sfast multi-agent coding CLI%s", bold, blue.seq(), version.App, reset, dim, reset),
		fmt.Sprintf("%sShift+Tab%s switches agents. %s/setup%s runs initial onboarding.", gray.seq(), reset, gray.seq(), reset),
	}
	return strings.Join(lines, "\n")
}

func (r *Renderer) Prompt(mode, provider, model string) string {
	t := r.theme(mode)
	return fmt.Sprintf("%s%s %s%s%s %s%s/%s%s >", t.Accent.seq(), t.Prompt, bold, mode, reset, dim, provider, model, reset)
}

func (r *Renderer) Status(mode, permission string) string {
	t := r.theme(mode)
	return fmt.Sprintf("%s%s [%s]%s %s%s%s  %sperm:%s %s%s%s", t.Accent.seq(), t.Icon, t.Label, reset, bold, strings.ToUpper(mode), reset, gray.seq(), reset, red.seq(), permission, reset)
}

func (r *Renderer) Panel(mode, title, body string) string {
	t := r.theme(mode)
	header := fmt.Sprintf("%s┌─ %s%s%s", t.Accent.seq(), bold, title, reset)
	content := fmt.Sprintf("%s│ %s%s", t.Accent.seq(), reset, body)
	footer := fmt.Sprintf("%s└─%s", t.Accent.seq(), reset)
	return strings.Join([]string{header, content, footer}, "\n")
}

func (r *Renderer) Info(s string) string {
	return fmt.Sprintf("%s%s%s", gray.seq(), s, reset)
}

func (r *Renderer) theme(mode string) Theme {
	if t, ok := r.themes[mode]; ok {
		return t
	}
	return Theme{Label: "Unknown", Accent: blue, Prompt: "•", Icon: "•", Stage: "unknown"}
}

func (r *Renderer) Stage(mode string) string {
	return r.theme(mode).Stage
}
