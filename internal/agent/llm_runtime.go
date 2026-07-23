package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"spettro/internal/budget"
	compactpkg "spettro/internal/compact"
	"spettro/internal/config"
	"spettro/internal/diff"
	"spettro/internal/hooks"
	"spettro/internal/provider"
	"spettro/internal/session"
	"spettro/internal/skills"
)

const (
	toolCallPrefix = "TOOL_CALL"
	finalPrefix    = "FINAL"
)

const codingSystemPromptFallback = `You are a coding agent that can use tools.
Implement the task using minimal safe edits and verify your changes.
Do not include <think> blocks in your FINAL answer; put reasoning in the thinking channel if the model supports it.`

type LLMCoder struct {
	ProviderManager *provider.Manager
	ProviderName    func() string
	ModelName       func() string
	CWD             string
	MaxTokens       int // max tokens per request; 0 = unlimited
	RequiredReads   []string
	ToolCallback    func(ToolTrace) // optional: called with status="running" before and final status after each tool
	ShellApproval   ShellApprovalCallback
	AskUser         AskUserCallback
}

type ShellApprovalDecision string

const (
	ShellApprovalAllowOnce   ShellApprovalDecision = "allow-once"
	ShellApprovalAllowAlways ShellApprovalDecision = "allow-always"
	ShellApprovalDeny        ShellApprovalDecision = "deny"
)

type ShellApprovalRequest struct {
	Command  string
	ToolID   string
	Segments []string
	Reason   string
	// Diff is a plain unified diff of the proposed file change (file-write /
	// file-edit approvals only); the UI renders it so the user sees exactly
	// what will change before approving.
	Diff string
}

type ShellApprovalCallback func(context.Context, ShellApprovalRequest) (ShellApprovalDecision, error)

type AskUserRequest struct {
	Question          string
	Options           []string
	Context           string
	DefaultOption     string
	AllowFreeResponse bool
}

type AskUserCallback func(context.Context, AskUserRequest) (string, error)

func (c LLMCoder) Execute(ctx context.Context, plan string, level config.PermissionLevel, approved bool) (RunResult, error) {
	if strings.TrimSpace(plan) == "" {
		return RunResult{}, fmt.Errorf("empty approved plan")
	}
	if level == config.PermissionAskFirst && !approved {
		return RunResult{}, fmt.Errorf("ask-first policy requires explicit approval")
	}

	systemPrompt := loadPromptOrFallback(c.CWD, "agents/coding.md", codingSystemPromptFallback)
	thinking := provider.ThinkingLevel("")
	if c.ProviderManager.SupportsReasoning(c.ProviderName(), c.ModelName()) {
		thinking = provider.ThinkingMedium
	}
	res, err := runToolLoop(ctx, toolLoopConfig{
		SystemPrompt:    systemPrompt,
		UserTask:        plan,
		CWD:             c.CWD,
		AllowedTools:    []string{"repo-search", "file-read", "file-write", "shell-exec", "job-output", "job-kill", "tool-output", "glob", "grep", "diagnostics", "references", "hover", "rename-symbol"},
		LogToolCalls:    true,
		ProviderManager: c.ProviderManager,
		ProviderName:    c.ProviderName,
		ModelName:       c.ModelName,
		MaxTokens:       c.MaxTokens,
		Thinking:        thinking,
		RequiredReads:   c.RequiredReads,
		ToolCallback:    c.ToolCallback,
		Permission:      level,
		ShellApproval:   c.ShellApproval,
		AskUser:         c.AskUser,
	})
	if err != nil {
		return RunResult{}, err
	}
	main, _ := stripThinkTags(res.content)
	return RunResult{
		Content:       strings.TrimSpace(main),
		Tools:         res.traces,
		TokensUsed:    res.tokens,
		ContextTokens: res.contextTokens,
		Messages:      res.messages,
	}, nil
}

type toolLoopConfig struct {
	SystemPrompt string
	UserTask     string
	// History is an optional bounded transcript of prior conversation turns
	// (user/assistant), rendered into the prompt as a "Conversation so far"
	// section before the current Task. Empty means a fresh, first-turn run.
	// Only consulted when Messages is empty (legacy/degraded path, e.g. the
	// first turn after resuming a session saved without structured context).
	History string
	// Messages is the structured prior conversation carried across turns,
	// exactly as returned by the previous run (assistant turns, tool calls and
	// tool results included). When non-empty the loop appends a task-only user
	// turn to it instead of rebuilding a first message, keeping the request
	// prefix byte-identical with prior requests so provider prompt caching
	// keeps hitting and no generated tokens are discarded between turns.
	Messages []provider.Message
	CWD      string
	AgentID  string
	// InstanceID, when set, is the per-instance display name (e.g. "code#3")
	// stamped on every ToolTrace this run emits, so hosts can tell apart
	// concurrent sub-agents of the same type. AgentID keeps the manifest spec
	// ID for prompt/handoff resolution; InstanceID only affects observability.
	InstanceID string
	// GoalMode enables generous tool timeouts and (step 03) goal-complete
	// signaling. Non-goal runs behave exactly as before.
	GoalMode        bool
	ContextWindow   int // model context window in tokens; drives in-loop compaction. 0 → default
	ShellTimeoutSec int // goal-mode per shell/bash timeout; 0 → default
	// Compact is the auto-compaction policy for the in-loop trigger (user
	// settings: off switch, threshold percent, failure pause). The zero value
	// means defaults (enabled at 85%), so hosts that don't wire user config
	// keep auto-compaction on.
	Compact         compactpkg.Config
	AllowedTools    []string
	ToolPolicies    map[string]config.ToolSpec
	LogToolCalls    bool
	ProviderManager *provider.Manager
	ProviderName    func() string
	ModelName       func() string
	MaxTokens       int                    // max tokens per request; 0 = unlimited
	Thinking        provider.ThinkingLevel // forwarded to provider.Request.Thinking
	RequiredReads   []string
	Images          []string        // attached to this turn's user message (re-sent every step)
	ToolCallback    func(ToolTrace) // optional: called with status="running" before and final status after each tool
	// StreamCallback, when set, receives demultiplexed thinking/answer chunks as
	// the model streams. Only the top-level run sets it; sub-agents stay silent.
	StreamCallback StreamCallback
	// UsageCallback, when set, receives per-request token accounting as each
	// LLM call completes. Only the top-level run sets it; sub-agents stay
	// silent (their cost surfaces through the parent's tool results).
	UsageCallback UsageCallback
	Permission    config.PermissionLevel
	// PermissionFn, when set, is consulted on every approval decision instead
	// of the static Permission snapshot, so the user can change the permission
	// level (e.g. to yolo) while a run is in flight and have it take effect
	// immediately. An empty return falls back to Permission.
	PermissionFn  func() config.PermissionLevel
	ShellApproval ShellApprovalCallback
	AskUser       AskUserCallback
	// Checkpoint, when set, is invoked synchronously right before any
	// file-modifying tool executes (file-write, file-edit, shell), so the host
	// can snapshot the working tree and conversation for /rewind.
	Checkpoint      func(tool string)
	Manifest        *config.AgentManifest
	SandboxState    *SandboxState // session-scoped OS sandbox policy; nil = disabled
	SessionDir      string
	DelegationDepth int
	ParentAgentID   string
	MaxWorkers      int
	MaxMicroagents  int
	MaxDepth        int
	MaxToolCalls    int            // max tool calls per LLM step (0 → default 32)
	SkillsCatalog   skills.Catalog // discovered skills to disclose in prompts
	// Steering, when set, is drained at every step boundary; each pending
	// message is appended to the conversation as a user turn so the model sees
	// it before its next step. Top-level runs only — sub-agents never get one.
	Steering *SteeringQueue
}

// traceID is the agent identity stamped on emitted ToolTraces: the unique
// per-instance name when one was assigned (swarm members), else the spec ID.
func (r *toolRuntime) traceID() string {
	if r.instanceID != "" {
		return r.instanceID
	}
	return r.agentID
}

type toolCall struct {
	Tool string          `json:"tool"`
	Args json.RawMessage `json:"args"`
}

