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

// inProgress marks a label as describing a call still running by ending it
// with "…", unless it already ends with one because its argument was cut
// (a doubled "……" reads as a rendering glitch).
func inProgress(label string) string {
	if strings.HasSuffix(label, "…") {
		return label
	}
	return label + "…"
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

// approvalLabel describes the "approval" trace the agent emits after a
// permission decision (see agent emitApprovalTrace): what was decided, by
// whom, and why, e.g. "Approval: allowed by user (approved once)".
func approvalLabel(argsJSON string) string {
	var args struct {
		Decision string `json:"decision"`
		Source   string `json:"source"`
		Reason   string `json:"reason"`
	}
	if json.Unmarshal([]byte(argsJSON), &args) != nil || strings.TrimSpace(args.Decision) == "" {
		return "Approval"
	}
	label := "Approval: " + termtext.SingleLine(args.Decision)
	if source := strings.TrimSpace(args.Source); source != "" {
		label += " by " + termtext.SingleLine(source)
	}
	if reason := strings.TrimSpace(args.Reason); reason != "" {
		label += " (" + truncateLabel(termtext.SingleLine(reason), 40) + ")"
	}
	return label
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

// Labels for tools of the operator's own.
//
// Every label function in this package (formatToolLabel, formatRunningLabel,
// formatToolGroupLabel and the helpers behind them) picks its wording by the
// tool's name alone: "bash" reads "Ran $ ...", "references" reads "Found
// references to ...". A tool the operator defines in the agent manifest
// (kind mcp, script or http) may take any name, a built-in's included, and
// it always owns that name: a call under it never reaches the built-in (see
// agent/tool_names.go). Labelling it as the built-in would misreport what
// ran, so a call of such a tool gets the generic label every unknown tool
// gets instead. Only the Model knows the manifest, so it makes that choice
// (toolLabel, isUserTool) and hands the transcript renderer the predicate.

// isUserTool reports whether name belongs to a tool of the operator's own,
// per the loaded agent manifest.
func (m Model) isUserTool(name string) bool {
	_, ok := m.manifest.UserTool(name)
	return ok
}

// toolLabel is the one-row label of a single tool call, as the activity
// panel and the run summary show it: the generic label for a tool of the
// operator's own, else the built-in's label for a finished or a running
// call.
func (m Model) toolLabel(name, argsJSON string, running bool) string {
	switch {
	case m.isUserTool(name):
		return userToolLabel(name, running)
	case running:
		return formatRunningLabel(name, argsJSON)
	default:
		return formatToolLabel(name, argsJSON)
	}
}

// userToolLabel is the label of one call of a tool of the operator's own:
// the same wording formatToolLabel and formatRunningLabel use for a tool
// they do not know ("Deploy Preview", "Using Deploy Preview…").
func userToolLabel(name string, running bool) string {
	if running {
		return "Using " + humanizeToolID(name) + "…"
	}
	return humanizeToolID(name)
}

// userToolGroupLabel is the header of a run of consecutive calls of a tool
// of the operator's own.
func userToolGroupLabel(name string, count int, running bool) string {
	if running {
		return fmt.Sprintf("Using %s %d time(s)…", humanizeToolID(name), count)
	}
	return fmt.Sprintf("Used %s %d times", humanizeToolID(name), count)
}
