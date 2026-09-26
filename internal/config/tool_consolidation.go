package config

import (
	"slices"
	"strings"
)

// consolidatedTools lists the built-in tools the v12 migration folds into one
// canonical tool each. The agent runtime keeps every retired name callable
// as a hidden alias of its canonical tool (internal/agent/tool_aliases.go),
// so a manifest only has to stop listing them.
//
// Retired names are ordered so that bash's long-standing bash-output alias
// stays first, and so that the task tools that write come before the ones
// that only read (see consolidateBuiltinTools).
var consolidatedTools = []struct {
	canonical string
	retired   []string
}{
	{"bash", []string{"bash-output", "shell-exec"}},
	{"file-edit", []string{"multi-edit"}},
	{"grep", []string{"repo-search"}},
	{"glob", []string{"ls"}},
	{"todo-write", []string{"task-create", "task-update", "task-delete", "task-get", "task-list"}},
}

// v11ToolDescriptions are the descriptions the canonical tools shipped with
// before they absorbed their duplicates. v12 replaces a description that is
// still exactly this with the current default, so it names what the tool now
// does; one the operator rewrote is kept.
var v11ToolDescriptions = map[string]string{
	"glob":       "Find files by name pattern.",
	"grep":       "Search file contents with regex.",
	"bash":       "Execute a bash command and return output.",
	"todo-write": "Write a list of todos to track task progress.",
}

// removedTools are built-ins v12 deletes outright.
var removedTools = []string{"grok-image", "grok-video"}

// readOnlyRetiredTools are retired tools whose canonical tool can write. An
// agent that held only these could read the task list but not change it, so
// the migration drops them instead of handing it todo-write.
var readOnlyRetiredTools = map[string]bool{"task-get": true, "task-list": true}

// retiredToolNames returns the names v12 folded into canonical, in table
// order; nil when it replaced none.
func retiredToolNames(canonical string) []string {
	for _, g := range consolidatedTools {
		if g.canonical == canonical {
			return slices.Clone(g.retired)
		}
	}
	return nil
}

// canonicalOf returns the tool a retired built-in was folded into.
func canonicalOf(id string) (string, bool) {
	for _, g := range consolidatedTools {
		if slices.Contains(g.retired, id) {
			return g.canonical, true
		}
	}
	return "", false
}

// consolidateBuiltinTools is the v12 migration: it folds the duplicate
// built-ins into their canonical tools and removes the grok media tools,
// without giving any agent access it did not have.
//
//   - Definitions: only built-in tools are touched; a user's own script or
//     MCP tool that shares a name is left alone. A retired tool whose
//     canonical tool is missing becomes it in place (the ID, name and
//     description change, the operator's settings stay). Otherwise its
//     settings merge into the canonical tool, each toward the stricter or
//     more generous side the operator already chose: approval if either
//     required it, the longer timeout, the higher risk, enabled if either
//     was, both rule sets. permitted_actions are not merged: todo-write
//     would inherit task-get's "read" and reach agents that may only read.
//     Either way the retired ID becomes an alias of the canonical tool.
//   - Allow-lists: each retired ID becomes its canonical ID, duplicates
//     collapse, order is kept. An agent that could only read tasks
//     (task-get/task-list without a task tool that writes) loses them
//     rather than gaining todo-write. A list left empty holds just comment,
//     with the agent disabled, since Validate rejects an empty list.
//   - Rules: a tool-level rule (permission "tool" or "*") naming a retired ID
//     exactly now names the canonical ID, so a deny on shell-exec keeps
//     denying bash.
func (m *AgentManifest) consolidateBuiltinTools() {
	folded := map[string]string{} // retired ID -> canonical ID, for folded definitions
	for _, g := range consolidatedTools {
		ci := m.toolIndex(g.canonical)
		if ci >= 0 && m.Tools[ci].Kind != "builtin" {
			// The canonical name belongs to a tool of the operator's own;
			// folding a built-in into it would change what it is.
			continue
		}
		for _, id := range g.retired {
			ri := m.toolIndex(id)
			if ri < 0 || m.Tools[ri].Kind != "builtin" {
				continue
			}
			folded[id] = g.canonical
			if ci < 0 {
				if readOnlyRetiredTools[id] {
					// Never promote a read-only tool into one that writes.
					m.Tools = slices.Delete(m.Tools, ri, ri+1)
					continue
				}
				t := &m.Tools[ri]
				t.ID = g.canonical
				if spec, ok := defaultToolSpec(g.canonical); ok {
					t.Name, t.Description = spec.Name, spec.Description
				}
				ci = ri
				continue
			}
			mergeToolInto(&m.Tools[ci], m.Tools[ri])
			m.Tools = slices.Delete(m.Tools, ri, ri+1)
			if ri < ci {
				ci--
			}
		}
		if ci >= 0 {
			if spec, ok := defaultToolSpec(g.canonical); ok && m.Tools[ci].Description == v11ToolDescriptions[g.canonical] {
				m.Tools[ci].Description = spec.Description
			}
			for _, id := range g.retired {
				// A name some other tool already answers to (the operator's
				// own "ls" script, or its alias) stays theirs.
				if slices.Contains(m.Tools[ci].Aliases, id) || m.nameTaken(id, ci) {
					continue
				}
				m.Tools[ci].Aliases = append(m.Tools[ci].Aliases, id)
			}
		}
	}
	removed := map[string]bool{}
	for _, id := range removedTools {
		if i := m.toolIndex(id); i >= 0 && m.Tools[i].Kind == "builtin" {
			m.Tools = slices.Delete(m.Tools, i, i+1)
			removed[id] = true
		}
	}

	for i := range m.Agents {
		a := &m.Agents[i]
		canWriteTasks := false
		for _, id := range a.AllowedTools {
			if canon, ok := folded[id]; (ok && canon == "todo-write" && !readOnlyRetiredTools[id]) || id == "todo-write" {
				canWriteTasks = true
			}
		}
		tools := make([]string, 0, len(a.AllowedTools))
		for _, id := range a.AllowedTools {
			if removed[id] {
				continue
			}
			if canon, ok := folded[id]; ok {
				if readOnlyRetiredTools[id] && !canWriteTasks {
					continue
				}
				id = canon
			}
			if !slices.Contains(tools, id) {
				tools = append(tools, id)
			}
		}
		if len(tools) == 0 && len(a.AllowedTools) > 0 {
			tools = []string{"comment"}
			a.Enabled = false
			if m.toolIndex("comment") < 0 {
				m.Tools = append(m.Tools, commentToolSpec)
			}
		}
		a.AllowedTools = tools
		a.PermissionRules = renameToolRules(a.PermissionRules, folded)
	}
	m.Runtime.PermissionRules = renameToolRules(m.Runtime.PermissionRules, folded)
	for i := range m.Tools {
		m.Tools[i].PermissionRules = renameToolRules(m.Tools[i].PermissionRules, folded)
	}
}

