package agent

import (
	"context"
	"fmt"
	"strings"

	"spettro/internal/provider"
)

const planSystemPrompt = `You are a software planning agent. Explore the repository thoroughly with tools, then produce a precise implementation plan.

Every response must be exactly one of:

A) One tool call:
TOOL_CALL {"tool":"<name>","args":{...}}

B) The final plan (only after you have read all relevant files):
FINAL
<plan in markdown>

Rules:
- ONE tool call per response. Never two TOOL_CALL lines.
- Never write TOOL_CALL inside the FINAL block.
- No reasoning text, no filler before TOOL_CALL or FINAL.
- FINAL is mandatory — always end with a FINAL block.
- Do not reference file paths or function names you have not verified with tools.

Plan format (inside FINAL):

## Context
Why this change is needed.

## Current state
Specific files, exported types, function signatures. No invented names.

## Proposed changes
Numbered list: each item has the exact file path and what to change.

## Reuse
Existing utilities or patterns to reuse, with file paths.

## Validation
Exact commands to verify: go build ./..., go test ./..., manual checks.

## Critical files
3-5 most important files for this change.

## Risks
Edge cases, breaking changes, things to watch out for.`

// LLMPlanner uses the active provider to generate an implementation plan.
type LLMPlanner struct {
	ProviderManager *provider.Manager
	ProviderName    func() string
	ModelName       func() string
	CWD             string
	MaxTokens       int // max tokens per request; 0 = unlimited
	RequiredReads   []string
	ToolCallback    func(ToolTrace) // optional: called with status="running" before and final status after each tool
}

func (p LLMPlanner) Plan(ctx context.Context, userPrompt string) (RunResult, error) {
	prompt := strings.TrimSpace(userPrompt)
	if prompt == "" {
		return RunResult{}, fmt.Errorf("empty planning prompt")
	}

	systemPrompt := loadPromptOrFallback(p.CWD, "agents/planning.md", planSystemPrompt)
	plan, traces, tokens, contextTokens, err := runToolLoop(ctx, toolLoopConfig{
		SystemPrompt:    systemPrompt,
		UserTask:        prompt,
		CWD:             p.CWD,
		MaxSteps:        30,
		RequireToolCall: true,
		AllowedTools:    []string{"repo-search", "file-read", "glob", "grep"},
		LogToolCalls:    true,
		ProviderManager: p.ProviderManager,
		ProviderName:    p.ProviderName,
		ModelName:       p.ModelName,
		MaxTokens:       p.MaxTokens,
		RequiredReads:   p.RequiredReads,
		ToolCallback:    p.ToolCallback,
	})
	if err != nil {
		return RunResult{}, fmt.Errorf("llm planner: %w", err)
	}

	plan = strings.TrimSpace(plan)
	if plan == "" {
		return RunResult{}, fmt.Errorf("LLM returned empty plan")
	}
	plan = stripLeakedToolCalls(plan)
	plan = strings.TrimSpace(plan)
	if plan == "" {
		return RunResult{}, fmt.Errorf("LLM returned empty plan after stripping tool calls")
	}
	main, _ := stripThinkTags(plan)
	return RunResult{
		Content: fmt.Sprintf("# Plan\n\n%s\n\n---\n*planned by %s / %s*\n",
			strings.TrimSpace(main), p.ProviderName(), p.ModelName()),
		Tools:         traces,
		TokensUsed:    tokens,
		ContextTokens: contextTokens,
	}, nil
}
