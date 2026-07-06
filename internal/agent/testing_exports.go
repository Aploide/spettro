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

func IsBlockedCommandForTesting(cmd string) bool {
	return isBlockedCommand(cmd)
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

// BuildLoopPromptForTesting concatenates the system string and the initial user
// message for a minimal config so tests can assert how the cross-turn
// conversation History is surfaced. The toolLog argument is ignored (tool
// results are now separate message turns, not a flat log string). The step
// argument is likewise ignored: the system prompt is deliberately identical on
// every step so the provider prompt cache keeps hitting.
func BuildLoopPromptForTesting(systemPrompt, userTask, history, _ string, _ int) string {
	cfg := toolLoopConfig{
		SystemPrompt: systemPrompt,
		UserTask:     userTask,
		History:      history,
		CWD:          "/tmp/x",
	}
	return buildSystemString(cfg, false) + "\n\n" + buildInitialUserMessage(cfg)
}

// BuildTurnUserMessageForTesting exposes the follow-up-turn user message built
// when a structured prior conversation is carried in.
func BuildTurnUserMessageForTesting(userTask string, requiredReads []string) string {
	return buildTurnUserMessage(toolLoopConfig{UserTask: userTask, RequiredReads: requiredReads, CWD: "/tmp/x"})
}

func TailTrimHistoryForTesting(history string, maxBytes int) string {
	return tailTrimHistory(history, maxBytes)
}

func EnforceCommitCoAuthorForTesting(command string) string {
	return EnforceCommitCoAuthor(command)
}

func IsGitCommitInvocationForTesting(seg string) bool {
	return isGitCommitInvocation(seg)
}

func LexShellTokensForTesting(seg string) []string {
	return lexShellTokens(seg)
}

func SpettroCoAuthorTrailerForTesting() string {
	return spettroCoAuthorTrailer
}

func ResolveMediaPathForTesting(cwd, requested, prompt, kind string) (dir, baseName, fixedExt string, hasExt, dirOnly bool) {
	return resolveMediaPath(cwd, requested, prompt, kind)
}

func SlugifyPromptForTesting(prompt string) string {
	return slugifyPrompt(prompt)
}

func IsNextJSProjectForTesting(cwd string) bool {
	return isNextJSProject(cwd)
}

func PickExtensionForTesting(mime, kind string) string {
	return pickExtension(mime, kind)
}

func DefaultMediaDirForTesting(cwd string) string {
	return defaultMediaDirFor(cwd)
}
