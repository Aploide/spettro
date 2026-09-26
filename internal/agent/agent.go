package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	compactpkg "spettro/internal/compact"
	"spettro/internal/config"
	"spettro/internal/memory"
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
	// Images holds file paths of images the tool produced and attached for the
	// model (screenshot, view-image). Hosts that can render images (ACP
	// editors) show them; text-only hosts ignore the field.
	Images []string
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
	// GoalComplete is true when the agent called the goal-complete tool during
	// this run, signalling that the objective has been met. Only meaningful for
	// goal-mode runs (step 03).
	GoalComplete bool
	// GoalSummary is the summary text the agent provided when calling
	// goal-complete. Empty if the agent didn't provide one.
	GoalSummary string
	// Messages is the full structured conversation after the run: the carried
	// prior history plus this turn's user task, every tool call and result,
	// and the final assistant answer. Callers hand it back as the next run's
	// LLMAgent.Messages so the provider request keeps a byte-stable, growing
	// prefix — that stability is what makes prompt caching hit and stops
	// generated tokens from being thrown away between turns. On a failed or
	// cancelled run Messages still carries the conversation accumulated up to
	// the error (the final assistant answer may be missing), so hosts can
	// preserve context across failed turns.
	Messages []provider.Message
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

// GoalModePreamble is prepended to the task for /goal runs. It tells the agent
// the run is autonomous and how to terminate cleanly.
const GoalModePreamble = `You are operating in GOAL MODE. Work autonomously and persistently toward the objective below until it is fully achieved. There is no time pressure — do not stop early, do not ask whether to continue, and do not summarize-and-quit while work remains. The harness runs you in bounded iterations: if a run pauses mid-work you are resumed automatically with your context intact, so just keep working from where you left off.

Rules:
- Break the objective into concrete steps and execute them with tools.
- Verify your work (run builds/tests/linters where relevant) before claiming done.
- If a command is slow (installs, builds), let it run.
- When — and only when — the objective is fully met AND verified, call the goal-complete tool with a short summary and verified=true. Calling goal-complete is the ONLY correct way to finish.
- If you hit a genuine blocker you cannot resolve (missing credentials, ambiguous requirements that change the outcome), explain it clearly in your response so the operator can intervene.

OBJECTIVE:
`

