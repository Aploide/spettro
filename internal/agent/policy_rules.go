package agent

import "spettro/internal/config"

// The rule evaluation lives in config so the manifest migrations resolve
// access exactly as the runtime does.

func evaluatePermissionRule(permission, pattern string, layers ...[]config.PermissionRule) config.RuleAction {
	return config.EvaluatePermissionRule(permission, pattern, layers...)
}
