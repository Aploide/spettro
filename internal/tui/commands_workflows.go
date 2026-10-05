package tui

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"

	"spettro/internal/config"
	"spettro/internal/workflow"
)

const workflowsUsage = "usage: /workflows <list|show|run|size|where> [name] [args-json | task]"

const workflowsHelp = `workflow commands:
  /workflows                     list saved workflow templates (project first, then global)
  /workflows show <name>         print a saved workflow's header, params and source
  /workflows run <name> [json | task]
                                 have the agent adapt a saved template to the task
                                 and run it: JSON args, or a task in plain words
  /workflows size [tier]         show or set the size guideline:
                                 small | medium | large | unbounded
  /workflows where               show the directories scanned for saved workflows

Workflows are JavaScript orchestration scripts the model writes for the task
at hand. Spettro runs their phases, fan-outs and loops, and a script can adapt
as it goes: plan its work-list at runtime, open phases nobody declared, or
pause at a checkpoint() so the agent reads the interim results and decides
the next step before the run continues.

Write "ultracode" in a message to give the agent the workflow tool for that
turn, or run /ultracode to make orchestrating through workflows its standing
default for the session. With workflows on, "+500k" (or "+1.5m") in a message
sets the token budget the turn's workflows share.

Saved scripts are templates: the agent reads one, adapts it to the task —
filling its declared params and discovering work-lists at runtime — and runs
the adapted copy. They live in .spettro/workflows/<name>.js (project) or
~/.spettro/workflows/<name>.js (global); a project script shadows a global
one with the same name.`

func (m Model) handleWorkflowsCommand(input string) (tea.Model, tea.Cmd) {
	fields := strings.Fields(input)
	if len(fields) <= 1 {
		return m.runWorkflowsList()
	}
	switch strings.ToLower(fields[1]) {
	case "list", "ls":
		return m.runWorkflowsList()
	case "show", "cat", "info":
		return m.runWorkflowsShow(fields[2:])
	case "run", "start":
		return m.runWorkflowsRun(input, fields[2:])
	case "size":
		return m.runWorkflowsSize(fields[2:])
	case "where", "paths":
		var b strings.Builder
		b.WriteString("workflow search paths (first match wins):\n")
		for _, p := range workflow.SearchPaths(m.cwd) {
			b.WriteString("  " + p + "\n")
		}
		m.pushSystemMsg(strings.TrimRight(b.String(), "\n"))
		m.refreshViewport()
		return m, nil
	case "help":
		m.pushSystemMsg(workflowsHelp)
		m.refreshViewport()
		return m, nil
	default:
		m.showBanner(workflowsUsage, "info")
		return m, nil
	}
}

func (m Model) runWorkflowsList() (tea.Model, tea.Cmd) {
	saved := workflow.Discover(m.cwd)
	if len(saved) == 0 {
		m.pushSystemMsg("no saved workflows.\n\n" + workflowsHelp)
		m.refreshViewport()
		return m, nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "saved workflows (%d):\n", len(saved))
	for _, s := range saved {
		if s.Err != nil {
			fmt.Fprintf(&b, "  %-24s [%s] header does not parse: %v\n", s.Name, s.Scope, s.Err)
			continue
		}
		fmt.Fprintf(&b, "  %-24s [%s] %s\n", s.Name, s.Scope, s.Meta.Description)
		if s.Meta.WhenToUse != "" {
			fmt.Fprintf(&b, "  %-24s      when: %s\n", "", s.Meta.WhenToUse)
		}
		if len(s.Meta.Phases) > 0 {
			titles := make([]string, 0, len(s.Meta.Phases))
			for _, p := range s.Meta.Phases {
				titles = append(titles, p.Title)
			}
			fmt.Fprintf(&b, "  %-24s      phases: %s\n", "", strings.Join(titles, " → "))
		}
		if len(s.Meta.Params) > 0 {
			fmt.Fprintf(&b, "  %-24s      params: %s\n", "", workflowParamsSummary(s.Meta.Params))
		}
	}
	b.WriteString("\nrun one with /workflows run <name> [json args | task]")
	m.pushSystemMsg(strings.TrimRight(b.String(), "\n"))
	m.refreshViewport()
	return m, nil
}