// LLMAgent is the unified agent runner. It reads the agent's system prompt from
// the PromptFile specified in the spec (stripping frontmatter), and runs the
// standard tool loop with the tools, permissions, and limits from the spec.
type LLMAgent struct {
	Spec            config.AgentSpec
	ProviderManager *provider.Manager
	ProviderName    func() string
	ModelName       func() string
	CWD             string
	// MaxTokens is the per-request input token budget (config token_budget);
	// 0 = unlimited.
	MaxTokens int
	// MaxOutputTokens caps each reply (config max_output_tokens); 0 = the
	// provider manager's per-model default.
	MaxOutputTokens int
	Thinking        provider.ThinkingLevel
	// Ultra, when true on a top-level run, injects the ultra fan-out tool and
	// swarm guidance so the agent decomposes hard tasks across many parallel
	// sub-agents. Read once at run construction (the system prompt must stay
	// byte-stable per run); ignored on sub-agents.
	Ultra bool
	// Workflows forces the workflow tool on for this run. Hosts normally leave
	// it false and let the "ultracode" keyword in the task turn it on;
	// ignored on sub-agents, which never orchestrate.
	Workflows     bool
	RequiredReads []string
	Images        []string // attached to this turn's user message (re-sent every step)
	// History is an optional bounded transcript of prior conversation turns,
	// surfaced to the model as a "Conversation so far" section so follow-up
	// turns have memory. Only consulted when Messages is empty — the degraded
	// path for resumed sessions whose structured context was not persisted.
	History string
	// Messages is the structured prior conversation, exactly as returned in
	// the previous RunResult.Messages. Preferred over History: it preserves
	// tool calls/results verbatim and keeps the request prefix cache-stable.
	Messages     []provider.Message
	ToolCallback func(ToolTrace)
	// StreamCallback, when set, receives live thinking/answer chunks as the
	// model streams. Set only on the top-level run (chat/coding/plan/ask).
	StreamCallback StreamCallback
	// UsageCallback, when set, receives per-request token accounting as each
	// LLM call inside the run completes, so hosts can update cost/context
	// displays live instead of waiting for RunResult.
	UsageCallback UsageCallback
	// PermissionFn, when set, supplies the live permission level for every
	// approval decision (instead of the Spec.Permission snapshot), so a
	// mid-run /permission change by the user applies to the rest of the run.
	// An empty return falls back to Spec.Permission.
	PermissionFn  func() config.PermissionLevel
	ShellApproval ShellApprovalCallback
	AskUser       AskUserCallback
	// Checkpoint, when set, is called synchronously before the first
	// file-modifying tool call of each step (including in sub-agents sharing
	// this checkout) so the host can snapshot files + conversation for
	// /rewind. See checkpoint_policy.go for what counts as file-modifying.
	Checkpoint func(tool string)
	Manifest   *config.AgentManifest // for sub-agent spawning via agent tool
	// SandboxState is the session-scoped OS sandbox policy shared across the
	// whole agent tree. nil means the sandbox feature is disabled.
	SandboxState    *SandboxState
	SessionDir      string
	DelegationDepth int
	ParentAgentID   string
	// InstanceID, when set, replaces Spec.ID as the agent identity on emitted
	// ToolTraces (e.g. "code#3" for the third member of an Ultra swarm) so
	// hosts can tell concurrent same-type sub-agents apart. Prompt, tool, and
	// handoff resolution still use Spec.ID.
	InstanceID string

	// GoalMode enables generous tool timeouts and (step 03) goal-complete
	// signaling. Non-goal runs behave exactly as before.
	GoalMode        bool
	ContextWindow   int // model context window in tokens; drives in-loop compaction. 0 → default
	ShellTimeoutSec int // goal-mode per shell/bash timeout; 0 → default
	// MaxSteps bounds the number of LLM steps in this run; 0 = unlimited.
	// Goal hosts set it per iteration so the run yields back to the outer goal
	// loop (progress check, stall guard, iteration cap) instead of running one
	// endless first iteration.
	MaxSteps int
	// Compact carries the user's auto-compaction settings into the run loop
	// (typically cfg.CompactConfig()). Zero value → defaults (enabled, 85%).
	Compact compactpkg.Config

	// parentSnapshot and parentCWD carry a parent run's environment
	// snapshot to a sub-agent (set by the delegating runtime, never by hosts).
	parentSnapshot string
	parentCWD      string

	// Steering, when set, lets the host inject user guidance while the run is
	// executing: the tool loop drains it at every step boundary and appends
	// each message as a user turn (append-only, so prompt caching still hits).
	Steering *SteeringQueue
}

// fanOutTools grants the orchestration tools a run is entitled to and returns
// the guidance to append to its system prompt.
//
// Both tools bypass the manifest's PrimaryOnly/handoff gating by design — any
// top-level agent on any model may fan out — and neither is ever granted to a
// sub-agent, which is what stops a swarm from spawning swarms.
func fanOutTools(allowed []string, ultra, workflows bool, depth int) ([]string, string) {
	if depth != 0 {
		return allowed, ""
	}
	prompt := ""
	if ultra {
		if !slices.Contains(allowed, ultraToolID) {
			allowed = append(allowed, ultraToolID)
		}
		prompt += ultraPromptSection
	}
	if workflows {
		if !slices.Contains(allowed, workflowToolID) {
			allowed = append(allowed, workflowToolID)
		}
		prompt += workflowPromptSection
	}
	return allowed, prompt
}