type toolRuntime struct {
	cwd           string
	mu            sync.Mutex
	shellMu       sync.Mutex
	readSet       map[string]struct{}
	requiredReads map[string]struct{}
	searcher      RepoSearcher
	permission    config.PermissionLevel
	permissionFn  func() config.PermissionLevel
	shellApproval ShellApprovalCallback
	askUser       AskUserCallback
	allowedShell  map[string]struct{}
	toolPolicies  map[string]config.ToolSpec
	logToolCalls  bool
	runtimeRules  []config.PermissionRule
	agentRules    []config.PermissionRule
	sandboxState  *SandboxState
	// sub-agent support
	manifest      *config.AgentManifest
	providerMgr   *provider.Manager
	providerName  func() string
	modelName     func() string
	maxTokens     int
	thinkingLevel provider.ThinkingLevel
	toolCallback  func(ToolTrace)
	checkpoint    func(tool string)
	sessionDir    string
	agentID       string
	instanceID    string
	parentID      string

	delegationDepth      int
	maxParallelWorkers   int
	maxParallelMicroagnt int
	maxDelegationDepth   int
	maxToolCallsPerStep  int
	hooksConfig          hooks.EffectiveConfig
	stopRequested        bool
	stopReason           string
	skillsCatalog        skills.Catalog
	goalMode             bool
	shellTimeoutSec      int
	// compactCfg is the auto-compaction policy (zero value → defaults);
	// compactFailures counts consecutive summarizer failures so the trigger
	// pauses after MaxFailures instead of burning a failing call every step.
	compactCfg      compactpkg.Config
	compactFailures int
	goalComplete    bool
	goalSummary     string
	goalVerified    bool

	// httpClient overrides the hardened SSRF-safe client used by web-fetch,
	// web-search and download. Nil in production (the safe client is built per
	// call); tests inject a plain client so httptest loopback servers work.
	httpClient *http.Client

	// Model fallback routing (manifest [runtime.fallback]). modelOverride is
	// set once the user consents to a switch and pins the rest of the run to
	// the fallback model; fallbackTried prevents re-offering a model that
	// already failed this run.
	fallbackMode     config.FallbackMode
	fallbackChain    []provider.ModelRef
	internalModelRef provider.ModelRef
	modelOverride    *provider.ModelRef
	fallbackTried    map[provider.ModelRef]bool

	// loopDetect spots the agent repeating itself (manifest
	// [runtime.loop_detection]); nil when disabled.
	loopDetect *loopDetector

	// visionCheck overrides the provider manager's SupportsVision lookup for
	// the view-image tool. Nil in production (test seam).
	visionCheck func() bool
}

// perm returns the permission level to enforce right now: the host's live
// selection when a PermissionFn is wired (so mid-run /permission changes take
// effect immediately), otherwise the level captured at run start.
func (r *toolRuntime) perm() config.PermissionLevel {
	if r.permissionFn != nil {
		if p := r.permissionFn(); p != "" {
			return p
		}
	}
	return r.permission
}

// effectiveModel returns the model the main loop should call: the consented
// fallback override if one is active, otherwise the live UI selection.
func (r *toolRuntime) effectiveModel() provider.ModelRef {
	if r.modelOverride != nil {
		return *r.modelOverride
	}
	return provider.ModelRef{Provider: r.providerName(), Model: r.modelName()}
}

// offerFallback decides whether the failed main-loop request should be
// retried on a fallback model. Transient availability failures only. The
// user is asked before any main-thread switch (a swap invalidates the prompt
// cache); FallbackSilent skips the question only when no interactive prompt
// is available (headless). Returns the model to switch to and true to retry.
func (r *toolRuntime) offerFallback(ctx context.Context, failed provider.ModelRef, cause error) (provider.ModelRef, bool) {
	if r.fallbackMode == config.FallbackOff || len(r.fallbackChain) == 0 {
		return provider.ModelRef{}, false
	}
	kind := provider.Classify(cause)
	if !kind.Transient() {
		return provider.ModelRef{}, false
	}
	if r.fallbackTried == nil {
		r.fallbackTried = map[provider.ModelRef]bool{}
	}
	r.fallbackTried[failed] = true
	var next provider.ModelRef
	for _, ref := range r.fallbackChain {
		if ref == failed || r.fallbackTried[ref] {
			continue
		}
		if r.providerMgr != nil && !r.providerMgr.HasModel(ref.Provider, ref.Model) {
			continue
		}
		next = ref
		break
	}
	if next.IsZero() {
		return provider.ModelRef{}, false
	}
	if r.askUser == nil {
		// No way to ask: only proceed when the manifest explicitly opts into
		// silent switching; never silently swap the main thread otherwise.
		if r.fallbackMode == config.FallbackSilent {
			return next, true
		}
		return provider.ModelRef{}, false
	}
	switchOpt := fmt.Sprintf("Switch to %s", next)
	answer, err := r.askUser(ctx, AskUserRequest{
		Question:      fmt.Sprintf("Model %s is unavailable (%s). Switch to %s for the rest of this run? Note: switching models invalidates the prompt cache.", failed, kind, next),
		Options:       []string{switchOpt, "Abort"},
		Context:       truncate(cause.Error(), 300),
		DefaultOption: switchOpt,
	})
	if err != nil || !strings.HasPrefix(strings.ToLower(strings.TrimSpace(answer)), "switch") {
		return provider.ModelRef{}, false
	}
	return next, true
}

// parallelResult holds the outcome of a single tool execution in a parallel batch.
type parallelResult struct {
	agentID string
	name    string
	args    string
	output  string
	status  string
	// images are file paths attached by the tool for the model to see
	// (collected via the per-call image sink; see view_image.go).
	images []string
}

// toolLoopResult carries everything a run produces.
//
// tokens is the cumulative count across all LLM calls (sum of every step's
// prompt+completion — the session COST); contextTokens approximates context
// occupancy (the largest single-step prompt+completion — see EFF-3). They are
// deliberately distinct: each step's prompt re-embeds the rolling history, so
// summing every step double-counts the same context. The gauge must use
// occupancy, not the cumulative sum.
//
// messages is the full post-run conversation (prior history + this turn's
// user task, tool calls/results, and the final assistant answer). Callers
// pass it back as the next turn's cfg.Messages so the provider sees a stable,
// growing prefix — the prompt-cache contract.
type toolLoopResult struct {
	content       string
	traces        []ToolTrace
	tokens        int
	contextTokens int
	goalComplete  bool
	goalSummary   string
	messages      []provider.Message
}

