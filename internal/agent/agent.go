package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"spettro/internal/config"
	"spettro/internal/provider"
	"spettro/internal/skills"
)

// Legacy interfaces — kept for backward compatibility with existing tests and callers.

type PlanningAgent interface {
	Plan(context.Context, string) (RunResult, error)
}

type CodingAgent interface {
	Execute(context.Context, string, config.PermissionLevel, bool) (RunResult, error)
}

type ChatAgent interface {
	Reply(context.Context, string, []string) (provider.Response, error)
}

// CommitAgent is defined in committer.go.
// SearchAgent is defined in searcher.go.
// ExploreAgent is defined in explorer.go.

type ExploreAgent interface {
	Explore(context.Context, string) (RunResult, error)
}

type ToolTrace struct {
	AgentID string
	Name    string
	Status  string
	Args    string
	Output  string
}

type RunResult struct {
	Content    string
	Tools      []ToolTrace
	TokensUsed int // total tokens consumed across all LLM calls in the run (session COST)
	// ContextTokens approximates how full the context window is after the
	// run: the largest single LLM request (prompt+completion). It is NOT a
	// sum across steps — each step's prompt re-embeds the rolling history, so
	// summing would double-count the same context and inflate the gauge.
	ContextTokens int
}

// Legacy stub types — kept so existing tests compile.

type Planner struct{}

func (Planner) Plan(_ context.Context, userPrompt string) (RunResult, error) {
	p := strings.TrimSpace(userPrompt)
	if p == "" {
		return RunResult{}, fmt.Errorf("empty planning prompt")
	}

	return RunResult{
		Content: fmt.Sprintf(
			"# Generated Plan\n\n- Timestamp: %s\n- Objective: %s\n\n## Steps\n1. Analyze current files\n2. Propose edits\n3. Request approval\n4. Execute in coding mode\n",
			time.Now().UTC().Format(time.RFC3339),
			p,
		),
	}, nil
}

type Coder struct{}

func (Coder) Execute(_ context.Context, plan string, level config.PermissionLevel, approved bool) (RunResult, error) {
	if strings.TrimSpace(plan) == "" {
		return RunResult{}, fmt.Errorf("empty approved plan")
	}

	if level == config.PermissionAskFirst && !approved {
		return RunResult{}, fmt.Errorf("ask-first policy requires explicit approval")
	}

	return RunResult{
		Content: fmt.Sprintf("Executed plan with permission=%s.\nSummary: %s\n", level, compact(plan)),
	}, nil
}

type Chatter struct {
	ProviderManager *provider.Manager
	ProviderName    func() string
	ModelName       func() string
	Thinking        provider.ThinkingLevel
}

func (c Chatter) Reply(ctx context.Context, prompt string, images []string) (provider.Response, error) {
	return c.ProviderManager.Send(ctx, c.ProviderName(), c.ModelName(), provider.Request{
		Prompt:   prompt,
		Images:   images,
		Thinking: c.Thinking,
	})
}

// LLMAgent is the unified agent runner. It reads the agent's system prompt from
// the PromptFile specified in the spec (stripping frontmatter), and runs the
// standard tool loop with the tools, permissions, and limits from the spec.
type LLMAgent struct {
	Spec            config.AgentSpec
	ProviderManager *provider.Manager
	ProviderName    func() string
	ModelName       func() string
	CWD             string
	MaxTokens       int
	Thinking        provider.ThinkingLevel
	RequiredReads   []string
	Images          []string // only used on first LLM call (chat use case)
	// History is an optional bounded transcript of prior conversation turns,
	// surfaced to the model as a "Conversation so far" section so follow-up
	// turns have memory. Empty == first turn (no behavior change). The caller
	// is responsible for bounding it (see maxHistoryBytes).
	History      string
	ToolCallback func(ToolTrace)
	// StreamCallback, when set, receives live thinking/answer chunks as the
	// model streams. Set only on the top-level run (chat/coding/plan/ask).
	StreamCallback StreamCallback
	ShellApproval  ShellApprovalCallback
	AskUser        AskUserCallback
	Manifest       *config.AgentManifest // for sub-agent spawning via agent tool
	// SandboxState is the session-scoped OS sandbox policy shared across the
	// whole agent tree. nil means the sandbox feature is disabled.
	SandboxState    *SandboxState
	SessionDir      string
	DelegationDepth int
	ParentAgentID   string

	// GoalMode enables generous tool timeouts and (step 03) goal-complete
	// signaling. Non-goal runs behave exactly as before.
	GoalMode        bool
	ContextWindow   int // model context window in tokens; drives in-loop compaction. 0 → default
	ShellTimeoutSec int // goal-mode per shell/bash timeout; 0 → default
}