func (m Model) runWorkflowsShow(args []string) (tea.Model, tea.Cmd) {
	if len(args) == 0 {
		m.showBanner("usage: /workflows show <name>", "error")
		return m, nil
	}
	script, path, err := workflow.Load(m.cwd, args[0])
	if err != nil {
		m.showBanner(err.Error(), "error")
		return m, nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n\n", path)
	if meta, err := workflow.ParseMeta(script); err == nil {
		fmt.Fprintf(&b, "%s — %s\n", meta.Name, meta.Description)
		for _, p := range meta.Phases {
			fmt.Fprintf(&b, "  · %s%s\n", p.Title, optionalDetail(p.Detail))
		}
		if len(meta.Params) > 0 {
			b.WriteString("params:\n")
			for _, p := range meta.Params {
				fmt.Fprintf(&b, "  · %s%s\n", workflowParamSignature(p), optionalDetail(p.Description))
			}
		}
		b.WriteString("\n")
	} else {
		fmt.Fprintf(&b, "header does not parse: %v\n\n", err)
	}
	b.WriteString("```javascript\n" + strings.TrimRight(script, "\n") + "\n```")
	m.pushSystemMsg(b.String())
	m.refreshViewport()
	return m, nil
}

func optionalDetail(detail string) string {
	if detail == "" {
		return ""
	}
	return " — " + detail
}

// runWorkflowsRun dispatches a saved workflow as an ordinary turn. The command
// does not execute the script itself: a workflow's results are only useful to
// someone who then acts on them, and that someone is the agent. Handing it the
// tool call to make keeps one execution path — the model reviews the result,
// re-dispatches failures, and integrates the outcome exactly as it would for a
// workflow it wrote itself.
//
// A saved workflow is a template, not a recording. The task it was written for
// is rarely the one at hand, and a script that replays last month's file list
// audits the wrong code with full confidence. So the agent is asked to read the
// template, adapt whatever is task-specific or stale, and run the adapted copy
// inline — running it by name only when it fits as it stands.
func (m Model) runWorkflowsRun(input string, args []string) (tea.Model, tea.Cmd) {
	if len(args) == 0 {
		m.showBanner("usage: /workflows run <name> [args-json | task]", "error")
		return m, nil
	}
	name := args[0]
	script, path, err := workflow.Load(m.cwd, name)
	if err != nil {
		m.showBanner(err.Error(), "error")
		return m, nil
	}
	meta, err := workflow.ParseMeta(script)
	if err != nil {
		m.showBanner(fmt.Sprintf("%s: %v", name, err), "error")
		return m, nil
	}

	rawArgs, task, err := splitWorkflowRunInput(restAfterFields(input, 3))
	if err != nil {
		m.showBanner(err.Error(), "error")
		return m, nil
	}

	m.showBanner("running workflow "+name+" ("+filepath.Base(path)+")", "info")
	return m.handlePrompt(workflowRunPrompt(name, path, meta, rawArgs, task))
}

// splitWorkflowRunInput reads what follows "/workflows run <name>": JSON
// becomes the run's args, anything else is the task in the user's own words,
// which the agent maps onto the template's params. Text that opens like JSON
// but does not parse is a typo in args, not a task, and is refused rather
// than handed over as prose.
func splitWorkflowRunInput(rest string) (rawArgs, task string, err error) {
	rest = strings.TrimSpace(rest)
	switch {
	case rest == "":
		return "", "", nil
	case json.Valid([]byte(rest)):
		return rest, "", nil
	case strings.HasPrefix(rest, "{") || strings.HasPrefix(rest, "["):
		return "", "", fmt.Errorf("workflow args look like JSON but do not parse")
	}
	return "", rest, nil
}

// workflowRunPrompt is the request /workflows run sends: adapt the saved
// template to the task and run it. It keeps the "ultracode" keyword, so the
// run is pre-approved — the user asked for it by name.
func workflowRunPrompt(name, path string, meta workflow.Meta, rawArgs, task string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "ultracode: run the saved workflow %s — %s.\n", jsonQuote(name), meta.Description)
	if task != "" {
		fmt.Fprintf(&b, "Task: %s\n", task)
	}
	// The template is read through the workflow tool's show argument rather
	// than the file tools: a global template lives under ~/.spettro, which
	// the file tools cannot reach from the workspace.
	fmt.Fprintf(&b, "Read the saved template (call the workflow tool with {\"name\": %s, \"show\": true}; it is %s) and check it fits the task. "+
		"Adapt anything task-specific or stale — work-lists must be discovered at runtime, never replayed "+
		"from a hardcoded list — and run the adapted script inline; or, if it fits as-is, run it by name "+
		"with {\"name\": %s", jsonQuote(name), jsonQuote(path), jsonQuote(name))
	if rawArgs != "" {
		b.WriteString(", \"args\": " + rawArgs)
	}
	b.WriteString("}.\n")
	if len(meta.Params) > 0 {
		b.WriteString("Declared params: " + workflowParamsSummary(meta.Params) + ".\n")
	} else {
		b.WriteString("Declared params: none.\n")
	}
	if rawArgs != "" {
		b.WriteString("Use these args whichever way you run it: " + rawArgs + "\n")
	}
	b.WriteString("Then review the result and act on it.")
	return b.String()
}