func runToolLoop(ctx context.Context, cfg toolLoopConfig) (toolLoopResult, error) {
	if cfg.ProviderManager == nil {
		return toolLoopResult{}, fmt.Errorf("missing provider manager")
	}
	if cfg.ProviderName == nil || cfg.ModelName == nil {
		return toolLoopResult{}, fmt.Errorf("missing provider/model selectors")
	}
	if strings.TrimSpace(cfg.UserTask) == "" {
		return toolLoopResult{}, fmt.Errorf("empty task")
	}

	var totalTokens int
	// contextTokens tracks the largest single-step request size, used as the
	// approximate current context occupancy for the compaction gauge.
	var contextTokens int
	allowed := make(map[string]struct{}, len(cfg.AllowedTools))
	for _, t := range cfg.AllowedTools {
		allowed[t] = struct{}{}
		if spec, ok := cfg.ToolPolicies[t]; ok {
			for _, alias := range spec.Aliases {
				alias = strings.TrimSpace(alias)
				if alias != "" {
					allowed[alias] = struct{}{}
				}
			}
		}
	}
	runtime := toolRuntime{
		cwd:             cfg.CWD,
		searcher:        NewRepoSearcher(cfg.CWD),
		readSet:         map[string]struct{}{},
		requiredReads:   map[string]struct{}{},
		permission:      cfg.Permission,
		permissionFn:    cfg.PermissionFn,
		shellApproval:   cfg.ShellApproval,
		askUser:         cfg.AskUser,
		allowedShell:    map[string]struct{}{},
		toolPolicies:    map[string]config.ToolSpec{},
		logToolCalls:    cfg.LogToolCalls,
		sandboxState:    cfg.SandboxState,
		manifest:        cfg.Manifest,
		providerMgr:     cfg.ProviderManager,
		providerName:    cfg.ProviderName,
		modelName:       cfg.ModelName,
		maxTokens:       cfg.MaxTokens,
		toolCallback:    cfg.ToolCallback,
		checkpoint:      cfg.Checkpoint,
		sessionDir:      cfg.SessionDir,
		agentID:         cfg.AgentID,
		instanceID:      cfg.InstanceID,
		parentID:        cfg.ParentAgentID,
		delegationDepth: cfg.DelegationDepth,
		skillsCatalog:   cfg.SkillsCatalog,
		compactCfg:      cfg.Compact,
	}
	var loopPolicy config.LoopDetectionPolicy
	if cfg.Manifest != nil {
		loopPolicy = cfg.Manifest.Runtime.LoopDetection
	}
	runtime.loopDetect = newLoopDetector(loopPolicy)
	if !cfg.LogToolCalls {
		runtime.logToolCalls = false
	}
	if cfg.Manifest != nil {
		fb := cfg.Manifest.Runtime.Fallback
		runtime.fallbackMode = fb.Mode
		// Refs are validated at manifest load; a parse error here just means
		// no chain, never a failed run.
		if chain, err := provider.ParseModelRefs(fb.Chain); err == nil {
			runtime.fallbackChain = chain
		}
		if ref, err := provider.ParseModelRef(fb.InternalModel); err == nil {
			runtime.internalModelRef = ref
		}
		runtime.runtimeRules = append(runtime.runtimeRules, cfg.Manifest.Runtime.PermissionRules...)
		if spec, ok := cfg.Manifest.AgentByID(cfg.AgentID); ok {
			runtime.agentRules = append(runtime.agentRules, spec.PermissionRules...)
		}
	}
	for id, spec := range cfg.ToolPolicies {
		runtime.toolPolicies[id] = spec
		for _, alias := range spec.Aliases {
			alias = strings.TrimSpace(alias)
			if alias != "" {
				runtime.toolPolicies[alias] = spec
			}
		}
	}
	if cfg.MaxWorkers <= 0 {
		cfg.MaxWorkers = 2
	}
	if cfg.MaxMicroagents <= 0 {
		cfg.MaxMicroagents = 2
	}
	if cfg.MaxDepth <= 0 {
		cfg.MaxDepth = 2
	}
	if cfg.MaxToolCalls <= 0 {
		cfg.MaxToolCalls = 32
	}
	runtime.maxParallelWorkers = cfg.MaxWorkers
	runtime.maxParallelMicroagnt = cfg.MaxMicroagents
	runtime.maxDelegationDepth = cfg.MaxDepth
	runtime.maxToolCallsPerStep = cfg.MaxToolCalls
	runtime.goalMode = cfg.GoalMode
	runtime.shellTimeoutSec = cfg.ShellTimeoutSec
	allowedShell, err := loadAllowedCommandSet(cfg.CWD)
	if err != nil {
		return toolLoopResult{}, err
	}
	runtime.allowedShell = allowedShell
	hooksCfg, err := hooks.LoadEffective(cfg.CWD)
	if err != nil {
		return toolLoopResult{}, err
	}
	runtime.hooksConfig = hooksCfg
	if err := runtime.runSessionStartHooks(ctx); err != nil {
		return toolLoopResult{}, err
	}
	for _, p := range cfg.RequiredReads {
		p = filepath.ToSlash(strings.TrimSpace(p))
		if p != "" {
			runtime.requiredReads[p] = struct{}{}
		}
	}
	var traces []ToolTrace

	// Native tool calling is always used: tool schemas ride on the API request
	// for every model. The spec list is built once (it doesn't change between
	// steps). Models whose catalog entry claims no tool support still get the
	// schemas — local OpenAI-compatible servers accept them, and the old
	// TOOL_CALL text-protocol fallback caused tool-capable local models to
	// emit unparsed TOOL_CALL strings instead of real tool calls.
	nativeToolSpecs := buildToolSpecs(cfg.AllowedTools)

	// Seed the message array. With a carried structured history the new turn is
	// appended after it — the carried prefix must stay byte-identical to what
	// the provider already cached. Without one, the first user turn is built
	// fresh (task + working dir + optional legacy history transcript).
	var convMsgs []provider.Message
	if len(cfg.Messages) > 0 {
		convMsgs = make([]provider.Message, 0, len(cfg.Messages)+8)
		convMsgs = append(convMsgs, cfg.Messages...)
		convMsgs = append(convMsgs, provider.Message{Role: provider.RoleUser, Content: buildTurnUserMessage(cfg), Images: cfg.Images})
	} else {
		convMsgs = []provider.Message{
			{Role: provider.RoleUser, Content: buildInitialUserMessage(cfg), Images: cfg.Images},
		}
	}

	// finish appends the final assistant turn so the returned conversation is
	// complete and reusable as the next turn's prefix.
	finish := func(content string, goalDone bool, goalSummary string) (toolLoopResult, error) {
		if strings.TrimSpace(content) != "" {
			last := len(convMsgs) - 1
			if last < 0 || convMsgs[last].Role != provider.RoleAssistant || convMsgs[last].Content != content {
				convMsgs = append(convMsgs, provider.Message{Role: provider.RoleAssistant, Content: content})
			}
		}
		return toolLoopResult{
			content:       content,
			traces:        traces,
			tokens:        totalTokens,
			contextTokens: contextTokens,
			goalComplete:  goalDone,
			goalSummary:   goalSummary,
			messages:      convMsgs,
		}, nil
	}

	// fail is the error-path counterpart of finish: it returns the error
	// together with the conversation accumulated so far, so hosts can keep
	// the partial history (user task, tool calls and results) as the next
	// turn's carried prefix instead of losing the whole turn's context.
	// convMsgs is append-only up to any error point, so it is always a valid
	// provider prefix (tool calls are never left without their results).
	fail := func(err error) (toolLoopResult, error) {
		return toolLoopResult{
			traces:        traces,
			tokens:        totalTokens,
			contextTokens: contextTokens,
			messages:      convMsgs,
		}, err
	}

	// The system prompt is intentionally built once: it must not vary between
	// steps or the provider-side prompt cache misses on every call.
	system := buildSystemString(cfg)

	// Resilience state: transient provider failures are retried in-loop and an
	// over-budget context gets one forced compaction attempt, so a single bad
	// step (huge tool output, provider hiccup) doesn't kill the whole run.
	const maxSendRetries = 2
	sendRetries := 0
	budgetCompacted := false

	for {
		// Mid-run steering: deliver any guidance the user typed while the run
		// was executing. Each message is appended as a user turn at this step
		// boundary — the conversation only ever grows, so the cached prompt
		// prefix stays valid. (Tool results from the previous step are already
		// in convMsgs at this point, so a steering turn can never split an
		// assistant tool-call message from its results.)
		if cfg.Steering != nil {
			for _, s := range cfg.Steering.Drain() {
				convMsgs = append(convMsgs, provider.Message{Role: provider.RoleUser, Content: steeringMessagePrefix + s})
				if cfg.ToolCallback != nil {
					cfg.ToolCallback(ToolTrace{AgentID: runtime.traceID(), Name: "comment", Status: "success", Output: "steering delivered: " + truncate(s, 200)})
				}
			}
		}
		// In-loop compaction: summarize older turns when context pressure
		// approaches the window. Fires for all runs (the loop is unbounded),
		// honoring the user's auto-compact settings via runtime.compactCfg.
		// On error, keep convMsgs as-is — never abort a run for compaction;
		// the trigger fires again at the next step until MaxFailures pauses it.
		beforeTokens := compactpkg.EstimateHistoryTokens(system, convMsgs)
		if compacted, did, err := runtime.compactConv(ctx, system, convMsgs, cfg.ContextWindow, false); err != nil {
			if cfg.ToolCallback != nil {
				cfg.ToolCallback(ToolTrace{AgentID: runtime.traceID(), Name: "comment", Status: "success", Output: fmt.Sprintf("auto-compaction failed (%s) — continuing; will retry at the next threshold crossing", truncate(err.Error(), 200))})
			}
		} else {
			convMsgs = compacted
			if did && cfg.ToolCallback != nil {
				afterTokens := compactpkg.EstimateHistoryTokens(system, convMsgs)
				cfg.ToolCallback(ToolTrace{AgentID: runtime.traceID(), Name: "comment", Status: "success", Output: fmt.Sprintf("compacted %s → %s tokens to stay within the context window", formatTokens(beforeTokens), formatTokens(afterTokens))})
			}
		}
		// Budget validation: sum system + all messages.
		allContent := make([]string, 0, 1+len(convMsgs))
		allContent = append(allContent, system)
		for _, m := range convMsgs {
			allContent = append(allContent, m.Content)
		}
		if err := budget.Validate(cfg.MaxTokens, allContent...); err != nil {
			// Over budget (e.g. an oversized tool result blew up the history):
			// force-compact once instead of failing the run. Only if forced
			// compaction doesn't help either does the run error out.
			if !budgetCompacted {
				budgetCompacted = true
				if compacted, did, cerr := runtime.compactConv(ctx, system, convMsgs, cfg.ContextWindow, true); cerr == nil && did {
					convMsgs = compacted
					if cfg.ToolCallback != nil {
						cfg.ToolCallback(ToolTrace{AgentID: runtime.traceID(), Name: "comment", Status: "success", Output: "context exceeded the token budget — force-compacted history and continuing"})
					}
					continue
				}
			}
			return fail(err)
		}
		budgetCompacted = false
		req := provider.Request{
			System:    system,
			Messages:  convMsgs,
			MaxTokens: cfg.MaxTokens,
			Thinking:  cfg.Thinking,
		}
		if len(nativeToolSpecs) > 0 {
			req.Tools = nativeToolSpecs
		}
		if cfg.ToolCallback != nil {
			req.OnRateLimit = func(d time.Duration) {
				cfg.ToolCallback(ToolTrace{AgentID: runtime.traceID(), Name: "comment", Status: "success", Output: fmt.Sprintf("rate limited, waiting %ds before retrying...", int(d.Round(time.Second).Seconds()))})
			}
		}
		var demux *streamDemux
		if cfg.StreamCallback != nil {
			// Clear any draft left by a previous step before this one streams.
			cfg.StreamCallback(StreamChunk{Kind: StreamKindAnswer, Reset: true})
			demux = newStreamDemux(cfg.StreamCallback)
			req.OnStream = func(ev provider.StreamEvent) {
				switch ev.Kind {
				case provider.StreamReasoning:
					demux.reasoning(ev.Delta)
				case provider.StreamText:
					demux.text(ev.Delta)
				}
			}
		}
		model := runtime.effectiveModel()
		resp, err := cfg.ProviderManager.Send(ctx, model.Provider, model.Model, req)
		if demux != nil {
			demux.flush()
		}
		if err != nil {
			// User cancellation is not retryable — surface it immediately.
			if ctx.Err() != nil {
				return fail(fmt.Errorf("agent call failed: %w", err))
			}
			// Transient failure (5xx/timeout/network): retry the same request a
			// bounded number of times before considering a fallback model, so a
			// single provider hiccup doesn't kill the whole run.
			if sendRetries < maxSendRetries {
				sendRetries++
				if cfg.ToolCallback != nil {
					cfg.ToolCallback(ToolTrace{AgentID: runtime.traceID(), Name: "comment", Status: "success", Output: fmt.Sprintf("provider call failed (%s) — retrying (%d/%d)...", truncate(err.Error(), 180), sendRetries, maxSendRetries)})
				}
				select {
				case <-time.After(time.Duration(sendRetries) * 2 * time.Second):
				case <-ctx.Done():
					return fail(ctx.Err())
				}
				continue
			}
			// Availability failure (quota/5xx/timeout): offer the configured
			// fallback model instead of failing the turn. The switch requires
			// user consent on interactive runs and pins the rest of the run.
			if next, ok := runtime.offerFallback(ctx, model, err); ok {
				runtime.modelOverride = &next
				sendRetries = 0
				if cfg.ToolCallback != nil {
					cfg.ToolCallback(ToolTrace{AgentID: runtime.traceID(), Name: "comment", Status: "success", Output: fmt.Sprintf("model %s unavailable — switched to fallback %s for the rest of this run", model, next)})
				}
				continue
			}
			return fail(fmt.Errorf("agent call failed: %w", err))
		}
		sendRetries = 0
		totalTokens += resp.EstimatedTokens
		// Occupancy ~= the largest single request (prompt+completion). The
		// last step usually has the most accumulated history, but using the
		// max is robust even if the final completion is short.
		if resp.EstimatedTokens > contextTokens {
			contextTokens = resp.EstimatedTokens
		}
		if cfg.UsageCallback != nil {
			cfg.UsageCallback(UsageEvent{
				StepTokens:    resp.EstimatedTokens,
				TotalTokens:   totalTokens,
				ContextTokens: contextTokens,
				Usage:         resp.Usage,
			})
		}

		content := strings.TrimSpace(resp.Content)
		main, _ := stripThinkTags(content)
		main = strings.TrimSpace(main)
		if main == "" && len(resp.ToolCalls) == 0 {
			continue
		}

		// Native tool-calling path: model returned structured tool calls.
		if len(resp.ToolCalls) > 0 {
			emitNarration(cfg, main)
			internalCalls := make([]toolCall, len(resp.ToolCalls))
			for i, tc := range resp.ToolCalls {
				internalCalls[i] = toolCall{Tool: tc.Name, Args: tc.Args}
			}
			// Loop check before execution: an abort skips the repeated calls
			// entirely (the assistant turn is not yet in the history, so the
			// carried prefix stays valid); a nudge lets the step run and is
			// injected alongside the tool results below.
			loopAct := runtime.loopDetect.observe(internalCalls, main)
			if loopAct == loopAbort {
				return finish(loopStopMessage, false, "")
			}
			results := runtime.parallelExec(ctx, internalCalls, allowed, cfg.ToolCallback)
			convMsgs = append(convMsgs, provider.Message{
				Role:      provider.RoleAssistant,
				Content:   main,
				ToolCalls: resp.ToolCalls,
			})
			toolResults := make([]provider.ToolResult, len(results))
			for i, res := range results {
				traces = append(traces, ToolTrace{AgentID: res.agentID, Name: res.name, Status: res.status, Args: res.args, Output: truncate(res.output, 600), Images: res.images})
				toolResults[i] = provider.ToolResult{
					ID:      resp.ToolCalls[i].ID,
					Name:    res.name,
					Output:  res.output,
					IsErr:   res.status == "error",
					Images:  res.images,
					SpoolID: ensureSpooled(res.output),
				}
			}
			// Tool results are appended before any exit check: an assistant
			// tool-call turn without its matching results is an invalid prefix
			// for the next request (and would poison the carried history).
			resultsMsg := provider.Message{Role: provider.RoleUser, ToolResults: toolResults}
			if loopAct == loopNudge {
				resultsMsg.Content = loopNudgeMessage
				if cfg.ToolCallback != nil {
					cfg.ToolCallback(ToolTrace{AgentID: runtime.traceID(), Name: "comment", Status: "success", Output: "repetition detected — nudged the agent to change approach"})
				}
			}
			convMsgs = append(convMsgs, resultsMsg)
			if runtime.shouldStop() {
				return finish(runtime.stopMessage(), false, "")
			}
			if runtime.goalIsComplete() {
				summary := runtime.goalSummary
				if summary == "" {
					summary = strings.TrimSpace(main)
				}
				return finish(summary, true, summary)
			}
			continue
		}

		// Final answer: model returned text with no tool calls.
		if next, ok := runtime.nextRequiredRead(); ok {
			emitNarration(cfg, main)
			convMsgs = append(convMsgs, provider.Message{Role: provider.RoleAssistant, Content: main})
			convMsgs = append(convMsgs, provider.Message{Role: provider.RoleUser, Content: fmt.Sprintf("system: you must read %q with file-read before giving your final answer.", next)})
			continue
		}
		return finish(strings.TrimSpace(main), false, "")
	}
}

