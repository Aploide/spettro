package agent

import "context"

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
