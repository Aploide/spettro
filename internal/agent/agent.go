package agent

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	compactpkg "spettro/internal/compact"
	"spettro/internal/config"
	"spettro/internal/memory"
	"spettro/internal/provider"
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
	// FileChanges lists the files the call changed, with their text before
	// and after, on the completion trace of a call that wrote files
	// (file-write, file-edit, rename-symbol). Hosts that render diffs (ACP
	// editors) show them; others ignore the field. See file_changes.go.
	FileChanges []FileChange
	// Narration marks a "comment" trace that carries the model's own prose:
	// text it wrote alongside (or instead of) tool calls in a step (see
	// emitNarration). Other comment traces are the runtime's progress notes.
	// Hosts that keep a transcript show narration as the model's words.
	Narration bool
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
	// Workflows forces the workflow tool on for this run. Hosts normally leave
	// it false and let the "ultracode" keyword in the task turn it on;
	// ignored on sub-agents, which never orchestrate.
	Workflows bool
	// Ultracode is the host's standing ultracode toggle — /ultra, persisted
	// as config.UserConfig.Ultra. Hosts pass it only when the run's
	// effective permission is not ask-first (where every workflow call is
	// refused): cfg.UltraActive() where the run takes the user's level, the
	// TUI's per-agent rule where a spec's own level applies. Every turn then behaves as if the
	// user had written the keyword: the workflow tool is granted, runs are
	// pre-approved, and the agent is told to orchestrate substantive work
	// through workflows by default. Read once at run construction (the
	// system prompt must stay byte-stable per run); ignored on sub-agents.
	Ultracode bool
	// WorkflowSize is the configured size tier for workflow runs
	// (config.WorkflowSizeTier); empty means medium.
	WorkflowSize string
	// WorkflowRuns holds workflow runs paused at a checkpoint so a later
	// tool call — in this turn or a later one — can continue them. Hosts own
	// one per session; nil gives each run a turn-local registry, so a paused
	// run cannot outlive the turn that started it.
	WorkflowRuns  *WorkflowRuns
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
	// CheckpointPrepare, optional next to Checkpoint, lets the runtime take
	// a step's snapshot while the model is still generating: it is called
	// on a background goroutine when the step's request is sent, and
	// returns the prepared snapshot (nil when preparing failed). The step's
	// first mutating call then claims it instead of waiting for Checkpoint.
	// Only this run uses it; sub-agents snapshot through Checkpoint. See
	// checkpoint_policy.go.
	CheckpointPrepare func() PreparedCheckpoint
	Manifest          *config.AgentManifest // for sub-agent spawning via agent tool
	// SandboxState is the session-scoped OS sandbox policy shared across the
	// whole agent tree. nil means the sandbox feature is disabled.
	SandboxState    *SandboxState
	SessionDir      string
	DelegationDepth int
	ParentAgentID   string
	// InstanceID, when set, replaces Spec.ID as the agent identity on emitted
	// ToolTraces (e.g. "general-purpose#3" for a workflow's third member) so
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

// fanOutTools grants the orchestration tool a run is entitled to — the
// workflow tool, when workflows apply to the turn — and returns the guidance
// to append to its system prompt.
//
// The tool bypasses the manifest's PrimaryOnly/handoff gating by design — any
// top-level agent on any model may fan out — and is never granted to a
// sub-agent, which is what stops a workflow member from starting workflows
// of its own. The same rule keeps the ultracode guidance off sub-agents: a
// member told to run a workflow for every task would only be told to do
// something it cannot.
func fanOutTools(allowed []string, workflows workflowGuidance, depth int) ([]string, string) {
	if depth != 0 || !workflows.Enabled {
		return allowed, ""
	}
	if !slices.Contains(allowed, workflowToolID) {
		allowed = append(allowed, workflowToolID)
	}
	return allowed, workflows.prompt()
}