// emitNarration surfaces mid-run assistant prose as a persistent comment. The
// live stream draft is discarded when the next step resets it, so without this
// any text the model writes alongside (or instead of) tool calls would flash
// on screen and then vanish.
func emitNarration(cfg toolLoopConfig, text string) {
	if cfg.ToolCallback == nil {
		return
	}
	text = stripLeakedToolCalls(text)
	if text == "" {
		return
	}
	id := cfg.InstanceID
	if id == "" {
		id = cfg.AgentID
	}
	cfg.ToolCallback(ToolTrace{AgentID: id, Name: "comment", Status: "success", Args: fmt.Sprintf(`{"message":%q}`, text), Output: text})
}

// compactConv summarizes the older portion of convMsgs into a single
// synthetic message when the estimated request size approaches the context
// window (or unconditionally when force is set — used to recover from an
// over-budget context instead of failing the run). The cut/summarize core
// lives in compactpkg.CompactHistory (shared with the ACP bridge's
// between-turn compaction); this wrapper supplies the runtime's summarizer
// routing.
func (r *toolRuntime) compactConv(ctx context.Context, system string, msgs []provider.Message, window int, force bool) ([]provider.Message, bool, error) {
	if r.providerMgr == nil || r.providerName == nil || r.modelName == nil {
		return msgs, false, fmt.Errorf("compaction: provider not configured")
	}
	// Compaction is an internal utility call: route it to the designated
	// small/cheap model when configured and fall back silently through the
	// chain on availability failures — the main conversation model (and its
	// prompt cache) is unaffected either way.
	send := func(ctx context.Context, req provider.Request) (provider.Response, error) {
		primary := r.internalModelRef
		if primary.IsZero() || !r.providerMgr.HasModel(primary.Provider, primary.Model) {
			primary = provider.ModelRef{Provider: r.providerName(), Model: r.modelName()}
		}
		chain := r.fallbackChain
		if r.fallbackMode == config.FallbackOff {
			chain = nil
		}
		return provider.SendWithFallback(ctx,
			func(ctx context.Context, ref provider.ModelRef, req provider.Request) (provider.Response, error) {
				return r.providerMgr.Send(ctx, ref.Provider, ref.Model, req)
			},
			primary, chain, req, nil)
	}
	out, did, err := compactpkg.CompactHistoryWithPolicy(ctx, send, system, msgs, window, force, r.compactCfg, r.compactFailures)
	// Consecutive-failure bookkeeping: a failing summarizer pauses the auto
	// trigger after MaxFailures (see compact.Evaluate); any success resets it.
	if err != nil {
		r.compactFailures++
	} else if did {
		r.compactFailures = 0
	}
	return out, did, err
}

