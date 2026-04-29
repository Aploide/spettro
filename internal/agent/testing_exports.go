package agent

import (
	"context"

	"spettro/internal/config"
	"spettro/internal/provider"
)

func ParseToolCallForTesting(s string) (toolCall, bool, error) {
	return parseToolCall(s)
}

func ParseAllToolCallsForTesting(s string) ([]toolCall, []error) {
	return parseAllToolCalls(s)
}

func ParseFinalForTesting(s string) (string, bool) {
	return parseFinal(s)
}

func StripLeakedToolCallsForTesting(s string) string {
	return stripLeakedToolCalls(s)
}

func NormalizeCommandForTesting(cmd string) string {
	return normalizeCommand(cmd)
}

func IsAlwaysAllowedCommandForTesting(cmd string) bool {
	return isAlwaysAllowedCommand(cmd)
}

func AllowedCommandsPathForTesting(cwd string) string {
	return allowedCommandsPath(cwd)
}

func LoadAllowedCommandSetForTesting(cwd string) (map[string]struct{}, error) {
	return loadAllowedCommandSet(cwd)
}

func SaveAllowedCommandSetForTesting(cwd string, set map[string]struct{}) error {
	return saveAllowedCommandSet(cwd, set)
}

func SplitShellCommandSegmentsForTesting(command string) []string {
	return splitShellCommandSegments(command)
}

func AuthorizeShellCommandForTesting(r *toolRuntime, ctx context.Context, command string) error {
	return r.authorizeShellCommand(ctx, "shell-exec", command)
}

func BuildToolSchemaSectionForTesting(allowedTools []string) string {
	return buildToolSchemaSection(allowedTools)
}

func TailTrimHistoryForTesting(history string, maxBytes int) string {
	return tailTrimHistory(history, maxBytes)
}

// FilterProviderGatedToolsForTesting exposes the allowed-tools filter so
// tests can verify that devin-session (and any future provider-gated tool)
// is stripped when its API key is absent and kept when present.
func FilterProviderGatedToolsForTesting(allowed []string, policies map[string]config.ToolSpec, mgr *provider.Manager) ([]string, map[string]config.ToolSpec) {
	return filterProviderGatedTools(allowed, policies, mgr)
}
