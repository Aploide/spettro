package agent

import (
	"strings"

	"spettro/internal/config"
)

func toolPermissionFamilies(tool config.ToolSpec) []string {
	return config.ToolPermissionFamilies(tool)
}

func resolveToolPolicies(spec config.AgentSpec, manifest *config.AgentManifest) ([]string, map[string]config.ToolSpec) {
	ordered := make([]string, 0, len(spec.AllowedTools))
	seen := map[string]struct{}{}
	for _, id := range spec.AllowedTools {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		ordered = append(ordered, id)
	}

	if manifest == nil {
		return ordered, map[string]config.ToolSpec{}
	}

	toolByID := map[string]config.ToolSpec{}
	for _, tool := range manifest.Tools {
		toolByID[tool.ID] = tool
		for _, alias := range tool.Aliases {
			alias = strings.TrimSpace(alias)
			if alias != "" {
				toolByID[alias] = tool
			}
		}
	}

	allowed := make([]string, 0, len(ordered))
	policies := map[string]config.ToolSpec{}
	for _, id := range ordered {
		tool, ok := toolByID[id]
		if !ok || !manifest.ToolUsableBy(spec, tool) {
			continue
		}
		allowed = append(allowed, id)
		policies[id] = tool
	}

	return allowed, policies
}