// formatTokens renders a token count compactly for transcript notices
// ("42k" above a thousand, exact below).
func formatTokens(n int) string {
	if n >= 1000 {
		return fmt.Sprintf("%dk", n/1000)
	}
	return fmt.Sprintf("%d", n)
}

// parallelExec fires one goroutine per call and collects results in original order.
//
// It enforces two limits:
//   - r.maxToolCallsPerStep caps the total batch size; calls beyond the limit
//     get a synthetic error result so the LLM sees the deny in the next step
//     and adapts. This protects against the model emitting hundreds of tool
//     calls in a single response (which would otherwise spawn a goroutine per
//     call and balloon history/cost).
//   - agentBudget caps how many `agent` (sub-agent) calls execute in parallel
//     within a single batch.
func (r *toolRuntime) parallelExec(ctx context.Context, calls []toolCall, allowed map[string]struct{}, callback func(ToolTrace)) []parallelResult {
	results := make([]parallelResult, len(calls))
	agentBudget := r.maxParallelWorkers
	if r.delegationDepth > 0 {
		agentBudget = r.maxParallelMicroagnt
	}
	toolCap := r.maxToolCallsPerStep
	agentCalls := 0
	var wg sync.WaitGroup
	for i, call := range calls {
		if toolCap > 0 && i >= toolCap {
			results[i] = parallelResult{
				agentID: r.traceID(),
				name:    call.Tool,
				args:    singleLine(string(call.Args)),
				output:  fmt.Sprintf("error: too many tool calls in one step (limit %d, batch %d); this call was skipped — emit a smaller batch", toolCap, len(calls)),
				status:  "error",
			}
			continue
		}
		if call.Tool == "agent" {
			agentCalls++
			if agentCalls > agentBudget {
				results[i] = parallelResult{
					agentID: r.traceID(),
					name:    call.Tool,
					args:    singleLine(string(call.Args)),
					output:  fmt.Sprintf("error: delegation limit reached (max %d in parallel)", agentBudget),
					status:  "error",
				}
				continue
			}
		}
		wg.Add(1)
		go func(idx int, c toolCall) {
			defer wg.Done()
			callArgs := singleLine(string(c.Args))
			if callback != nil && isMajorOperationTool(c.Tool) {
				msg := fmt.Sprintf("Starting %s (%s).", c.Tool, summarizeLoopToolArgs(c.Tool, callArgs))
				callback(ToolTrace{AgentID: r.traceID(), Name: "comment", Status: "success", Args: fmt.Sprintf(`{"message":%q}`, msg), Output: msg})
			}
			if callback != nil {
				callback(ToolTrace{AgentID: r.traceID(), Name: c.Tool, Args: callArgs, Status: "running"})
			}
			cctx, sink := withImageSink(ctx)
			output, err := r.executeWithTimeout(cctx, c, allowed)
			status := "success"
			if err != nil {
				status = "error"
				output = "error: " + err.Error()
			}
			results[idx] = parallelResult{
				agentID: r.traceID(),
				name:    c.Tool,
				args:    callArgs,
				output:  output,
				status:  status,
				images:  sink.list(),
			}
			if callback != nil {
				callback(ToolTrace{AgentID: r.traceID(), Name: c.Tool, Status: status, Args: callArgs, Output: truncate(output, 600), Images: sink.list()})
				if isMajorOperationTool(c.Tool) {
					msg := fmt.Sprintf("Completed %s.", c.Tool)
					if err != nil {
						msg = fmt.Sprintf("Failed %s: %s", c.Tool, truncate(err.Error(), 180))
					}
					callback(ToolTrace{AgentID: r.traceID(), Name: "comment", Status: "success", Args: fmt.Sprintf(`{"message":%q}`, msg), Output: msg})
				}
			}
		}(i, call)
	}
	wg.Wait()
	return results
}

// historyLimit returns the character cap for a tool's output in model history,
// applying any manifest override before falling back to the package default.
func (r *toolRuntime) historyLimit(toolName string) int {
	if r.manifest != nil {
		lim := r.manifest.Runtime.Limits
		switch toolName {
		case "file-read":
			if lim.FileReadChars > 0 {
				return lim.FileReadChars
			}
		case "repo-search", "grep", "glob", "ls":
			if lim.SearchChars > 0 {
				return lim.SearchChars
			}
		default:
			if lim.ToolOutputChars > 0 {
				return lim.ToolOutputChars
			}
		}
	}
	return toolOutputHistoryLimit(toolName)
}

func (r *toolRuntime) executeWithTimeout(ctx context.Context, call toolCall, allowed map[string]struct{}) (string, error) {
	timeoutSec := 45
	if spec, ok := r.toolPolicies[call.Tool]; ok && spec.TimeoutSec > 0 {
		timeoutSec = spec.TimeoutSec
	}
	if call.Tool == "ultra" {
		// A swarm runs many full sub-agent turns; the per-tool default (and any
		// manifest value tuned for single tools) would kill it mid-flight.
		timeoutSec = 7200
	}
	if r.goalMode {
		switch call.Tool {
		case "shell-exec", "bash", "bash-output":
			if r.shellTimeoutSec > 0 {
				timeoutSec = r.shellTimeoutSec
			} else if timeoutSec < 600 {
				timeoutSec = 600
			}
		}
	}
	tctx, cancel := context.WithTimeout(ctx, time.Duration(timeoutSec)*time.Second)
	defer cancel()
	out, err := r.execute(tctx, call, allowed)
	_ = r.runPostToolHooks(tctx, call.Tool, call.Args, out)
	return out, err
}

