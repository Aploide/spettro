package config

import (
	"slices"
	"strings"
)

// toolFold is one canonical tool and the built-ins a migration folds into
// it.
type toolFold struct {
	canonical string
	retired   []string
}

// consolidatedTools lists the built-in tools the v12 migration folds into one
// canonical tool each. The agent runtime keeps every retired name callable
// as a hidden alias of its canonical tool (internal/agent/tool_aliases.go),
// so a manifest only has to stop listing them.
//
// Retired names are ordered so that bash's long-standing bash-output alias
// stays first, and so that the task tools that write come before the ones
// that only read (see consolidateBuiltinTools).
var consolidatedTools = []toolFold{
	{"bash", []string{"bash-output", "shell-exec"}},
	{"file-edit", []string{"multi-edit"}},
	{"grep", []string{"repo-search"}},
	{"glob", []string{"ls"}},
	{"todo-write", []string{"task-create", "task-update", "task-delete", "task-get", "task-list"}},
}

// lspConsolidatedTools is the v13 fold: the read-only language-server tools
// become the ops of one lsp tool (diagnostics; references and definition;
// hover; restart). rename-symbol writes, needs approval and stays a tool of
// its own. diagnostics comes first so a manifest without lsp gets it made
// from diagnostics, the tool the others' settings then merge into.
var lspConsolidatedTools = []toolFold{
	{"lsp", []string{"diagnostics", "references", "hover", "lsp-restart"}},
}

// skillConsolidatedTools is the v14 fold: skill-read (load one skill) and
// skill-list (list them) become one skill tool, which loads a skill when
// given a name and lists them when not. activate-skill and skill-activate
// were aliases of skill-read before; listing them here keeps them aliases
// of the new tool. An agent that held only skill-list gains the ability to
// load a skill's instructions: both halves only read SKILL.md files the
// catalog already exposes, so this is not a new kind of access.
var skillConsolidatedTools = []toolFold{
	{"skill", []string{"activate-skill", "skill-activate", "skill-read", "skill-list"}},
}

// opFolds are the canonical tools whose retired tools became operations of
// it rather than duplicates. An agent granted one through only some of them
// gets a rule denying each op of the others (see denyUnheldOps), so an agent
// that held diagnostics but not lsp-restart still cannot restart a server.
var opFolds = map[string]bool{"lsp": true}

// LSPOpPermission is the permission an lsp op is checked against, with the
// op as the pattern: { permission = "lsp-op", pattern = "restart", action =
// "deny" } stops an agent restarting language servers while it keeps the
// lookups. Only rules naming this permission exactly apply (see
// LSPOpDenied), so a catch-all rule that the lsp tool itself is an exception
// to cannot take its ops away.
const LSPOpPermission = "lsp-op"

// lspOpsByRetiredTool maps each language-server tool v13 folds into lsp to
// the ops that do its job.
var lspOpsByRetiredTool = map[string][]string{
	"diagnostics": {"diagnostics"},
	"references":  {"references", "definition"},
	"hover":       {"hover"},
	"lsp-restart": {"restart"},
}

// LSPOpsOf returns the lsp ops that replaced a retired language-server tool;
// nil for any other name.
func LSPOpsOf(retired string) []string {
	return slices.Clone(lspOpsByRetiredTool[retired])
}

// LSPOpDenied reports whether the rules, across the layers in order (runtime,
// agent, tool), deny the lsp op: the last rule whose permission is exactly
// LSPOpPermission and whose pattern matches op decides. Rules for any other
// permission, a "*" permission included, never do: those decide whether the
// lsp tool can be called at all.
func LSPOpDenied(op string, layers ...[]PermissionRule) bool {
	op = strings.ToLower(strings.TrimSpace(op))
	denied := false
	for _, rules := range layers {
		for _, r := range rules {
			if !strings.EqualFold(strings.TrimSpace(r.Permission), LSPOpPermission) || !WildcardMatch(r.Pattern, op) {
				continue
			}
			denied = r.Action == RuleDeny
		}
	}
	return denied
}