func (a LLMAgent) Run(ctx context.Context, task string) (RunResult, error) {
	task = strings.TrimSpace(task)
	if task == "" {
		return RunResult{}, fmt.Errorf("empty task")
	}
	systemPrompt := loadPromptOrFallback(a.CWD, a.Spec.PromptFile, a.Spec.Description)
	allowedTools, policies := resolveToolPolicies(a.Spec, a.Manifest)
	requireToolCall := a.Spec.Mode != "ask" && len(allowedTools) > 0
	logToolCalls := true
	maxWorkers := 4
	maxDelegationDepth := 2
	maxToolCallsPerStep := 32
	if a.Manifest != nil {
		logToolCalls = a.Manifest.Runtime.LogToolCalls
		if a.Manifest.Runtime.Delegation.MaxParallelWorkers > 0 {
			maxWorkers = a.Manifest.Runtime.Delegation.MaxParallelWorkers
		}
		if a.Manifest.Runtime.Delegation.MaxDepth > 0 {
			maxDelegationDepth = a.Manifest.Runtime.Delegation.MaxDepth
		}
		if a.Manifest.Runtime.Delegation.MaxToolCallsPerStep > 0 {
			maxToolCallsPerStep = a.Manifest.Runtime.Delegation.MaxToolCallsPerStep
		}
	}
	catalog, _ := skills.Discover(a.CWD, skills.DefaultLookupOptions())
	catalog = filterDisabledSkills(catalog)
	out, traces, tokens, contextTokens, err := runToolLoop(ctx, toolLoopConfig{
		SystemPrompt:    systemPrompt,
		UserTask:        task,
		History:         a.History,
		CWD:             a.CWD,
		AgentID:         a.Spec.ID,
		RequireToolCall: requireToolCall,
		AllowedTools:    allowedTools,
		ToolPolicies:    policies,
		LogToolCalls:    logToolCalls,
		ProviderManager: a.ProviderManager,
		ProviderName:    a.ProviderName,
		ModelName:       a.ModelName,
		MaxTokens:       a.MaxTokens,
		Thinking:        a.Thinking,
		RequiredReads:   a.RequiredReads,
		Images:          a.Images,
		ToolCallback:    a.ToolCallback,
		StreamCallback:  a.StreamCallback,
		Permission:      a.Spec.Permission,
		ShellApproval:   a.ShellApproval,
		AskUser:         a.AskUser,
		Manifest:        a.Manifest,
		SandboxState:    a.SandboxState,
		SessionDir:      a.SessionDir,
		DelegationDepth: a.DelegationDepth,
		ParentAgentID:   a.ParentAgentID,
		GoalMode:        a.GoalMode,
		ContextWindow:   a.ContextWindow,
		ShellTimeoutSec: a.ShellTimeoutSec,
		MaxWorkers:      maxWorkers,
		MaxDepth:        maxDelegationDepth,
		MaxToolCalls:    maxToolCallsPerStep,
		SkillsCatalog:   catalog,
	})
	if err != nil {
		return RunResult{}, fmt.Errorf("%s agent: %w", a.Spec.ID, err)
	}
	out = strings.TrimSpace(out)
	out = stripLeakedToolCalls(out)
	main, _ := stripThinkTags(out)
	return RunResult{
		Content:       strings.TrimSpace(main),
		Tools:         traces,
		TokensUsed:    tokens,
		ContextTokens: contextTokens,
	}, nil
}

func compact(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.Join(strings.Fields(s), " ")
	const max = 180
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

// filterDisabledSkills removes skills that have a sentinel `.spettro-disabled`
// file in their directory. The TUI command `/skill disable <name>` writes this
// marker so the user can opt out of a discovered skill without uninstalling.
func filterDisabledSkills(c skills.Catalog) skills.Catalog {
	keep := make([]skills.Skill, 0, len(c.Skills))
	for _, s := range c.Skills {
		flag := filepath.Join(s.Directory, ".spettro-disabled")
		if _, err := os.Stat(flag); err == nil {
			continue
		}
		keep = append(keep, s)
	}
	c.Skills = keep
	return c
}