func (r *toolRuntime) execute(ctx context.Context, call toolCall, allowed map[string]struct{}) (string, error) {
	if _, ok := allowed[call.Tool]; !ok {
		return "", fmt.Errorf("tool %q not allowed", call.Tool)
	}
	if spec, ok := r.toolPolicies[call.Tool]; ok {
		if evaluatePermissionRule("tool", spec.ID, r.runtimeRules, r.agentRules, spec.PermissionRules) == config.RuleDeny {
			return "", fmt.Errorf("tool %q denied by policy", call.Tool)
		}
		for _, fam := range toolPermissionFamilies(spec) {
			if evaluatePermissionRule(fam, spec.ID, r.runtimeRules, r.agentRules, spec.PermissionRules) == config.RuleDeny {
				return "", fmt.Errorf("tool %q denied by policy for permission %q", call.Tool, fam)
			}
		}
	}
	updatedArgs, denyReason, err := r.runPreToolHooks(ctx, call.Tool, call.Args)
	if err != nil {
		return "", err
	}
	if denyReason != "" {
		return "", fmt.Errorf("tool %q blocked by hook: %s", call.Tool, denyReason)
	}
	if len(updatedArgs) > 0 {
		call.Args = updatedArgs
	}
	if call.Tool != "file-read" && call.Tool != "glob" && call.Tool != "grep" {
		if next, ok := r.nextRequiredRead(); ok {
			return "", fmt.Errorf("must read %q with file-read first", next)
		}
	}
	if r.checkpoint != nil && isMutatingTool(call.Tool) {
		r.checkpoint(call.Tool)
	}
	switch call.Tool {
	case "repo-search":
		var args struct {
			Query string `json:"query"`
		}
		if err := decodeJSONStrict(call.Args, &args); err != nil {
			return "", fmt.Errorf("repo-search args: %w", err)
		}
		out, err := r.searcher.Search(ctx, r.cwd, strings.TrimSpace(args.Query))
		if err != nil {
			return "", err
		}
		r.markReadFromSearch(out)
		return r.spoolResult("repo-search", out), nil
	case "file-read":
		var args struct {
			Path      string `json:"path"`
			StartLine int    `json:"start_line"`
			EndLine   int    `json:"end_line"`
		}
		if err := decodeJSONStrict(call.Args, &args); err != nil {
			return "", fmt.Errorf("file-read args: %w", err)
		}
		abs, rel, err := r.resolvePath(args.Path)
		if err != nil {
			return "", err
		}
		data, err := os.ReadFile(abs)
		if err != nil {
			return "", err
		}
		r.mu.Lock()
		r.readSet[rel] = struct{}{}
		delete(r.requiredReads, rel)
		r.mu.Unlock()
		content := string(data)
		if args.StartLine > 0 {
			// Bounded reads are already scoped by the model; plain truncation
			// keeps the response aligned with the requested line window.
			content = sliceLines(content, args.StartLine, args.EndLine)
			return truncate(content, r.historyLimit("file-read")), nil
		}
		return r.spoolResult("file-read", content), nil
	case "file-write":
		var args struct {
			Path    string `json:"path"`
			Content string `json:"content"`
			Append  bool   `json:"append"`
		}
		if err := decodeJSONStrict(call.Args, &args); err != nil {
			return "", fmt.Errorf("file-write args: %w", err)
		}
		abs, rel, err := r.resolvePath(args.Path)
		if err != nil {
			return "", err
		}
		if strings.TrimSpace(args.Path) == "" {
			return "", fmt.Errorf("file-write path is required")
		}
		_, statErr := os.Stat(abs)
		exists := statErr == nil
		oldContent := ""
		if exists {
			r.mu.Lock()
			_, alreadyRead := r.readSet[rel]
			r.mu.Unlock()
			if !alreadyRead {
				return "", fmt.Errorf("refusing write: read %q first", rel)
			}
			if raw, err := os.ReadFile(abs); err == nil {
				oldContent = string(raw)
			}
		}
		newContent := args.Content
		if args.Append {
			newContent = oldContent + args.Content
		}
		if err := r.authorizeWriteAccess(ctx, "file-write", rel, diff.Unified(rel, oldContent, newContent)); err != nil {
			return "", err
		}
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			return "", err
		}
		if args.Append {
			f, err := os.OpenFile(abs, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
			if err != nil {
				return "", err
			}
			defer f.Close()
			if _, err := f.WriteString(args.Content); err != nil {
				return "", err
			}
		} else {
			if err := os.WriteFile(abs, []byte(args.Content), 0o644); err != nil {
				return "", err
			}
		}
		r.mu.Lock()
		r.readSet[rel] = struct{}{}
		r.mu.Unlock()
		r.invalidateSymbolIndex(rel)
		if exists {
			return r.withLSPDiagnostics(ctx, abs, fmt.Sprintf("updated %s", rel)), nil
		}
		return r.withLSPDiagnostics(ctx, abs, fmt.Sprintf("created %s", rel)), nil
	case "shell-exec":
		return r.runShellTool(ctx, call.Tool, call.Args, "shell-exec")
	case "glob":
		var args struct {
			Pattern string `json:"pattern"`
			Path    string `json:"path"` // optional subdirectory
		}
		if err := decodeJSONStrict(call.Args, &args); err != nil {
			return "", fmt.Errorf("glob args: %w", err)
		}
		return r.runGlob(args.Pattern, args.Path)
	case "grep":
		var gargs grepArgs
		if err := decodeJSONStrict(call.Args, &gargs); err != nil {
			return "", fmt.Errorf("grep args: %w", err)
		}
		out, err := r.runGrep(ctx, gargs)
		if err != nil {
			return "", err
		}
		if gargs.OutputMode == "" || gargs.OutputMode == "content" {
			return r.spoolResult("grep", out), nil
		}
		return out, nil
	case "ls":
		var args struct {
			Path string `json:"path"`
		}
		if err := decodeJSONStrict(call.Args, &args); err != nil {
			return "", fmt.Errorf("ls args: %w", err)
		}
		dir := "."
		if args.Path != "" {
			abs, _, err := r.resolvePath(args.Path)
			if err != nil {
				return "", fmt.Errorf("ls: %w", err)
			}
			dir = abs
		} else {
			dir = r.cwd
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			return "", fmt.Errorf("ls: %w", err)
		}
		var lines []string
		for _, e := range entries {
			if e.IsDir() {
				lines = append(lines, e.Name()+"/")
			} else {
				lines = append(lines, e.Name())
			}
		}
		return strings.Join(lines, "\n"), nil
	case "web-fetch":
		return r.runWebFetch(ctx, call.Args)
	case "download":
		return r.runDownload(ctx, call.Args)
	case "web-search":
		return r.runWebSearch(ctx, call.Args)
	case "grok-image":
		return r.runGrokImage(ctx, call.Args)
	case "grok-video":
		return r.runGrokVideo(ctx, call.Args)
	case "view-image":
		return r.runViewImage(ctx, call.Args)
	case "ask-user":
		return r.runAskUser(ctx, call.Args)
	case "enter-plan-mode":
		return r.runPlanModeToggle(call.Args, true)
	case "exit-plan-mode":
		return r.runPlanModeToggle(call.Args, false)
	case "task-create":
		return r.runTaskCreate(call.Args)
	case "task-get":
		return r.runTaskGet(call.Args)
	case "task-update":
		return r.runTaskUpdate(call.Args)
	case "task-list":
		return r.runTaskList(call.Args)
	case "task-delete":
		return r.runTaskDelete(call.Args)
	case "task-stop":
		return r.runTaskStop(call.Args)
	case "goal-complete":
		return r.runGoalComplete(call.Args)
	case "tool-search":
		return r.runToolSearch(allowed, call.Args)
	case "skill-read", "activate-skill", "skill-activate":
		return r.runSkillRead(call.Args)
	case "skill-list":
		return r.runSkillList(call.Args)
	case "config":
		return r.runConfigTool(call.Args)
	case "diagnostics":
		return r.runLSPDiagnostics(ctx, call.Args)
	case "references":
		return r.runLSPReferences(ctx, call.Args)
	case "hover":
		return r.runLSPHover(ctx, call.Args)
	case "rename-symbol":
		return r.runLSPRename(ctx, call.Args)
	case "lsp-restart":
		return r.runLSPRestart(call.Args)
	case "mcp-list-resources":
		return r.runMCPListResources(ctx, call.Args)
	case "mcp-read-resource":
		return r.runMCPReadResource(ctx, call.Args)
	case "mcp-auth":
		return r.runMCPAuth(ctx, call.Args)
	case "save-memory":
		return r.runSaveMemory(call.Args)
	case "todo-write":
		var args struct {
			Todos []any `json:"todos"`
		}
		if err := decodeJSONStrict(call.Args, &args); err != nil {
			return "", fmt.Errorf("todo-write args: %w", err)
		}
		if strings.TrimSpace(r.sessionDir) == "" {
			return "", fmt.Errorf("todo-write requires an active session")
		}
		out := make([]session.Todo, 0, len(args.Todos))
		now := time.Now()
		for i, item := range args.Todos {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			id, _ := m["id"].(string)
			if strings.TrimSpace(id) == "" {
				id = fmt.Sprintf("todo-%d", i+1)
			}
			content, _ := m["content"].(string)
			status, _ := m["status"].(string)
			if status == "" {
				status = "pending"
			}
			owner, _ := m["owner"].(string)
			source, _ := m["source"].(string)
			priority, _ := m["priority"].(string)
			var deps []string
			if rawDeps, ok := m["dependencies"].([]any); ok {
				for _, d := range rawDeps {
					if s, ok := d.(string); ok && strings.TrimSpace(s) != "" {
						deps = append(deps, strings.TrimSpace(s))
					}
				}
			}
			out = append(out, session.Todo{
				ID:           id,
				Content:      content,
				Status:       status,
				Owner:        owner,
				Source:       source,
				Priority:     priority,
				Dependencies: deps,
				UpdatedAt:    now,
			})
		}
		// Route through the session store so the write is atomic and holds the
		// same lock as the task tools; direct file writes here raced with them.
		sid := filepath.Base(r.sessionDir)
		if err := session.SaveTodos(filepath.Dir(filepath.Dir(r.sessionDir)), sid, out); err != nil {
			return "", fmt.Errorf("todo-write: %w", err)
		}
		return fmt.Sprintf("wrote %d todos", len(out)), nil
	case "file-edit":
		return r.runFileEdit(ctx, call.Args)
	case "multi-edit":
		return r.runMultiEdit(ctx, call.Args)
	case "enter-worktree":
		return r.runEnterWorktree(ctx, call.Args)
	case "exit-worktree":
		return r.runExitWorktree(ctx, call.Args)
	case "send-message":
		return r.runSendMessage(call.Args)
	case "bash", "bash-output":
		// Models frequently treat bash-output as the polling tool for background
		// jobs (job_id + offset) rather than as a bash alias; honor that reading
		// whenever a job_id is supplied so both conventions work.
		var probe struct {
			JobID string `json:"job_id"`
		}
		if json.Unmarshal(call.Args, &probe) == nil && strings.TrimSpace(probe.JobID) != "" {
			return r.runJobOutput(call.Args)
		}
		return r.runShellTool(ctx, call.Tool, call.Args, "bash")
	case "job-output":
		return r.runJobOutput(call.Args)
	case "tool-output":
		return r.runToolOutput(call.Args)
	case "job-kill":
		return r.runJobKill(call.Args)
	case "pty-start":
		return r.runPtyStart(ctx, call.Tool, call.Args)
	case "pty-write":
		return r.runPtyWrite(call.Args)
	case "pty-kill":
		return r.runPtyKill(call.Args)
	case "comment":
		var args struct {
			Message string `json:"message"`
		}
		if err := decodeJSONStrict(call.Args, &args); err != nil {
			return "", fmt.Errorf("comment args: %w", err)
		}
		return args.Message, nil
	case "agent":
		var args struct {
			Agent          string `json:"agent"`
			Target         string `json:"target"`
			ID             string `json:"id"`
			Task           string `json:"task"`
			Constraints    string `json:"constraints"`
			ExpectedOutput string `json:"expected_output"`
			ParentAgentID  string `json:"parent_agent_id"`
		}
		if err := decodeJSONStrict(call.Args, &args); err != nil {
			return "", fmt.Errorf("agent args: %w", err)
		}
		target := strings.TrimSpace(args.Agent)
		if target == "" {
			target = strings.TrimSpace(args.Target)
		}
		if target == "" {
			target = strings.TrimSpace(args.ID)
		}
		if target == "" || strings.TrimSpace(args.Task) == "" {
			return "", fmt.Errorf("agent: target and task required")
		}
		if r.delegationDepth >= r.maxDelegationDepth {
			return "", fmt.Errorf("agent: delegation depth exceeded")
		}
		if r.manifest == nil || r.providerMgr == nil {
			return "", fmt.Errorf("agent: sub-agent execution not configured")
		}
		// Find agent spec
		var spec *config.AgentSpec
		for _, a := range r.manifest.Agents {
			if a.ID == target {
				s := a
				spec = &s
				break
			}
		}
		if spec == nil {
			return "", fmt.Errorf("agent: unknown agent %q", target)
		}
		if !spec.Enabled {
			return "", fmt.Errorf("agent: target %q is disabled", target)
		}
		if strings.TrimSpace(r.agentID) != "" {
			if caller, ok := r.manifest.AgentByID(r.agentID); ok {
				allowedHandoff := slices.Contains(caller.Handoffs, target)
				if !allowedHandoff {
					return "", fmt.Errorf("agent: %q cannot delegate to %q (allowed handoffs: %s)", r.agentID, target, strings.Join(caller.Handoffs, ", "))
				}
				if !isDelegationRoleAllowed(caller.Role, spec.Role) {
					return "", fmt.Errorf("agent: role %q cannot delegate to role %q", caller.Role, spec.Role)
				}
				if spec.Mode == "orchestrator" {
					return "", fmt.Errorf("agent: delegation target %q must be worker/subagent role, got orchestrator mode", target)
				}
			}
		}
		// Create and run sub-agent
		subTask := strings.TrimSpace(args.Task)
		if strings.TrimSpace(args.Constraints) != "" {
			subTask += "\n\nConstraints:\n" + strings.TrimSpace(args.Constraints)
		}
		if strings.TrimSpace(args.ExpectedOutput) != "" {
			subTask += "\n\nExpected output:\n" + strings.TrimSpace(args.ExpectedOutput)
		}
		parentID := strings.TrimSpace(args.ParentAgentID)
		if parentID == "" {
			parentID = r.agentID
		}
		subSpec := *spec
		// The parent's effective permission cascades to sub-agents so that a
		// user-level setting (e.g. yolo) is honoured across the entire tree.
		if p := r.perm(); p != "" {
			subSpec.Permission = p
		}
		subAgent := LLMAgent{
			Spec:            subSpec,
			PermissionFn:    r.permissionFn,
			ProviderManager: r.providerMgr,
			ProviderName:    r.providerName,
			ModelName:       r.modelName,
			CWD:             r.cwd,
			MaxTokens:       r.maxTokens,
			Thinking:        r.thinkingLevel,
			ToolCallback:    r.toolCallback,
			Checkpoint:      r.checkpoint,
			ShellApproval:   r.shellApproval,
			AskUser:         r.askUser,
			Manifest:        r.manifest,
			SandboxState:    r.sandboxState,
			SessionDir:      r.sessionDir,
			DelegationDepth: r.delegationDepth + 1,
			ParentAgentID:   parentID,
		}
		result, err := subAgent.Run(ctx, subTask)
		if err != nil {
			return "", fmt.Errorf("agent %s: %w", target, err)
		}
		return marshalSubagentResult(target, result), nil
	case "ultra":
		return r.runUltra(ctx, call.Args)
	default:
		return "", fmt.Errorf("unsupported tool %q", call.Tool)
	}
}

