package tui

import (
	"encoding/json"
	"fmt"
	"strings"

	"spettro/internal/termtext"
)

// labelPathWidth caps a file path inside a tool label. The transcript header
// row is cut to the terminal width anyway; this keeps one very deep path from
// crowding out the rest of the label on a wide terminal.
const labelPathWidth = 60

// labelPath prepares a path argument for a tool label: folded onto one line
// and, when too long, cut from the left so the file name stays visible.
func labelPath(p string) string {
	return termtext.FitLeft(termtext.SingleLine(p), labelPathWidth)
}

// labelCommand prepares a shell command for a tool label: a heredoc or a
// multi-line script is folded onto one line before it is cut to n runes, so
// the label shows as much of the command as the budget allows instead of a
// first line followed by a tab-indented fragment.
func labelCommand(cmd string, n int) string {
	return truncateLabel(termtext.SingleLine(cmd), n)
}

// lspLabelArgs is the subset of the lsp tool's arguments a label shows. The
// retired tools it replaced (diagnostics, references, hover, lsp-restart)
// took the same fields minus op, which their name implied.
type lspLabelArgs struct {
	Op     string `json:"op"`
	Path   string `json:"path"`
	Symbol string `json:"symbol"`
	Line   int    `json:"line"`
	Kind   string `json:"kind"`
	Server string `json:"server"`
}

// lspOp is the lsp operation a call performs, whether it was made as the
// canonical lsp tool or under one of the retired names a resumed session
// may still contain.
func lspOp(name string, args lspLabelArgs) string {
	switch name {
	case "diagnostics":
		return "diagnostics"
	case "references":
		if args.Kind == "definition" {
			return "definition"
		}
		return "references"
	case "hover":
		return "hover"
	case "lsp-restart":
		return "restart"
	}
	return strings.ToLower(strings.TrimSpace(args.Op))
}

// lspTarget names what a position-based lsp call looks at: the symbol when
// one was given, otherwise path:line, otherwise just the path.
func lspTarget(args lspLabelArgs) string {
	switch {
	case strings.TrimSpace(args.Symbol) != "":
		return truncateLabel(termtext.SingleLine(args.Symbol), 40)
	case args.Path != "" && args.Line > 0:
		return fmt.Sprintf("%s:%d", labelPath(args.Path), args.Line)
	default:
		return labelPath(args.Path)
	}
}

// lspLabel describes an lsp call (or a retired lsp tool's call) for the
// transcript and the activity panel; running selects the in-progress form.
func lspLabel(name, argsJSON string, running bool) string {
	var args lspLabelArgs
	_ = json.Unmarshal([]byte(argsJSON), &args)
	pick := func(done, inProgress string) string {
		if running {
			return inProgress
		}
		return done
	}
	target := lspTarget(args)
	switch lspOp(name, args) {
	case "diagnostics":
		if args.Path != "" {
			return pick("Checked diagnostics for "+labelPath(args.Path), "Checking diagnostics for "+labelPath(args.Path)+"…")
		}
		return pick("Checked diagnostics", "Checking diagnostics…")
	case "references":
		return pick("Found references to "+target, "Finding references to "+target+"…")
	case "definition":
		return pick("Found definition of "+target, "Finding definition of "+target+"…")
	case "hover":
		return pick("Looked up "+target, "Looking up "+target+"…")
	case "restart":
		if s := strings.TrimSpace(args.Server); s != "" {
			return pick("Restarted language server "+truncateLabel(s, 30), "Restarting language server "+truncateLabel(s, 30)+"…")
		}
		return pick("Restarted language servers", "Restarting language servers…")
	}
	return pick("Queried language server", "Querying language server…")
}

// skillLabel describes a skill call: loading a named skill, or listing the
// skills when none is named (skill-list, or skill with no name). The retired
// names skill-read, activate-skill and skill-activate load a skill like skill.
func skillLabel(name, argsJSON string, running bool) string {
	var args struct {
		Name  string `json:"name"`
		Skill string `json:"skill"`
	}
	_ = json.Unmarshal([]byte(argsJSON), &args)
	skill := strings.TrimSpace(args.Name)
	if skill == "" {
		skill = strings.TrimSpace(args.Skill)
	}
	if name == "skill-list" || skill == "" {
		if running {
			return "Listing skills…"
		}
		return "Listed skills"
	}
	skill = truncateLabel(termtext.SingleLine(skill), 40)
	if running {
		return "Loading skill " + skill + "…"
	}
	return "Loaded skill " + skill
}

// isLSPTool reports whether name is the lsp tool or one of the retired
// language-server tools it replaced.
func isLSPTool(name string) bool {
	switch name {
	case "lsp", "diagnostics", "references", "hover", "lsp-restart":
		return true
	}
	return false
}

// isSkillTool reports whether name is the skill tool or one of its retired
// names.
func isSkillTool(name string) bool {
	switch name {
	case "skill", "skill-read", "skill-list", "activate-skill", "skill-activate":
		return true
	}
	return false
}
