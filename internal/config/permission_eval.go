package config

import (
	"path/filepath"
	"strings"
)

// NormalizePermissionFamily maps a permitted action onto the permission
// family rules are written against ("write" and its synonyms are "edit").
func NormalizePermissionFamily(action string) string {
	a := strings.ToLower(strings.TrimSpace(action))
	switch a {
	case "write", "edit", "apply_patch", "file-write", "multiedit":
		return "edit"
	default:
		return a
	}
}

// EvaluatePermissionRule returns the action of the last rule, across the
// layers in order, whose permission and pattern match; ask when none does.
func EvaluatePermissionRule(permission, pattern string, layers ...[]PermissionRule) RuleAction {
	perm := strings.TrimSpace(permission)
	pat := filepath.ToSlash(strings.TrimSpace(pattern))
	if perm == "" {
		return RuleAsk
	}
	decision := RuleAsk
	for _, rules := range layers {
		for _, rule := range rules {
			if !WildcardMatch(rule.Permission, perm) {
				continue
			}
			if !WildcardMatch(rule.Pattern, pat) {
				continue
			}
			decision = rule.Action
		}
	}
	return decision
}

// WildcardMatch reports whether a rule's permission or pattern matches value:
// "*" matches anything, otherwise a path.Match glob, a "dir/*" prefix or the
// exact string.
func WildcardMatch(rulePattern, value string) bool {
	rulePattern = strings.TrimSpace(rulePattern)
	value = filepath.ToSlash(strings.TrimSpace(value))
	if rulePattern == "" {
		return false
	}
	if rulePattern == "*" {
		return true
	}
	rulePattern = filepath.ToSlash(rulePattern)
	ok, err := filepath.Match(rulePattern, value)
	if err == nil && ok {
		return true
	}
	if strings.HasSuffix(rulePattern, "/*") {
		prefix := strings.TrimSuffix(rulePattern, "*")
		return strings.HasPrefix(value, prefix)
	}
	return rulePattern == value
}

// ToolPermissionFamilies returns the distinct permission families of a
// tool's permitted actions, in order.
func ToolPermissionFamilies(tool ToolSpec) []string {
	families := make([]string, 0, len(tool.PermittedActions))
	seen := map[string]struct{}{}
	for _, action := range tool.PermittedActions {
		fam := NormalizePermissionFamily(action)
		if fam == "" {
			continue
		}
		if _, ok := seen[fam]; ok {
			continue
		}
		seen[fam] = struct{}{}
		families = append(families, fam)
	}
	return families
}

// ToolSharesAction reports whether the tool has a permitted action the agent
// is allowed (always, when either list is empty).
func ToolSharesAction(tool ToolSpec, agent AgentSpec) bool {
	if len(agent.PermittedActions) == 0 || len(tool.PermittedActions) == 0 {
		return true
	}
	allowed := map[string]struct{}{}
	for _, action := range agent.PermittedActions {
		if a := NormalizePermissionFamily(action); a != "" {
			allowed[a] = struct{}{}
		}
	}
	if len(allowed) == 0 {
		return true
	}
	for _, action := range tool.PermittedActions {
		if _, ok := allowed[NormalizePermissionFamily(action)]; ok {
			return true
		}
	}
	return false
}

// ToolAllowedByRules reports whether no permission rule (runtime, agent,
// then the tool's own) denies the agent the tool, either outright
// (permission "tool") or for one of its permission families.
func (m *AgentManifest) ToolAllowedByRules(agent AgentSpec, tool ToolSpec) bool {
	layers := [][]PermissionRule{m.Runtime.PermissionRules, agent.PermissionRules, tool.PermissionRules}
	if EvaluatePermissionRule("tool", tool.ID, layers...) == RuleDeny {
		return false
	}
	for _, fam := range ToolPermissionFamilies(tool) {
		if EvaluatePermissionRule(fam, tool.ID, layers...) == RuleDeny {
			return false
		}
	}
	return true
}

// ToolUsableBy reports whether the agent can actually call the tool when it
// lists it: the tool is enabled, not primary-only for a worker, shares an
// action with the agent, and no rule denies it. It is the per-tool test the
// agent runtime applies when it resolves an allow-list.
func (m *AgentManifest) ToolUsableBy(agent AgentSpec, tool ToolSpec) bool {
	if !tool.Enabled {
		return false
	}
	if tool.PrimaryOnly && !agent.IsPrimaryRole() {
		return false
	}
	return ToolSharesAction(tool, agent) && m.ToolAllowedByRules(agent, tool)
}