// isMutatingTool reports whether a tool can modify the working tree and thus
// warrants a pre-execution checkpoint. Shell tools are always treated as
// mutating: classifying arbitrary commands reliably is not possible, and a
// spurious checkpoint is cheap while a missed one is unrecoverable.
func isMutatingTool(tool string) bool {
	switch tool {
	case "file-write", "file-edit", "multi-edit", "rename-symbol", "shell-exec", "bash", "pty-start", "pty-write":
		return true
	}
	return false
}

// skipDirs are directories to skip when walking the workspace.
var skipDirs = map[string]bool{
	".git":         true,
	".spettro":     true,
	"vendor":       true,
	"node_modules": true,
	"dist":         true,
	"build":        true,
}

// runGlob implements the glob tool using filepath.WalkDir with ** support.
func (r *toolRuntime) runGlob(pattern, subPath string) (string, error) {
	if strings.TrimSpace(pattern) == "" {
		return "", fmt.Errorf("glob: pattern is required")
	}
	root := r.cwd
	if strings.TrimSpace(subPath) != "" {
		abs, _, err := r.resolvePath(subPath)
		if err != nil {
			return "", fmt.Errorf("glob path: %w", err)
		}
		root = abs
	}

	var matches []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // skip inaccessible entries
		}
		if d.IsDir() {
			if skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		rel, relErr := filepath.Rel(r.cwd, path)
		if relErr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if matchGlobPattern(pattern, rel) {
			matches = append(matches, rel)
		}
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("glob walk: %w", err)
	}
	sort.Strings(matches)
	if len(matches) == 0 {
		return fmt.Sprintf("no files match %q", pattern), nil
	}
	return fmt.Sprintf("%d files:\n%s", len(matches), strings.Join(matches, "\n")), nil
}

// matchGlobPattern matches a slash-separated path against a glob pattern with ** support.
func matchGlobPattern(pattern, rel string) bool {
	patParts := strings.Split(pattern, "/")
	pathParts := strings.Split(rel, "/")
	return globMatch(patParts, pathParts)
}

func globMatch(patParts, pathParts []string) bool {
	if len(patParts) == 0 && len(pathParts) == 0 {
		return true
	}
	if len(patParts) == 0 {
		return false
	}
	if patParts[0] == "**" {
		// ** can match zero or more path components
		// Try matching rest of pattern against every suffix of path
		restPat := patParts[1:]
		// Zero-component match: skip ** entirely
		if globMatch(restPat, pathParts) {
			return true
		}
		// One or more components match
		for i := 1; i <= len(pathParts); i++ {
			if globMatch(restPat, pathParts[i:]) {
				return true
			}
		}
		return false
	}
	if len(pathParts) == 0 {
		return false
	}
	matched, err := filepath.Match(patParts[0], pathParts[0])
	if err != nil || !matched {
		return false
	}
	return globMatch(patParts[1:], pathParts[1:])
}