// toolIndex returns the position of the tool definition with this ID, or -1.
func (m *AgentManifest) toolIndex(id string) int {
	return slices.IndexFunc(m.Tools, func(t ToolSpec) bool { return t.ID == id })
}

// nameTaken reports whether a tool other than the one at index except has
// name as its ID or as one of its aliases.
func (m *AgentManifest) nameTaken(name string, except int) bool {
	for i, t := range m.Tools {
		if i != except && (t.ID == name || slices.Contains(t.Aliases, name)) {
			return true
		}
	}
	return false
}

// mergeToolInto folds a retired tool's settings into its canonical tool
// (see consolidateBuiltinTools for why permitted_actions are not merged).
func mergeToolInto(dst *ToolSpec, src ToolSpec) {
	dst.RequiresApproval = dst.RequiresApproval || src.RequiresApproval
	dst.TimeoutSec = max(dst.TimeoutSec, src.TimeoutSec)
	if riskRank(src.RiskLevel) > riskRank(dst.RiskLevel) {
		dst.RiskLevel = src.RiskLevel
	}
	dst.Enabled = dst.Enabled || src.Enabled
	for _, r := range src.PermissionRules {
		if !slices.Contains(dst.PermissionRules, r) {
			dst.PermissionRules = append(dst.PermissionRules, r)
		}
	}
	for _, alias := range src.Aliases {
		if alias != dst.ID && !slices.Contains(dst.Aliases, alias) {
			dst.Aliases = append(dst.Aliases, alias)
		}
	}
}

func riskRank(level string) int {
	switch strings.TrimSpace(level) {
	case "low":
		return 1
	case "medium":
		return 2
	case "high":
		return 3
	}
	return 0
}

// renameToolRules points tool-level rules (permission "tool" or "*") whose
// pattern is exactly a folded ID at its canonical ID, dropping rules that
// become duplicates.
func renameToolRules(rules []PermissionRule, folded map[string]string) []PermissionRule {
	if len(rules) == 0 {
		return rules
	}
	out := make([]PermissionRule, 0, len(rules))
	for _, r := range rules {
		perm := strings.TrimSpace(r.Permission)
		if perm == "tool" || perm == "*" {
			if canon, ok := folded[strings.TrimSpace(r.Pattern)]; ok {
				r.Pattern = canon
			}
		}
		if !slices.Contains(out, r) {
			out = append(out, r)
		}
	}
	return out
}

// defaultToolSpec returns the built-in definition of a tool as the default
// manifest ships it.
func defaultToolSpec(id string) (ToolSpec, bool) {
	for _, t := range defaultToolSpecs() {
		if t.ID == id {
			return t, true
		}
	}
	return ToolSpec{}, false
}