func (a LLMAgent) Run(ctx context.Context, task string) (RunResult, error) {
	task = strings.TrimSpace(task)
	if task == "" {
		return RunResult{}, fmt.Errorf("empty task")
	}
	systemPrompt := loadPromptOrFallback(a.CWD, a.Spec.PromptFile, a.Spec.Description)
	// Persistent cross-session memory: the snapshot is loaded once per process
	// and frozen (see memory.SessionContext), so appending it here keeps the
	// system prompt byte-stable across every turn of the session.
	systemPrompt += memory.SessionContext(a.CWD)
	allowedTools, policies := resolveToolPolicies(a.Spec, a.Manifest)
	var fanOutPrompt string
	// Workflows are a per-turn opt-in: the user writes the keyword in their
	// message, or a host sets the flag. Detection lives in the runner so every
	// surface (TUI, ACP, goal, Telegram, headless) honours the keyword without
	// each one re-implementing it.
	allowedTools, fanOutPrompt = fanOutTools(allowedTools, a.Ultra, a.Workflows || WorkflowRequested(task), a.DelegationDepth)
	systemPrompt += fanOutPrompt
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
	res, err := runToolLoop(ctx, toolLoopConfig{
		SystemPrompt: systemPrompt,
		UserTask:     task,
		// The keyword is a standing yes; a plain-English request is not, and
		// the workflow tool confirms before spending on the latter.
		WorkflowPreapproved: a.Workflows || WorkflowPreapproved(task),
		History:             a.History,
		Messages:            a.Messages,
		CWD:                 a.CWD,
		AgentID:             a.Spec.ID,
		InstanceID:          a.InstanceID,
		AllowedTools:        allowedTools,
		ToolPolicies:        policies,
		LogToolCalls:        logToolCalls,
		ProviderManager:     a.ProviderManager,
		ProviderName:        a.ProviderName,
		ModelName:           a.ModelName,
		MaxTokens:           a.MaxTokens,
		MaxOutputTokens:     a.MaxOutputTokens,
		parentSnapshot:      a.parentSnapshot,
		parentCWD:           a.parentCWD,
		Thinking:            a.Thinking,
		RequiredReads:       a.RequiredReads,
		Images:              a.Images,
		ToolCallback:        a.ToolCallback,
		StreamCallback:      a.StreamCallback,
		UsageCallback:       a.UsageCallback,
		Permission:          a.Spec.Permission,
		PermissionFn:        a.PermissionFn,
		ShellApproval:       a.ShellApproval,
		AskUser:             a.AskUser,
		Checkpoint:          a.Checkpoint,
		Manifest:            a.Manifest,
		SandboxState:        a.SandboxState,
		SessionDir:          a.SessionDir,
		DelegationDepth:     a.DelegationDepth,
		ParentAgentID:       a.ParentAgentID,
		GoalMode:            a.GoalMode,
		ContextWindow:       a.ContextWindow,
		Compact:             a.Compact,
		ShellTimeoutSec:     a.ShellTimeoutSec,
		MaxSteps:            a.MaxSteps,
		MaxWorkers:          maxWorkers,
		MaxDepth:            maxDelegationDepth,
		MaxToolCalls:        maxToolCallsPerStep,
		SkillsCatalog:       catalog,
		Steering:            a.Steering,
	})
	if err != nil {
		// Preserve the partial conversation so hosts can carry it into the
		// next turn: a failed or cancelled run must not wipe the context the
		// user already built up (tool results, steering, prior steps).
		// Traces and token use come back too, so a delegating parent can
		// report what a failed or timed-out sub-agent already did.
		return RunResult{Messages: res.messages, Tools: res.traces, TokensUsed: res.tokens}, fmt.Errorf("%s agent: %w", a.Spec.ID, err)
	}
	out := strings.TrimSpace(res.content)
	out = stripLeakedToolCalls(out)
	main, _ := stripThinkTags(out)
	return RunResult{
		Content:       strings.TrimSpace(main),
		Tools:         res.traces,
		TokensUsed:    res.tokens,
		ContextTokens: res.contextTokens,
		GoalComplete:  res.goalComplete,
		GoalSummary:   res.goalSummary,
		Messages:      res.messages,
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