// workflowParamSignature renders one declared param as "name (type,
// required)" or "name (type, default x)".
func workflowParamSignature(p workflow.ParamMeta) string {
	typ := p.Type
	if typ == "" {
		typ = "any"
	}
	attrs := []string{typ}
	switch {
	case p.Required:
		attrs = append(attrs, "required")
	case p.Default != nil:
		if def, err := json.Marshal(p.Default); err == nil {
			attrs = append(attrs, "default "+string(def))
		}
	}
	return p.Name + " (" + strings.Join(attrs, ", ") + ")"
}

// workflowParamsSummary is every declared param on one line, descriptions
// included, for listings and the run prompt.
func workflowParamsSummary(params []workflow.ParamMeta) string {
	parts := make([]string, 0, len(params))
	for _, p := range params {
		parts = append(parts, workflowParamSignature(p)+optionalDetail(p.Description))
	}
	return strings.Join(parts, "; ")
}

// runWorkflowsSize shows or sets the workflow size guideline. The tier is a
// planning target the agent sizes its scripts around (and scripts read as
// the size global), not a cap, so the listing spells out what each tier
// means rather than leaving "large" to guesswork.
func (m Model) runWorkflowsSize(args []string) (tea.Model, tea.Cmd) {
	if len(args) == 0 {
		current := m.cfg.WorkflowSizeTier()
		var b strings.Builder
		if m.cfg.WorkflowSize == "" {
			fmt.Fprintf(&b, "workflow size: %s (default)\n", current)
		} else {
			fmt.Fprintf(&b, "workflow size: %s\n", current)
		}
		for _, tier := range config.WorkflowSizes {
			mark := " "
			if tier == current {
				mark = "*"
			}
			fmt.Fprintf(&b, "  %s %-10s %s\n", mark, tier, workflowSizeDescription(tier))
		}
		b.WriteString("\nset it with /workflows size <" + strings.Join(config.WorkflowSizes, "|") + ">. " +
			"It is a guideline the agent plans around, not a hard limit.")
		m.pushSystemMsg(b.String())
		m.refreshViewport()
		return m, nil
	}
	tier := strings.ToLower(strings.TrimSpace(args[0]))
	if !slices.Contains(config.WorkflowSizes, tier) {
		m.showBanner("usage: /workflows size <"+strings.Join(config.WorkflowSizes, "|")+">", "error")
		return m, nil
	}
	if err := m.updateConfig(func(cfg *config.UserConfig) error {
		cfg.WorkflowSize = tier
		return nil
	}); err != nil {
		m.showBanner("could not save workflow size: "+err.Error(), "error")
		return m, nil
	}
	msg := "workflow size set to " + tier + " — " + workflowSizeDescription(tier)
	if m.thinking {
		// The tier is captured when a run starts, like the thinking level.
		msg += " (applies from the next message)"
	}
	m.showBanner(msg, "success")
	return m, nil
}