// toolFolds is every fold, in migration order.
func toolFolds() []toolFold {
	out := append(slices.Clone(consolidatedTools), lspConsolidatedTools...)
	return append(out, skillConsolidatedTools...)
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

// retiredToolNames returns the names a migration (v12, v13 or v14) folded into
// canonical, in table order; nil when it replaced none.
func retiredToolNames(canonical string) []string {
	for _, g := range toolFolds() {
		if g.canonical == canonical {
			return slices.Clone(g.retired)
		}
	}
	return nil
}

// canonicalOf returns the tool a retired built-in was folded into, by any
// migration.
func canonicalOf(id string) (string, bool) {
	for _, g := range toolFolds() {
		if slices.Contains(g.retired, id) {
			return g.canonical, true
		}
	}
	return "", false
}

// consolidateBuiltinTools folds each group's retired built-ins into its
// canonical tool and deletes the removed ones, without giving any agent
// access it did not have. v12 runs it for the duplicate built-ins (and
// removes the grok media tools), v13 for the language-server tools, v14 for
// the skill tools.
//
//   - Access is settled first, against the manifest as it stands (v11 for
//     the v12 fold, v12 for the v13 one): for
//     each agent, which retired tools it could actually call (enabled, an
//     action it may take, no permission rule denying it). Only those carry
//     over.
//   - Definitions: only built-in tools are touched; a user's own script or
//     MCP tool that shares a name is left alone, and a group whose canonical
//     name such a tool holds is not folded at all. A retired tool whose
//     canonical tool is missing becomes it in place (the ID, name and
//     description change, the operator's settings stay). Otherwise its
//     settings merge into the canonical tool toward the stricter side:
//     approval if either required it, the longer timeout, the higher risk,
//     and its command/path rules that deny or ask. The canonical tool keeps
//     its own enabled flag and permitted_actions: a disabled canonical tool
//     stays off rather than reaching every agent that listed it, and
//     todo-write never inherits task-get's "read". The retired tool's allow
//     rules and the rules that switched it off are not merged: they would
//     loosen, or switch off, the canonical tool for agents that never had
//     the retired one. Either way the retired ID becomes an alias of the
//     canonical tool.
//   - Allow-lists: each retired ID the agent could call becomes its
//     canonical ID; one it could not call (disabled, or denied by a rule) is
//     dropped rather than turned into a grant, as is a canonical ID listed
//     before the tool was defined. Duplicates collapse, order is
//     kept. An agent that could only read tasks (task-get/task-list without a
//     task tool that writes) loses them rather than gaining todo-write. A
//     list left empty holds just comment, with the agent disabled, since
//     Validate rejects an empty list.
//   - Rules are left as written. One naming a retired ID only ever decided
//     whether that tool could be called, which the allow-lists now carry;
//     rewriting it to the canonical ID would deny or allow a tool it never
//     covered. For a tool whose retired tools became its operations
//     (opFolds: lsp), an agent granted the tool through some of them gets
//     an lsp-op rule denying each op it could not call before.
//   - The canonical tool's own allow rules stay with the agents that held
//     it (see localizeCanonicalAllowRules): an agent that reached it only
//     through a retired tool does not inherit them.
//   - An agent granted a canonical tool through a retired one it could call
//     keeps being able to call it: where its rules would deny the canonical
//     tool (an allow-list written as a "*" deny plus an allow per tool), it
//     gets an agent rule allowing the canonical tool, the one tool that now
//     does what the allowed ones did.
func (m *AgentManifest) consolidateBuiltinTools(groups []toolFold, removedIDs []string) {
	usable := m.retiredToolsUsable(groups)
	folded := map[string]string{} // retired ID -> canonical ID, for folded definitions
	created := map[string]bool{}  // canonical IDs that had no definition before
	for _, g := range groups {
		if m.userToolNamed(g.canonical) {
			// The canonical name belongs to a tool of the operator's own (its
			// ID or one of its aliases). Folding a built-in into it would
			// change what that tool is, and pointing allow-lists at it would
			// grant the operator's tool in place of a built-in. The group's
			// built-ins stay tools of their own, under their own names, and
			// the agent runtime runs them unfolded (see
			// internal/agent/tool_names.go).
			continue
		}
		ci := m.toolIndex(g.canonical)
		if ci < 0 {
			ci = m.renameInPlace(g.canonical, g.retired, folded)
			created[g.canonical] = ci >= 0
		}
		for _, id := range g.retired {
			ri := m.toolIndex(id)
			if ri < 0 || ri == ci || !m.Tools[ri].IsBuiltin() {
				continue
			}
			folded[id] = g.canonical
			if ci < 0 {
				// Only read-only task tools, and no todo-write to fold them
				// into: never promote one into a tool that writes.
				m.Tools = slices.Delete(m.Tools, ri, ri+1)
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
	m.localizeCanonicalAllowRules(groups, usable, created)
	removed := map[string]bool{}
	for _, id := range removedIDs {
		if i := m.toolIndex(id); i >= 0 && m.Tools[i].IsBuiltin() {
			m.Tools = slices.Delete(m.Tools, i, i+1)
			removed[id] = true
		}
	}

	for i := range m.Agents {
		a := &m.Agents[i]
		canWriteTasks := slices.Contains(a.AllowedTools, "todo-write") && !created["todo-write"]
		for _, id := range a.AllowedTools {
			if folded[id] == "todo-write" && !readOnlyRetiredTools[id] && usable[i][id] {
				canWriteTasks = true
			}
		}
		tools := make([]string, 0, len(a.AllowedTools))
		for _, id := range a.AllowedTools {
			if removed[id] {
				continue
			}
			if canon, ok := folded[id]; ok {
				if !usable[i][id] || (readOnlyRetiredTools[id] && !canWriteTasks) {
					continue
				}
				id = canon
			} else if created[id] {
				// The agent listed a tool that did not exist, so it could
				// not call it; the definition made from a retired tool is
				// not a grant.
				continue
			}
			if !slices.Contains(tools, id) {
				tools = append(tools, id)
			}
		}
		m.denyUnheldOps(i, groups, tools, usable[i], created)
		m.keepCanonicalUsable(i, groups, tools, usable[i], created)
		if len(tools) == 0 && len(a.AllowedTools) > 0 {
			tools = []string{"comment"}
			a.Enabled = false
			if m.toolIndex("comment") < 0 {
				m.Tools = append(m.Tools, commentToolSpec)
			}
		}
		a.AllowedTools = tools
	}
}

// localizeCanonicalAllowRules moves each canonical tool's allow rules from
// the tool to the agents that held the tool before the fold, when the fold
// also grants the tool to agents that did not.
//
// Why: rules are evaluated runtime, then agent, then tool, and the last
// match wins, so a tool's own allow rule (bash: execute "*" allow) overrides
// even an agent's denies. An agent that held only shell-exec, whose rules
// asked before every command, would otherwise inherit bash's allow and run
// every command without approval. Appending the rules to the end of each
// holder's own rules keeps them after that agent's rules, as they were; the
// only change for a holder is that the tool's remaining deny and ask rules
// now come after the allows, which can only make a command ask or be
// denied, never newly allowed. A canonical tool made in place from a
// retired one (created) keeps its rules: they were that tool's all along.
func (m *AgentManifest) localizeCanonicalAllowRules(groups []toolFold, usable []map[string]bool, created map[string]bool) {
	for _, g := range groups {
		ci := m.toolIndex(g.canonical)
		if ci < 0 || created[g.canonical] || m.userToolNamed(g.canonical) {
			continue
		}
		var allows, rest []PermissionRule
		for _, r := range m.Tools[ci].PermissionRules {
			if r.Action == RuleAllow {
				allows = append(allows, r)
			} else {
				rest = append(rest, r)
			}
		}
		if len(allows) == 0 || !m.foldGrantsNewHolders(g, usable) {
			continue
		}
		for i := range m.Agents {
			a := &m.Agents[i]
			if slices.Contains(a.AllowedTools, g.canonical) {
				a.PermissionRules = append(a.PermissionRules, allows...)
			}
		}
		m.Tools[ci].PermissionRules = rest
	}
}

// foldGrantsNewHolders reports whether some agent that does not list the
// group's canonical tool could call one of its retired tools, and so gains
// the canonical tool from the fold.
func (m *AgentManifest) foldGrantsNewHolders(g toolFold, usable []map[string]bool) bool {
	for i, a := range m.Agents {
		if slices.Contains(a.AllowedTools, g.canonical) {
			continue
		}
		if slices.ContainsFunc(g.retired, func(id string) bool { return usable[i][id] }) {
			return true
		}
	}
	return false
}

// denyUnheldOps gives agent i, whose allow-list becomes tools, an lsp-op
// rule denying each operation of an op-fold tool (opFolds) that it could not
// call before the fold: it listed that retired tool but could not use it, or
// did not list it (a tool of the operator's own that shares the name, such
// as a "hover" script, is not the built-in's op). An agent that already held
// the canonical tool keeps all of it.
func (m *AgentManifest) denyUnheldOps(i int, groups []toolFold, tools []string, usable map[string]bool, created map[string]bool) {
	a := &m.Agents[i]
	for _, g := range groups {
		if !opFolds[g.canonical] || !slices.Contains(tools, g.canonical) {
			continue
		}
		if slices.Contains(a.AllowedTools, g.canonical) && !created[g.canonical] {
			continue
		}
		for _, id := range g.retired {
			if usable[id] {
				continue
			}
			for _, op := range lspOpsByRetiredTool[id] {
				rule := PermissionRule{Permission: LSPOpPermission, Pattern: op, Action: RuleDeny}
				if !slices.Contains(a.PermissionRules, rule) {
					a.PermissionRules = append(a.PermissionRules, rule)
				}
			}
		}
	}
}

// keepCanonicalUsable makes sure agent i can call each canonical tool it was
// granted through a retired tool it could call. Rules naming the retired
// tools are left as written, so an agent whose rules allowed only named
// tools ("*" denied, diagnostics allowed) would otherwise hold lsp and be
// denied it. It gets an agent rule allowing the canonical tool for each
// permission (tool, or a permission family) that denied it; the canonical
// tool's own rules still come after those and are not overridden.
func (m *AgentManifest) keepCanonicalUsable(i int, groups []toolFold, tools []string, usable map[string]bool, created map[string]bool) {
	a := &m.Agents[i]
	for _, g := range groups {
		if !slices.Contains(tools, g.canonical) {
			continue
		}
		if slices.Contains(a.AllowedTools, g.canonical) && !created[g.canonical] {
			continue // held before the fold: its rules are its own business
		}
		if !slices.ContainsFunc(g.retired, func(id string) bool { return usable[id] }) {
			continue
		}
		ci := m.toolIndex(g.canonical)
		if ci < 0 {
			continue
		}
		t := m.Tools[ci]
		layers := [][]PermissionRule{m.Runtime.PermissionRules, a.PermissionRules, t.PermissionRules}
		for _, perm := range append([]string{"tool"}, ToolPermissionFamilies(t)...) {
			if EvaluatePermissionRule(perm, t.ID, layers...) != RuleDeny {
				continue
			}
			rule := PermissionRule{Permission: perm, Pattern: t.ID, Action: RuleAllow}
			a.PermissionRules = append(a.PermissionRules, rule)
			layers[1] = a.PermissionRules
		}
	}
}

// retiredToolsUsable reports, per agent (by index), which built-ins the
// groups retire on its allow-list it could call under the manifest as it
// stands.
func (m *AgentManifest) retiredToolsUsable(groups []toolFold) []map[string]bool {
	out := make([]map[string]bool, len(m.Agents))
	for i, a := range m.Agents {
		out[i] = map[string]bool{}
		for _, id := range a.AllowedTools {
			if !slices.ContainsFunc(groups, func(g toolFold) bool { return slices.Contains(g.retired, id) }) {
				continue
			}
			// A tool of the operator's own that shares the name is not the
			// built-in, and calling it never did the built-in's job.
			if t, ok := m.toolNamed(id); ok && t.IsBuiltin() && m.ToolUsableBy(a, t) {
				out[i][id] = true
			}
		}
	}
	return out
}

// toolNamed returns the tool definition with this ID, or else the one that
// has it as an alias.
func (m *AgentManifest) toolNamed(name string) (ToolSpec, bool) {
	if i := m.toolIndex(name); i >= 0 {
		return m.Tools[i], true
	}
	for _, t := range m.Tools {
		if slices.Contains(t.Aliases, name) {
			return t, true
		}
	}
	return ToolSpec{}, false
}

// renameInPlace turns one of the retired built-ins into the missing
// canonical tool and returns its index, or -1 when there is none to turn.
// An enabled one is preferred, so the canonical tool is not switched off
// because the first retired tool in table order happened to be. Read-only
// task tools are never turned into todo-write.
func (m *AgentManifest) renameInPlace(canonical string, retired []string, folded map[string]string) int {
	pick := -1
	for _, id := range retired {
		ri := m.toolIndex(id)
		if ri < 0 || !m.Tools[ri].IsBuiltin() || readOnlyRetiredTools[id] {
			continue
		}
		if pick < 0 || (m.Tools[ri].Enabled && !m.Tools[pick].Enabled) {
			pick = ri
		}
	}
	if pick < 0 {
		return -1
	}
	t := &m.Tools[pick]
	folded[t.ID] = canonical
	t.ID = canonical
	// An operator may have taught the retired tool its future name as an
	// alias (skill-read answering to "skill"). Now that the name is the
	// tool's ID, the alias would name the tool itself, which Validate
	// rejects ("alias is another tool's id") and spettro would not start.
	t.Aliases = slices.DeleteFunc(t.Aliases, func(a string) bool { return a == canonical })
	if spec, ok := defaultToolSpec(canonical); ok {
		t.Name, t.Description = spec.Name, spec.Description
	}
	return pick
}

// userToolNamed reports whether name is the ID or an alias of a tool of the
// operator's own (kind mcp, script or http). Such a name is theirs: no
// migration folds a built-in into it, adds it to a built-in's aliases, or
// grants it to an agent in place of a built-in. It is UserTool as a yes/no,
// so the migrations and the runtime agree on whose a name is.
func (m *AgentManifest) userToolNamed(name string) bool {
	_, ok := m.UserTool(name)
	return ok
}

// builtinToolNamed reports whether name reaches a built-in tool: it is the
// ID or an alias of a built-in definition, and no tool of the operator's own
// claims it. The migrations that grant a tool, or that read an agent holding
// a tool as trust in a new one, go by this, so a tool of the operator's own
// that shares a built-in's name never counts as that built-in.
func (m *AgentManifest) builtinToolNamed(name string) bool {
	if m.userToolNamed(name) {
		return false
	}
	t, ok := m.toolNamed(name)
	return ok && t.IsBuiltin()
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
// (see consolidateBuiltinTools for what merges and why).
func mergeToolInto(dst *ToolSpec, src ToolSpec) {
	dst.RequiresApproval = dst.RequiresApproval || src.RequiresApproval
	dst.TimeoutSec = max(dst.TimeoutSec, src.TimeoutSec)
	if riskRank(src.RiskLevel) > riskRank(dst.RiskLevel) {
		dst.RiskLevel = src.RiskLevel
	}
	for _, r := range src.PermissionRules {
		if r.Action == RuleAllow || (r.Action == RuleDeny && WildcardMatch(r.Pattern, src.ID)) {
			continue
		}
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
