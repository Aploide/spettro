package agent

import (
	"context"
	"fmt"
	"strings"

	"spettro/internal/provider"
)

const exploreFallbackPrompt = `You are an Explore agent. Map the codebase rapidly by calling multiple tools in parallel, then produce a concise technical summary.`

// LLMExplorer is a read-only agent for fast codebase exploration.
// It supports parallel tool calls (glob, grep, file-read) and produces
// structured technical summaries without modifying any files.
type LLMExplorer struct {
	ProviderManager *provider.Manager
	ProviderName    func() string
	ModelName       func() string
	CWD             string
	MaxTokens       int
	ToolCallback    func(ToolTrace)
}

func (e LLMExplorer) Explore(ctx context.Context, task string) (RunResult, error) {
	task = strings.TrimSpace(task)
	if task == "" {
		return RunResult{}, fmt.Errorf("empty explore task")
	}

	systemPrompt := loadPromptOrFallback(e.CWD, "agents/explore.md", exploreFallbackPrompt)
	result, traces, tokens, contextTokens, err := runToolLoop(ctx, toolLoopConfig{
		SystemPrompt:    systemPrompt,
		UserTask:        task,
		CWD:             e.CWD,
		RequireToolCall: true,
		AllowedTools:    []string{"glob", "grep", "file-read"},
		LogToolCalls:    true,
		ProviderManager: e.ProviderManager,
		ProviderName:    e.ProviderName,
		ModelName:       e.ModelName,
		MaxTokens:       e.MaxTokens,
		ToolCallback:    e.ToolCallback,
	})
	if err != nil {
		return RunResult{}, fmt.Errorf("explorer: %w", err)
	}

	result = strings.TrimSpace(result)
	result = stripLeakedToolCalls(result)
	main, _ := stripThinkTags(result)
	return RunResult{
		Content:       strings.TrimSpace(main),
		Tools:         traces,
		TokensUsed:    tokens,
		ContextTokens: contextTokens,
	}, nil
}