// typeExtensions maps type names to file extensions.
func typeExtensions(t string) []string {
	switch strings.ToLower(t) {
	case "go":
		return []string{".go"}
	case "ts":
		return []string{".ts", ".tsx"}
	case "js":
		return []string{".js", ".jsx", ".mjs"}
	case "py":
		return []string{".py"}
	case "rs":
		return []string{".rs"}
	case "md":
		return []string{".md"}
	case "toml":
		return []string{".toml"}
	case "json":
		return []string{".json"}
	case "yaml", "yml":
		return []string{".yaml", ".yml"}
	case "sh":
		return []string{".sh", ".bash"}
	default:
		return nil
	}
}

type grepArgs struct {
	Pattern         string `json:"pattern"`
	Glob            string `json:"glob"`
	Type            string `json:"type"`
	CaseInsensitive bool   `json:"case_insensitive"`
	Context         int    `json:"context"`
	OutputMode      string `json:"output_mode"`
	MaxResults      int    `json:"max_results"`
}

// runGrep implements the grep tool.
func (r *toolRuntime) runGrep(_ context.Context, args grepArgs) (string, error) {
	if strings.TrimSpace(args.Pattern) == "" {
		return "", fmt.Errorf("grep: pattern is required")
	}
	regexPattern := args.Pattern
	if args.CaseInsensitive {
		regexPattern = "(?i)" + regexPattern
	}
	re, err := regexp.Compile(regexPattern)
	if err != nil {
		return "", fmt.Errorf("grep: invalid pattern: %w", err)
	}
	if args.MaxResults <= 0 {
		args.MaxResults = 200
	}
	outputMode := args.OutputMode
	if outputMode == "" {
		outputMode = "content"
	}

	exts := typeExtensions(args.Type)

	type fileResult struct {
		path   string
		count  int
		blocks []string // for content mode
	}

	var results []fileResult
	totalMatches := 0
	truncated := false

	walkErr := filepath.WalkDir(r.cwd, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if truncated {
			return nil
		}

		// Filter by type
		if len(exts) > 0 {
			ext := strings.ToLower(filepath.Ext(d.Name()))
			found := slices.Contains(exts, ext)
			if !found {
				return nil
			}
		}
		// Filter by glob
		if args.Glob != "" {
			matched, mErr := filepath.Match(args.Glob, d.Name())
			if mErr != nil || !matched {
				return nil
			}
		}

		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil
		}
		rel, relErr := filepath.Rel(r.cwd, path)
		if relErr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)

		lines := strings.Split(string(data), "\n")
		matchLines := make([]int, 0)
		for i, line := range lines {
			if re.MatchString(line) {
				matchLines = append(matchLines, i)
			}
		}
		if len(matchLines) == 0 {
			return nil
		}

		// Mark as read from search
		r.mu.Lock()
		r.readSet[rel] = struct{}{}
		r.mu.Unlock()

		fr := fileResult{path: rel, count: len(matchLines)}

		if outputMode == "content" {
			// Build context blocks
			included := make([]bool, len(lines))
			for _, mi := range matchLines {
				start := max(mi-args.Context, 0)
				end := mi + args.Context
				if end >= len(lines) {
					end = len(lines) - 1
				}
				for j := start; j <= end; j++ {
					included[j] = true
				}
			}

			var blockBuf bytes.Buffer
			prevIncluded := false
			for i, line := range lines {
				if included[i] {
					if !prevIncluded && blockBuf.Len() > 0 {
						blockBuf.WriteString("--\n")
					}
					fmt.Fprintf(&blockBuf, "%s:%d: %s\n", rel, i+1, line)
					prevIncluded = true
				} else {
					prevIncluded = false
				}
			}
			fr.blocks = []string{blockBuf.String()}
		}

		results = append(results, fr)
		totalMatches += len(matchLines)
		if totalMatches >= args.MaxResults {
			truncated = true
		}
		return nil
	})
	if walkErr != nil {
		return "", fmt.Errorf("grep walk: %w", walkErr)
	}

	if len(results) == 0 {
		return fmt.Sprintf("no matches for %q", args.Pattern), nil
	}

	var sb strings.Builder
	switch outputMode {
	case "files_with_matches":
		for _, fr := range results {
			sb.WriteString(fr.path)
			sb.WriteString("\n")
		}
		header := fmt.Sprintf("%d matches in %d files:\n", totalMatches, len(results))
		out := header + sb.String()
		if truncated {
			out += fmt.Sprintf("(truncated at %d matches)\n", args.MaxResults)
		}
		return strings.TrimRight(out, "\n"), nil
	case "count":
		for _, fr := range results {
			fmt.Fprintf(&sb, "%s: %d\n", fr.path, fr.count)
		}
		header := fmt.Sprintf("%d matches in %d files:\n", totalMatches, len(results))
		out := header + sb.String()
		if truncated {
			out += fmt.Sprintf("(truncated at %d matches)\n", args.MaxResults)
		}
		return strings.TrimRight(out, "\n"), nil
	default: // "content"
		for _, fr := range results {
			for _, block := range fr.blocks {
				sb.WriteString(block)
			}
		}
		header := fmt.Sprintf("%d matches in %d files:\n", totalMatches, len(results))
		out := header + sb.String()
		if truncated {
			out += fmt.Sprintf("(truncated at %d matches)\n", args.MaxResults)
		}
		return strings.TrimRight(out, "\n"), nil
	}
}

func (r *toolRuntime) nextRequiredRead() (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.requiredReads) == 0 {
		return "", false
	}
	keys := make([]string, 0, len(r.requiredReads))
	for k := range r.requiredReads {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys[0], true
}

func (r *toolRuntime) resolvePath(p string) (abs, rel string, err error) {
	p = strings.TrimSpace(p)
	if p == "" {
		return "", "", fmt.Errorf("path is required")
	}
	if filepath.IsAbs(p) {
		abs = filepath.Clean(p)
	} else {
		abs = filepath.Clean(filepath.Join(r.cwd, p))
	}
	rel, err = filepath.Rel(r.cwd, abs)
	if err != nil {
		return "", "", err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", "", fmt.Errorf("path outside workspace is not allowed")
	}
	// Under an active sandbox, also reject paths whose *real* target escapes the
	// workspace through a symlink. Without this, an agent could `ln -s` a secret
	// (e.g. ~/.ssh/id_rsa) into the workspace and read it via the in-process
	// file tools, which run in the parent (reads open) and would otherwise
	// follow the link past the shell-layer read confinement.
	if r.sandboxPolicy().FSEnforced() && realPathEscapes(r.cwd, abs) {
		return "", "", fmt.Errorf("path outside workspace is not allowed")
	}
	rel = filepath.ToSlash(rel)
	return abs, rel, nil
}

// realPathEscapes reports whether abs — after resolving symlinks on its
// longest existing prefix — points outside dir. The target itself need not
// exist (file-write creates new files), so only the existing ancestry is
// resolved and the missing tail is re-appended.
func realPathEscapes(dir, abs string) bool {
	realDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		realDir = filepath.Clean(dir)
	}
	real, rem := abs, ""
	for {
		if resolved, rerr := filepath.EvalSymlinks(real); rerr == nil {
			real = resolved
			break
		}
		parent := filepath.Dir(real)
		if parent == real {
			real = filepath.Clean(abs)
			break
		}
		rem = filepath.Join(filepath.Base(real), rem)
		real = parent
	}
	full := filepath.Clean(filepath.Join(real, rem))
	rel, err := filepath.Rel(realDir, full)
	if err != nil {
		return true
	}
	return rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// searchLineNumberRE matches ":<digits>" segments in repo-search output, used
// by markReadFromSearch to detect ripgrep-style "path:lineno:..." rows.
var searchLineNumberRE = regexp.MustCompile(`^\d+$`)

// invalidateSymbolIndex drops rel from the repo symbol index after one of the
// agent's own write tools touched it, so the next repo-search re-parses it
// even if the filesystem mtime didn't visibly change.
func (r *toolRuntime) invalidateSymbolIndex(rel string) {
	if r.searcher.Index != nil {
		r.searcher.Index.Invalidate(rel)
	}
}

func (r *toolRuntime) markReadFromSearch(out string) {
	lines := strings.SplitSeq(out, "\n")
	for line := range lines {
		parts := strings.SplitN(line, ":", 3)
		if len(parts) < 2 {
			continue
		}
		if !searchLineNumberRE.MatchString(parts[1]) {
			continue
		}
		r.mu.Lock()
		r.readSet[strings.TrimSpace(parts[0])] = struct{}{}
		r.mu.Unlock()
	}
}