// workflowSizeDescription spells out a tier from the engine's own table, so
// the numbers shown here are the ones scripts see.
func workflowSizeDescription(tier string) string {
	s := workflow.ResolveSize(tier)
	agents := fmt.Sprintf("~%d agents per run", s.Agents)
	if s.Agents <= 0 {
		agents = "no agent guideline"
	}
	out := fmt.Sprintf("%s, fan-outs up to %d wide", agents, s.Fanout)
	if s.Concurrency > 0 {
		out += fmt.Sprintf(", %d at a time", s.Concurrency)
	}
	return out
}

// handleUltracodeCommand toggles the session's standing ultracode opt-in:
// /ultracode [on|off], no argument flips it. While it is on every turn
// behaves as if the message said "ultracode" — the agent orchestrates
// substantive work through workflows by default and its runs need no
// consent prompt. It lasts for this session only, on purpose: a mode that
// multiplies the work every message does should not survive a restart
// unnoticed.
func (m Model) handleUltracodeCommand(fields []string) (tea.Model, tea.Cmd) {
	next := !m.ultracode
	if len(fields) >= 2 {
		switch strings.ToLower(strings.TrimSpace(fields[1])) {
		case "on":
			next = true
		case "off":
			next = false
		default:
			m.showBanner("usage: /ultracode [on|off]", "error")
			return m, nil
		}
	}
	m.ultracode = next
	suffix := ""
	if m.thinking {
		suffix = " (applies from the next message)"
	}
	switch {
	case next && !m.ultracodeActive():
		// Like /ultra's toggle, the opt-in is kept but suspended: the
		// workflow tool refuses to run under ask-first, so standing guidance
		// to run a workflow for every task would cost a failed call per turn.
		m.showBanner("ultracode on but suspended — workflows need restricted or yolo permission (the "+
			m.mode+" agent runs ask-first); switch with /permission", "warn")
	case next:
		m.showBanner("ultracode on — substantive tasks run as workflows by default, for this session"+suffix, "success")
	default:
		m.showBanner("ultracode off"+suffix, "success")
	}
	return m, nil
}

// effectiveRunPermission is the permission a run of spec executes under, by
// the rule runAgent applies: a user level other than ask-first overrides the
// agent's own, ask-first defers to it. An agent that names none is treated as
// ask-first — the cautious reading, since this only decides whether a mode
// that needs unattended sub-agents may engage.
func effectiveRunPermission(user config.PermissionLevel, spec config.AgentSpec) config.PermissionLevel {
	if user != "" && user != config.PermissionAskFirst {
		return user
	}
	if spec.Permission == "" {
		return config.PermissionAskFirst
	}
	return spec.Permission
}

// ultracodeActiveFor reports whether the session's ultracode opt-in engages
// for a run of spec: the toggle is on AND the run's permission lets the
// workflow tool run. Under ask-first every workflow call is refused, so the
// opt-in is suspended (not cleared) there, as UltraActive suspends /ultra.
// The effective permission matters, not the user level alone: under the
// default ask-first a "coding" run is still restricted by its own spec, and
// its workflows work.
func (m Model) ultracodeActiveFor(spec config.AgentSpec) bool {
	return m.ultracode && effectiveRunPermission(m.cfg.Permission, spec) != config.PermissionAskFirst
}

// ultracodeActive is ultracodeActiveFor the agent the next message goes to.
func (m Model) ultracodeActive() bool {
	if !m.ultracode {
		return false
	}
	spec, _ := m.manifest.AgentByID(m.mode)
	return m.ultracodeActiveFor(spec)
}

func jsonQuote(s string) string {
	encoded, err := json.Marshal(s)
	if err != nil {
		return `"` + s + `"`
	}
	return string(encoded)
}

// restAfterFields returns whatever follows the first n whitespace-separated
// fields, with the original spacing intact. Rejoining strings.Fields would
// collapse runs of spaces inside a JSON string literal.
func restAfterFields(input string, n int) string {
	rest := strings.TrimSpace(input)
	for range n {
		_, after, found := strings.Cut(rest, " ")
		if !found {
			return ""
		}
		rest = strings.TrimSpace(after)
	}
	return rest
}