// workflowGuidanceFor decides, once per run, how workflows apply to task:
// whether the tool is granted, which guidance variant the prompt carries,
// and the size and budget it states. allowed is the run's resolved tool list,
// which says what kind of agent the guidance is for.
//
// Ultracode — the keyword in the message, or the host's /ultra toggle —
// selects the standing-mode guidance; a plain-English request ("use a
// workflow") gets the judge-it guidance. A "+500k" budget directive is only
// honoured on a turn that has workflows at all: elsewhere "+500k" is just
// text.
//
// A run paused at a checkpoint in the host's registry grants the tool on its
// own. The checkpoint result tells the model to answer it, and the model very
// often does that by ending its turn to ask the user — whose reply ("yes, fix
// 1 and 3") then carries no keyword. Without the tool on that turn the
// continue the whole design hinges on would be refused, and the run would sit
// paused until the idle reaper stopped it. Such a turn gets the judge-it
// guidance (unless ultracode is on anyway) and no pre-approval for new runs:
// answering a question is not a request for more spending. Sub-agents never
// get any of it — Run clears the guidance below depth 0.
func (a LLMAgent) workflowGuidanceFor(task string, allowed []string) workflowGuidance {
	ultracode := a.Ultracode || WorkflowPreapproved(task)
	requested := a.Workflows || ultracode || WorkflowRequested(task)
	g := workflowGuidance{
		Requested: requested,
		Ultracode: ultracode,
		Research:  !slices.ContainsFunc(allowed, isWorkflowEditTool),
		NoRead:    !slices.ContainsFunc(allowed, isWorkflowReadTool),
		SizeTier:  a.WorkflowSize,
	}
	if a.DelegationDepth == 0 {
		g.Paused = a.WorkflowRuns.Paused()
	}
	g.Enabled = requested || len(g.Paused) > 0
	if g.Enabled {
		if tokens, ok := ParseBudgetDirective(task); ok {
			g.BudgetTokens = tokens
		}
	}
	return g
}

// ultraTurnReminder is appended to the user's message when the /ultra toggle,
// not the message itself, is what turned ultracode on.
//
// The system prompt already carries the standing-mode guidance, but a live
// run showed that is not enough on its own: with /ultra on, "review
// calc/calc.go for bugs" was done solo, while the same request with the
// keyword in it ran a workflow. A word in the user's own turn outweighs a
// paragraph in the system prompt, so the toggle says it there too — the way
// Claude Code confirms a standing ultracode with a reminder on each turn.
// It rides on the user message, not the system prompt, so the cached prefix
// is untouched; and a message that already has the keyword needs no echo.
func ultraTurnReminder(a LLMAgent, task string, g workflowGuidance) string {
	if !a.Ultracode || a.DelegationDepth != 0 || !g.Ultracode || WorkflowPreapproved(task) {
		return ""
	}
	if g.Research {
		return "\n\n<system-reminder>ultra is on (the user's /ultra toggle): the standing ultracode opt-in applies to this message. Investigate it with a workflow — several independent angles, findings verified — unless it is conversational.</system-reminder>"
	}
	return "\n\n<system-reminder>ultra is on (the user's /ultra toggle): the standing ultracode opt-in applies to this message. Author and run a workflow for it unless it is conversational or a trivial mechanical edit.</system-reminder>"
}

// isWorkflowEditTool reports whether tool lets an agent change files itself,
// which is what separates an implementing agent from a planner or a read-only
// Q&A agent for the standing-mode guidance. The shell counts: an agent with
// bash can edit through it. Retired aliases count as their canonical tool.
func isWorkflowEditTool(tool string) bool {
	switch tool {
	case "file-write", "file-edit", "multi-edit", "rename-symbol", "bash", "shell-exec", "pty-start":
		return true
	}
	return false
}

// isWorkflowReadTool reports whether tool lets an agent look at the code
// itself, so it can scout a work-list inline before writing a script.
func isWorkflowReadTool(tool string) bool {
	switch tool {
	case "file-read", "grep", "glob", "ls", "repo-search", "lsp", "bash", "shell-exec":
		return true
	}
	return false
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
	// Workflows are a per-turn opt-in: the user writes the keyword or asks in
	// their own words, or the host's /ultra toggle stands in for the
	// keyword on every turn; a run paused at a checkpoint also keeps the tool
	// for as long as it waits. Detection lives in the runner so every surface
	// (TUI, ACP, goal, Telegram, headless) honours it without each one
	// re-implementing it.
	workflows := a.workflowGuidanceFor(task, allowedTools)
	if a.DelegationDepth != 0 {
		workflows = workflowGuidance{}
	}
	allowedTools, fanOutPrompt = fanOutTools(allowedTools, workflows, a.DelegationDepth)
	systemPrompt += fanOutPrompt
	task += ultraTurnReminder(a, task, workflows)
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
	res, err := runToolLoop(ctx, toolLoopConfig{
		SystemPrompt: systemPrompt,
		UserTask:     task,
		// The keyword (or the /ultra toggle) is a standing yes; a
		// plain-English request is not, and the workflow tool confirms before
		// spending on the latter.
		WorkflowPreapproved: a.Workflows || workflows.Ultracode,
		WorkflowSize:        workflows.SizeTier,
		WorkflowBudget:      workflows.BudgetTokens,
		WorkflowRuns:        a.WorkflowRuns,
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
		CheckpointPrepare:   a.CheckpointPrepare,
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
		SkillsCatalog:       SkillCatalog(projectStateDir(a.CWD)),
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
