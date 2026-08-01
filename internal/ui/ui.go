package ui

import (
	"fmt"
	"strings"

	"spettro/internal/version"
)

const (
	reset   = "\033[0m"
	bold    = "\033[1m"
	dim     = "\033[2m"
	blue    = "\033[38;5;39m"
	cyan    = "\033[38;5;45m"
	green   = "\033[38;5;42m"
	yellow  = "\033[38;5;220m"
	gray    = "\033[38;5;246m"
	red     = "\033[38;5;203m"
	magenta = "\033[38;5;201m"
)

type Theme struct {
	Label  string
	Accent string
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
		fmt.Sprintf("%s%sSPETTRO %s%s  %sfast multi-agent coding CLI%s", bold, blue, version.App, reset, dim, reset),
		fmt.Sprintf("%sShift+Tab%s switches agents. %s/setup%s runs initial onboarding.", gray, reset, gray, reset),
	}
	return strings.Join(lines, "\n")
}

func (r *Renderer) Prompt(mode, provider, model string) string {
	t := r.theme(mode)
	return fmt.Sprintf("%s%s %s%s%s %s%s/%s%s >", t.Accent, t.Prompt, bold, mode, reset, dim, provider, model, reset)
}

func (r *Renderer) Status(mode, permission string) string {
	t := r.theme(mode)
	return fmt.Sprintf("%s%s [%s]%s %s%s%s  %sperm:%s %s%s%s", t.Accent, t.Icon, t.Label, reset, bold, strings.ToUpper(mode), reset, gray, reset, red, permission, reset)
}

func (r *Renderer) Panel(mode, title, body string) string {
	t := r.theme(mode)
	header := fmt.Sprintf("%s┌─ %s%s%s", t.Accent, bold, title, reset)
	content := fmt.Sprintf("%s│ %s%s", t.Accent, reset, body)
	footer := fmt.Sprintf("%s└─%s", t.Accent, reset)
	return strings.Join([]string{header, content, footer}, "\n")
}

func (r *Renderer) Info(s string) string {
	return fmt.Sprintf("%s%s%s", gray, s, reset)
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
