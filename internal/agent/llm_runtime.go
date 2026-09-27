package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	"spettro/internal/hooks"
	"spettro/internal/provider"
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
	// Change is the proposed change itself, the file's whole text before and
	// after, when the approval is for one file (file-write, file-edit); nil
	// otherwise. Hosts that render real diffs (ACP editors) use it instead of
	// Diff. Its texts are dropped for very large files (see FileChange).
	Change *FileChange
	// AgentID is the agent asking, under the same name its ToolTraces carry
	// (the per-instance name such as "code#3" for swarm members, else the
	// agent's ID). Sub-agents run in parallel with the main agent and with
	// each other, so a host that shows the request on a tool call card uses
	// it to pick a card of the asking agent. Set by toolRuntime.askApproval.
	AgentID string
	// CWD is the asking agent's working directory: a worktree for an
	// isolated sub-agent, else the session's directory. Relative paths in
	// that agent's tool arguments are relative to it. Set by
	// toolRuntime.askApproval.
	CWD string
}

type ShellApprovalCallback func(context.Context, ShellApprovalRequest) (ShellApprovalDecision, error)

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
		AllowedTools:    []string{"file-read", "file-write", "bash", "job-output", "job-kill", "tool-output", "glob", "grep", "lsp", "rename-symbol"},
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
	// WorkflowPreapproved marks the turn as having the user's standing consent
	// to start a workflow without confirming first (they typed the keyword).
	WorkflowPreapproved bool
	// GoalMode enables generous tool timeouts and (step 03) goal-complete
	// signaling. Non-goal runs behave exactly as before.
	GoalMode        bool
	ContextWindow   int // model context window in tokens; drives in-loop compaction. 0 → default
	ShellTimeoutSec int // goal-mode per shell/bash timeout; 0 → default
	// MaxSteps caps the number of successful LLM steps in this run; 0 means
	// unlimited (the default for normal runs). Goal-mode hosts set it so each
	// iteration is bounded and control returns to the outer goal loop, which
	// owns progress detection, the stall guard, and the iteration cap. Without
	// the bound the goal preamble ("only goal-complete finishes the run") keeps
	// the inner loop alive forever and the outer loop never runs.
	MaxSteps int
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
	// MaxTokens is the per-request INPUT token budget (config token_budget):
	// a prompt estimated at or above it is force-compacted once, then the run
	// fails. 0 = unlimited. It is never sent as the output cap.
	MaxTokens int
	// MaxOutputTokens caps each reply (max_tokens on the wire); 0 = auto, the
	// provider manager's per-model default (see provider.Manager.MaxOutputTokens).
	MaxOutputTokens int
	// parentSnapshot and parentCWD are a parent run's snapshot and
	// directory, for a sub-agent's system prompt (see sessionContextFor).
	parentSnapshot string
	parentCWD      string
	Thinking       provider.ThinkingLevel // forwarded to provider.Request.Thinking
	RequiredReads  []string
	Images         []string        // attached to this turn's user message (re-sent every step)
	ToolCallback   func(ToolTrace) // optional: called with status="running" before and final status after each tool
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
	// Checkpoint, when set, is invoked synchronously right before the first
	// file-modifying tool call of each step (file-write, file-edit, a shell
	// command not provably read-only; see checkpoint_policy.go), so the host
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
	// it before its next step. Top-level runs get the host's queue; a delegated
	// sub-agent gets one that only ever carries its time-limit wrap-up notice.
	Steering *SteeringQueue

	// toolSurfaceNote is the system prompt's note on the tools held but not
	// advertised up front (see toolSurfacePrompt); set by runToolLoop.
	toolSurfaceNote string

	// skillLoadTool is the tool the system prompt's skill list tells the
	// model to call (see toolRuntime.skillLoadTool); "" leaves the list out.
	// Set by runToolLoop.
	skillLoadTool string
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
	// CalledAs is the retired name the model used when canonicalToolCall
	// rewrote the call to its canonical tool; hooks match it as well.
	CalledAs string `json:"-"`
}

type toolRuntime struct {
	cwd     string
	mu      sync.Mutex
	shellMu sync.Mutex
	// worktreeMu serializes every git operation that touches the shared
	// repository — creating a sub-agent worktree, and merging one back. Two of
	// those running at once contend on the repo's index and ref locks, and git
	// reports the loser as a plain failure rather than as a conflict, so the
	// work looks merged when it is not.
	worktreeMu    sync.Mutex
	readSet       map[string]struct{}
	requiredReads map[string]struct{}
	// fileStamps and fileLocks back the stale-read guard and per-file write
	// serialization (file_stamps.go); readStamps holds the content hash of
	// each path's last file-read. All are created lazily.
	fileStamps    map[string][32]byte
	readStamps    map[string][32]byte
	stampsChanged map[string]struct{} // stamp keys changed since takeStampDelta
	fileLocks     map[string]*sync.Mutex
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
	manifest     *config.AgentManifest
	providerMgr  *provider.Manager
	providerName func() string
	modelName    func() string
	maxTokens    int
	// sessionCtx is this run's environment/instructions snapshot, handed
	// to sub-agents working in the same directory.
	sessionCtx string
	// maxOutputTokens is the user's output cap (config max_output_tokens),
	// handed on to sub-agents; 0 = the provider manager's default.
	maxOutputTokens int
	// thinkingLevel is the level sub-agents start from: the configured one,
	// then whatever level the model last accepted (see subAgentThinking).
	thinkingLevel provider.ThinkingLevel
	toolCallback  func(ToolTrace)
	checkpoint    func(tool string)
	sessionDir    string
	agentID       string
	instanceID    string
	parentID      string

	// stepCheckpointed records that the current step already snapshotted the
	// working tree (checkpoint_policy.go); parallelExec clears it per step.
	stepCheckpointMu sync.Mutex
	stepCheckpointed bool

	delegationDepth      int
	maxParallelWorkers   int
	maxParallelMicroagnt int
	maxDelegationDepth   int
	maxToolCallsPerStep  int
	hooksConfig          hooks.EffectiveConfig
	stopRequested        bool
	stopReason           string
	skillsCatalog        skills.Catalog
	skillTool            string // the tool the model loads skills with (see skillLoadTool); "" when none
	goalMode             bool
	// workflowPreapproved skips the workflow tool's confirmation prompt: the
	// user already said yes by writing the keyword.
	workflowPreapproved bool
	shellTimeoutSec     int
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

	// lspWarm makes file-read start the file's language server in the
	// background: set when the agent has a tool that uses one.
	lspWarm bool

	// surface is the set of tool schemas the run advertises: the core tools
	// plus the deferred ones activated so far (tool_surface.go). nil means
	// no deferral (runtimes built outside runToolLoop).
	surface *toolSurface

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
	answer, err := AskSingleQuestion(ctx, r.askUser, AskUserRequest{
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

// ErrContentFiltered is the error a run fails with when the provider's
// content filter stopped the model's reply before it said anything. Hosts
// test for it with errors.Is to report a refusal rather than a failure (ACP
// maps it to the "refusal" stop reason).
var ErrContentFiltered = errors.New("the provider's content filter stopped the response")

// stepCapMessage closes an iteration that hit cfg.MaxSteps. It is returned as
// the run's content (and recorded as the final assistant turn), so the goal
// host's transcript shows why the iteration ended before the next one starts.
const stepCapMessage = "(iteration paused: step limit reached — the goal loop will review progress and continue in the next iteration)"

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
		maxOutputTokens: cfg.MaxOutputTokens,
		thinkingLevel:   cfg.Thinking,
		toolCallback:    cfg.ToolCallback,
		checkpoint:      cfg.Checkpoint,
		sessionDir:      cfg.SessionDir,
		agentID:         cfg.AgentID,
		instanceID:      cfg.InstanceID,
		parentID:        cfg.ParentAgentID,
		delegationDepth: cfg.DelegationDepth,
		skillsCatalog:   cfg.SkillsCatalog,
		compactCfg:      cfg.Compact,
		lspWarm:         usesLanguageServer(allowed),
	}
	var loopPolicy config.LoopDetectionPolicy
	if cfg.Manifest != nil {
		loopPolicy = cfg.Manifest.Runtime.LoopDetection
	}
	runtime.loopDetect = newLoopDetector(loopPolicy)
	// The stale-read guard spans the conversation: earlier runs' stamps ride
	// on the carried tool-results messages.
	runtime.restoreStamps(cfg.Messages)
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
	runtime.workflowPreapproved = cfg.WorkflowPreapproved
	runtime.shellTimeoutSec = cfg.ShellTimeoutSec
	// Project state (.spettro/) comes from the main checkout when this run
	// is a sub-agent inside an agent worktree; see projectStateDir.
	allowedShell, err := loadAllowedCommandSet(projectStateDir(cfg.CWD))
	if err != nil {
		return toolLoopResult{}, err
	}
	runtime.allowedShell = allowedShell
	hooksCfg, err := hooks.LoadEffective(projectStateDir(cfg.CWD))
	if err != nil {
		return toolLoopResult{}, err
	}
	runtime.hooksConfig = hooksCfg
	if err := runtime.runSessionStartHooks(ctx); err != nil {
		return toolLoopResult{}, err
	}
	// Required reads are keyed by workspace-relative path, the key
	// runFileRead clears. The prompt lists the same normalized paths.
	cfg.RequiredReads = runtime.readableRequiredReads(cfg.RequiredReads)
	for _, p := range cfg.RequiredReads {
		runtime.requiredReads[p] = struct{}{}
	}
	var traces []ToolTrace

	// Native tool calling is always used: tool schemas ride on the API request
	// for every model. Models whose catalog entry claims no tool support still
	// get the schemas — local OpenAI-compatible servers accept them, and the
	// old TOOL_CALL text-protocol fallback caused tool-capable local models to
	// emit unparsed TOOL_CALL strings instead of real tool calls.
	//
	// The list is the tool surface: the core tools, plus each deferred tool
	// once tool-search (or a call by name) activated it — in this turn or an
	// earlier one of the carried conversation. It changes only on an
	// activation, so the cached prompt prefix survives every other step.
	runtime.skillTool = runtime.skillLoadTool(allowed)
	cfg.skillLoadTool = runtime.skillTool
	runtime.surface = runtime.buildToolSurface(cfg.AllowedTools, cfg.SystemPrompt)
	runtime.restoreActivations(cfg.Messages)
	cfg.toolSurfaceNote = toolSurfacePrompt(runtime.surface.deferredNames(), len(runtime.surface.droppedNames()) > 0)
	nativeToolSpecs := runtime.surface.specs()

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
	// complete and reusable as the next turn's prefix. When the loop already
	// stored that turn itself (with its reasoning, or as continued pieces) it
	// sets answerRecorded first.
	answerRecorded := false
	finish := func(content string, goalDone bool, goalSummary string) (toolLoopResult, error) {
		if strings.TrimSpace(content) != "" && !answerRecorded {
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
	// steps or the provider-side prompt cache misses on every call. The
	// session snapshot in it travels with the conversation (on its first
	// message) so later turns rebuild the exact same prompt.
	sessionCtx := sessionContextFor(cfg)
	system := buildSystemStringWith(cfg, sessionCtx)
	runtime.sessionCtx = sessionCtx
	if cfg.DelegationDepth == 0 && sessionCtx != "" && len(convMsgs) > 0 && carriedSessionContext(convMsgs) == "" {
		convMsgs[0].SessionContext = sessionCtx
	}

	// Resilience state. Provider failures are classified
	// (provider.ClassifyRetry): transient ones (rate limit, overload, 5xx,
	// network, stalled stream) are retried with exponential backoff that
	// honors Retry-After; deterministic ones (auth, bad request) fail fast; a
	// context overflow forces one compaction and a resend. An over-budget
	// context also gets one forced compaction attempt, so a single bad step
	// (huge tool output, provider hiccup) doesn't kill the whole run.
	retryPolicy := provider.DefaultRetryPolicy
	sendFailures := 0
	budgetCompacted := false
	overflowCompacted := false
	// overflowResent is set once an overflow was retried with a newly learned
	// window and no compaction (see the overflow branch below).
	overflowResent := false
	// learnedWindow is the context window a provider stated in an overflow
	// error. It overrides a missing (URL/local endpoints) or larger window.
	learnedWindow := 0
	// emptyReplies counts consecutive replies with neither text nor tool
	// calls; each one appends a nudge, which a run that gives up drops from
	// the history again (see dropEmptyNudges).
	emptyReplies := 0
	// truncatedText collects the pieces of a text answer that hit the output
	// limit and was continued; they are joined into the final answer.
	var truncatedText []string
	// toolCallsThisTurn counts the tool calls this run has executed. The
	// announce-only nudge applies only while it is zero: once the model has
	// done real work, a short closing line is a legitimate final answer.
	toolCallsThisTurn := 0
	// announceNudged and droppedCallNudged record that this turn already
	// spent its one announce-only or dropped-tool-call nudge (see
	// llm_runtime_nudge.go); a second such reply is handled as before.
	announceNudged := false
	droppedCallNudged := false
	// thinking starts at the configured level and follows the level the
	// manager actually succeeded with, so a level the model rejected is not
	// re-sent (and re-rejected) on every later step.
	thinking := cfg.Thinking
	// measure is the calibrated prompt size of a would-be request: history +
	// system + tool schemas, scaled by what the provider reported for the
	// previous step (see usageCalibration).
	var calibration usageCalibration
	measure := func(system string, msgs []provider.Message) int {
		return calibration.apply(provider.EstimateRequestTokens(provider.Request{System: system, Messages: msgs, Tools: nativeToolSpecs}))
	}
	contextWindow := func() int {
		w := cfg.ContextWindow
		if w <= 0 {
			// Hosts that don't wire the window (sub-agents, URL endpoints):
			// ask the catalog / local probe.
			m := runtime.effectiveModel()
			w = cfg.ProviderManager.ModelContext(m.Provider, m.Model)
		}
		if learnedWindow > 0 && (w <= 0 || learnedWindow < w) {
			w = learnedWindow
		}
		return w
	}
	notify := func(msg string) {
		if cfg.ToolCallback != nil {
			cfg.ToolCallback(ToolTrace{AgentID: runtime.traceID(), Name: "comment", Status: "success", Output: msg})
		}
	}
	// steps counts successful LLM calls; when cfg.MaxSteps is set (goal-mode
	// iterations) the loop yields back to the host once the cap is reached.
	steps := 0

	for {
		nativeToolSpecs = runtime.surface.specs()
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
		beforeTokens := measure(system, convMsgs)
		if compacted, did, err := runtime.compactConv(ctx, system, convMsgs, contextWindow(), false, measure); err != nil {
			notify(fmt.Sprintf("auto-compaction failed (%s) — continuing; will retry at the next threshold crossing", truncate(err.Error(), 200)))
		} else {
			convMsgs = compacted
			if did {
				notify(fmt.Sprintf("compacted %s → %s tokens to stay within the context window", formatTokens(beforeTokens), formatTokens(measure(system, convMsgs))))
			}
		}
		// Input budget (config token_budget): the whole prompt — tool results
		// and tool schemas included — must stay under it.
		if err := budget.CheckTokens(cfg.MaxTokens, measure(system, convMsgs)); err != nil {
			// Over budget (e.g. an oversized tool result blew up the history):
			// force-compact once instead of failing the run. Only if forced
			// compaction doesn't help either does the run error out.
			if !budgetCompacted {
				budgetCompacted = true
				if compacted, did, cerr := runtime.compactConv(ctx, system, convMsgs, contextWindow(), true, measure); cerr == nil && did {
					convMsgs = compacted
					notify("context exceeded the token budget — force-compacted history and continuing")
					continue
				}
			}
			return fail(err)
		}
		budgetCompacted = false
		req := provider.Request{
			System:        system,
			Messages:      convMsgs,
			MaxTokens:     cfg.MaxOutputTokens,
			Thinking:      thinking,
			ContextWindow: contextWindow(),
		}
		if len(nativeToolSpecs) > 0 {
			req.Tools = nativeToolSpecs
		}
		if cfg.ToolCallback != nil {
			req.OnRateLimit = func(d time.Duration) {
				notify(fmt.Sprintf("rate limited, waiting %ds before retrying...", int(d.Round(time.Second).Seconds())))
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
		sentEstimate := provider.EstimateRequestTokens(req)
		resp, err := cfg.ProviderManager.Send(ctx, model.Provider, model.Model, req)
		if demux != nil {
			demux.flush()
		}
		if err != nil {
			// User cancellation is not retryable — surface it immediately.
			if ctx.Err() != nil {
				return fail(fmt.Errorf("agent call failed: %w", err))
			}
			class := provider.ClassifyRetry(err)
			// Context overflow: resending the same prompt is pointless. Learn
			// the real window when the provider states it, force-compact once
			// and resend; fail clearly if compaction cannot shrink it.
			if class == provider.RetryContextOverflow {
				if n := provider.ContextLimitFromError(err); n > 0 && (learnedWindow == 0 || n < learnedWindow) {
					learnedWindow = n
				}
				if !overflowCompacted {
					overflowCompacted = true
					if compacted, did, cerr := runtime.compactConv(ctx, system, convMsgs, contextWindow(), true, measure); cerr == nil && did {
						convMsgs = compacted
						notify("the request exceeded the model's context window — force-compacted history and retrying")
						continue
					}
				}
				// Nothing to compact (a short history), but the error named a
				// smaller window than the request was sized for: resend once,
				// so the output cap is fitted to the real window. Often the
				// prompt fit and only max_tokens overflowed.
				if w := contextWindow(); w > 0 && w != req.ContextWindow && !overflowResent {
					overflowResent = true
					notify(fmt.Sprintf("the request exceeded the model's context window (%s tokens) — retrying with the output cap fitted to it", formatTokens(w)))
					continue
				}
				return fail(fmt.Errorf("agent call failed: the conversation exceeds the model's context window and compaction could not shrink it enough: %w", err))
			}
			// Transient (rate limit / overload / 5xx / network / stalled
			// stream) or unclassified: bounded exponential backoff with
			// jitter, honoring the provider's Retry-After. Deterministic
			// failures (auth, invalid request) are never resent.
			sendFailures++
			if delay, ok := retryPolicy.NextDelay(err, sendFailures); ok {
				notify(fmt.Sprintf("provider call failed (%s) — retrying in %s (attempt %d)...", truncate(err.Error(), 180), delay.Round(100*time.Millisecond), sendFailures+1))
				select {
				case <-time.After(delay):
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
				sendFailures = 0
				notify(fmt.Sprintf("model %s unavailable — switched to fallback %s for the rest of this run", model, next))
				continue
			}
			return fail(fmt.Errorf("agent call failed: %w", err))
		}
		sendFailures = 0
		overflowCompacted = false
		overflowResent = false
		thinking = resp.Thinking
		runtime.setSubAgentThinking(thinking)
		calibration.observe(resp.Usage.TotalInput(), sentEstimate)
		steps++
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

		logReply(runtime.traceID(), steps, resp)

		content := strings.TrimSpace(resp.Content)
		main, _ := stripThinkTags(content)
		main = strings.TrimSpace(main)
		// stepCapReached: the host's per-run step budget (goal-mode
		// iterations) is spent, so no nudge may ask for another request.
		stepCapReached := cfg.MaxSteps > 0 && steps >= cfg.MaxSteps

		// The provider says the reply stopped for tool calls, yet none
		// arrived: the call was lost on the way. Ask once for it again
		// instead of ending the turn on whatever text came with it. Not
		// during a continuation of a truncated answer, which has its own
		// bounded flow.
		if droppedToolCall(resp) && !droppedCallNudged && len(nativeToolSpecs) > 0 && len(truncatedText) == 0 && !stepCapReached {
			droppedCallNudged = true
			logDroppedToolCall(runtime.traceID(), steps)
			if main != "" {
				emitNarration(cfg, main)
				convMsgs = append(convMsgs, provider.Message{Role: provider.RoleAssistant, Content: main, Reasoning: resp.Reasoning})
			}
			convMsgs = append(convMsgs, provider.Message{Role: provider.RoleUser, Content: droppedToolCallNudge})
			notify("the model stopped for a tool call that never arrived — asking it to send the call again")
			continue
		}

		if main == "" && len(resp.ToolCalls) == 0 {
			if resp.FinishReason == provider.FinishContentFilter {
				return fail(fmt.Errorf("agent call failed: %w", ErrContentFiltered))
			}
			if len(truncatedText) > 0 {
				// A continuation came back empty: the answer is complete. Drop
				// the trailing continue request so the carried history ends on
				// the answer.
				if last := len(convMsgs) - 1; last >= 0 && convMsgs[last].Role == provider.RoleUser && convMsgs[last].Content == continueTruncatedNudge {
					convMsgs = convMsgs[:last]
				}
				answerRecorded = true
				return finish(strings.TrimSpace(strings.Join(truncatedText, "")), false, "")
			}
			if cfg.MaxSteps > 0 && steps >= cfg.MaxSteps {
				return finish("", false, "")
			}
			// Never resend the identical request: nudge the model, and give
			// up with a clear error once the empty streak persists.
			emptyReplies++
			if emptyReplies >= maxEmptyReplies {
				convMsgs = dropEmptyNudges(convMsgs, emptyReplies-1)
				return fail(fmt.Errorf("agent call failed: the model returned %d empty responses in a row", emptyReplies))
			}
			nudge := emptyReplyNudge
			if resp.Truncated() {
				nudge = emptyTruncatedNudge
			}
			convMsgs = append(convMsgs, provider.Message{Role: provider.RoleUser, Content: nudge})
			notify(fmt.Sprintf("the model returned an empty response — nudging it to continue (%d/%d)", emptyReplies, maxEmptyReplies))
			continue
		}
		emptyReplies = 0

		// Native tool-calling path: model returned structured tool calls.
		if len(resp.ToolCalls) > 0 {
			truncatedText = nil
			toolCallsThisTurn += len(resp.ToolCalls)
			emitNarration(cfg, main)
			internalCalls := runtime.loopCalls(resp.ToolCalls)
			results := runtime.execToolCalls(ctx, resp.ToolCalls, allowed, cfg.ToolCallback)
			// A deferred tool the model called by name is advertised from
			// the next step on, so its next call has the schema.
			runtime.noteCalls(toolCallNames(resp.ToolCalls))
			runtime.recordActivations(convMsgs)
			// Loop check after execution: the signature includes each result,
			// so re-running a command whose output changes (edit → test) is
			// progress; only the same call with the same result repeats. On
			// abort the results are still recorded below, keeping the
			// history a valid prefix.
			loopAct := runtime.loopDetect.observe(internalCalls, results, main)
			convMsgs = append(convMsgs, provider.Message{
				Role:      provider.RoleAssistant,
				Content:   main,
				ToolCalls: resp.ToolCalls,
				Reasoning: resp.Reasoning,
			})
			toolResults := make([]provider.ToolResult, len(results))
			for i, res := range results {
				traces = append(traces, ToolTrace{AgentID: res.agentID, Name: res.name, Status: res.status, Args: res.args, Output: truncate(res.output, 600), Images: res.images})
				toolResults[i] = provider.ToolResult{
					ID:      resp.ToolCalls[i].ID,
					Name:    resp.ToolCalls[i].Name,
					Output:  res.output,
					IsErr:   res.status == "error",
					Images:  res.images,
					SpoolID: ensureSpooled(res.output),
				}
			}
			// Tool results are appended before any exit check: an assistant
			// tool-call turn without its matching results is an invalid prefix
			// for the next request (and would poison the carried history).
			resultsMsg := provider.Message{Role: provider.RoleUser, ToolResults: toolResults, FileStamps: runtime.takeStampDelta()}
			if loopAct == loopNudge {
				resultsMsg.Content = loopNudgeMessage
				notify("repetition detected — nudged the agent to change approach")
			}
			convMsgs = append(convMsgs, resultsMsg)
			if loopAct == loopAbort {
				return finish(loopStopMessage, false, "")
			}
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
			// Per-run step cap: yield the iteration back to the host. The tool
			// results for this step are already in convMsgs, so the returned
			// conversation is a valid prefix for the next iteration to extend.
			if cfg.MaxSteps > 0 && steps >= cfg.MaxSteps {
				return finish(stepCapMessage, false, "")
			}
			continue
		}

		// Text cut at the output token limit: a half answer is not a final
		// answer. Record the piece and ask the model to continue (bounded).
		if resp.Truncated() && len(truncatedText) < maxContinuations && !(cfg.MaxSteps > 0 && steps >= cfg.MaxSteps) {
			piece, _ := stripThinkTags(resp.Content)
			if len(truncatedText) == 0 {
				piece = strings.TrimLeft(piece, " \t\r\n")
			}
			truncatedText = append(truncatedText, piece)
			convMsgs = append(convMsgs,
				provider.Message{Role: provider.RoleAssistant, Content: main, Reasoning: resp.Reasoning},
				provider.Message{Role: provider.RoleUser, Content: continueTruncatedNudge})
			notify(fmt.Sprintf("the response hit the output token limit — asking the model to continue (%d/%d)", len(truncatedText), maxContinuations))
			continue
		}

		// Final answer: model returned text with no tool calls.
		if next, ok := runtime.nextRequiredRead(); ok {
			emitNarration(cfg, main)
			convMsgs = append(convMsgs, provider.Message{Role: provider.RoleAssistant, Content: main, Reasoning: resp.Reasoning})
			convMsgs = append(convMsgs, provider.Message{Role: provider.RoleUser, Content: fmt.Sprintf("system: you must read %q with file-read before giving your final answer.", next)})
			continue
		}
		// An announcement with nothing done yet ("I'll start by exploring
		// the repository...") is not a final answer: nudge once to go on.
		if !announceNudged && toolCallsThisTurn == 0 && len(nativeToolSpecs) > 0 && len(truncatedText) == 0 && !stepCapReached && looksLikeAnnouncement(main) {
			announceNudged = true
			emitNarration(cfg, main)
			convMsgs = append(convMsgs,
				provider.Message{Role: provider.RoleAssistant, Content: main, Reasoning: resp.Reasoning},
				provider.Message{Role: provider.RoleUser, Content: announceOnlyNudge})
			notify("the model announced work without calling a tool — nudging it to continue")
			continue
		}
		// Store the answer with its reasoning (finish only records plain
		// text), then return it — joined with any earlier continued pieces.
		convMsgs = append(convMsgs, provider.Message{Role: provider.RoleAssistant, Content: main, Reasoning: resp.Reasoning})
		answerRecorded = true
		if len(truncatedText) > 0 {
			piece, _ := stripThinkTags(resp.Content)
			return finish(strings.TrimSpace(strings.Join(append(truncatedText, piece), "")), false, "")
		}
		return finish(main, false, "")
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
	cfg.ToolCallback(ToolTrace{AgentID: id, Name: "comment", Status: "success", Args: fmt.Sprintf(`{"message":%q}`, text), Output: text, Narration: true})
}

// dropEmptyNudges removes the last n empty-reply nudges from msgs, matched
// by content rather than position: compaction may have rewritten the history
// since the streak began (shifting or summarizing the nudges away), and
// steering turns the user sent between nudges must survive. Empty replies
// are never recorded, so the streak's nudges all follow the last assistant
// message; earlier ones (from a streak the model recovered from) are kept.
// The result never aliases msgs.
func dropEmptyNudges(msgs []provider.Message, n int) []provider.Message {
	drop := make(map[int]bool, n)
	for i := len(msgs) - 1; i >= 0 && len(drop) < n; i-- {
		m := msgs[i]
		if m.Role == provider.RoleAssistant {
			break
		}
		if m.Role == provider.RoleUser && len(m.ToolResults) == 0 && (m.Content == emptyReplyNudge || m.Content == emptyTruncatedNudge) {
			drop[i] = true
		}
	}
	out := make([]provider.Message, 0, len(msgs)-len(drop))
	for i, m := range msgs {
		if !drop[i] {
			out = append(out, m)
		}
	}
	return out
}

// compactConv compacts convMsgs when the measured request size approaches
// the context window (or unconditionally when force is set — used to recover
// from an over-budget or overflowing context instead of failing the run):
// old tool outputs are pruned first, and only if that is not enough are the
// older turns summarized (see compactpkg.Compact, shared with the ACP bridge
// and the TUI's /compact). measure sizes the would-be request (nil → plain
// history estimate); this wrapper supplies the runtime's summarizer routing.
func (r *toolRuntime) compactConv(ctx context.Context, system string, msgs []provider.Message, window int, force bool, measure compactpkg.MeasureFunc) ([]provider.Message, bool, error) {
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
	// A forced pass here is recovery from an over-budget or overflowing
	// context: when the summarizer fails, a summary extracted from the
	// transcript beats failing the run.
	res, err := compactpkg.Compact(ctx, send, msgs, compactpkg.Params{
		System:             system,
		Window:             window,
		Force:              force,
		Policy:             r.compactCfg,
		Failures:           r.compactFailures,
		Measure:            measure,
		ExtractiveFallback: force,
	})
	out, did := res.Messages, res.Compacted()
	if err != nil {
		out, did = msgs, false
	}
	if did && len(out) > 0 {
		// The summarized messages carried stamp deltas; the summary carries
		// all of them instead, so a later turn still restores every stamp.
		out[0].FileStamps = r.stampSnapshot()
	}
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

// concurrentTools may run at the same time as each other within one step:
// tools that only read (files, the index, the web, job/spool output) plus
// `agent`, whose sub-agent spawns are Spettro's parallelism feature and were
// always fanned out together (see agentBudget in parallelExec). Everything
// else — file writes and edits, shell and pty commands, worktree and swarm
// tools, and any tool not listed here (MCP included) — runs alone, in the
// model's order. The lsp tool's lookups are concurrent too, but not its
// restart (see concurrentCall).
var concurrentTools = map[string]bool{
	"file-read":          true,
	"grep":               true,
	"glob":               true,
	"web-fetch":          true,
	"web-search":         true,
	"view-image":         true,
	"skill":              true,
	"tool-search":        true,
	"job-output":         true,
	"tool-output":        true,
	"mcp-list-resources": true,
	"mcp-read-resource":  true,
	"comment":            true,
	"agent":              true,
}

// planToolBatches splits a step's calls (by index) into the batches parallelExec
// runs one after another: each maximal run of consecutive concurrent tools is
// one batch whose calls run together, and every other call is a batch of its
// own. A mutating call therefore always sees the effects of every call the
// model placed before it, and never races one placed after it.
func planToolBatches(calls []toolCall, indices []int) [][]int {
	var batches [][]int
	var group []int
	for _, idx := range indices {
		if concurrentCall(calls[idx]) {
			group = append(group, idx)
			continue
		}
		if len(group) > 0 {
			batches = append(batches, group)
			group = nil
		}
		batches = append(batches, []int{idx})
	}
	if len(group) > 0 {
		batches = append(batches, group)
	}
	return batches
}

// concurrentCall reports whether a call, as the built-in that carries it out
// sees it (toolRuntime.builtinCall), may run together with its neighbours
// (see concurrentTools). An lsp restart stops the servers the lookups next
// to it would be talking to, so it runs alone, as lsp-restart always did.
func concurrentCall(call toolCall) bool {
	if call.Tool == "lsp" {
		return lspCallOp(call.Args) != "restart"
	}
	return concurrentTools[call.Tool]
}

// planBatches is planToolBatches over how each call is carried out
// (toolRuntime.builtinCall), not what it is named: an unfolded retired
// built-in batches as the built-in whose code it runs, and a call no built-in
// carries out (a tool of the operator's own, or arguments that do not
// convert) is a batch of its own.
func (r *toolRuntime) planBatches(calls []toolCall, indices []int) [][]int {
	carried := make([]toolCall, len(calls))
	for _, idx := range indices {
		if run, err := r.builtinCall(calls[idx]); err == nil {
			carried[idx] = run
		}
	}
	return planToolBatches(carried, indices)
}

// parallelExec executes one step's tool calls and returns their results in
// call order. Consecutive read-only calls (and sub-agent spawns) run
// concurrently; mutating calls run serially in the order the model emitted
// them — see planToolBatches.
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
	r.resetStepCheckpoint()
	agentBudget := r.maxParallelWorkers
	if r.delegationDepth > 0 {
		agentBudget = r.maxParallelMicroagnt
	}
	toolCap := r.maxToolCallsPerStep
	agentCalls := 0
	runnable := make([]int, 0, len(calls))
	// Retired tool names become their canonical tool before anything else
	// looks at the call, so the allow-list, policies, hooks, batching, traces
	// and hosts only ever see canonical names.
	calls = slices.Clone(calls)
	for i, call := range calls {
		canon, err := r.canonicalCall(call)
		if err != nil {
			results[i] = parallelResult{
				agentID: r.traceID(),
				name:    call.Tool,
				args:    singleLine(string(call.Args)),
				output:  "error: " + err.Error(),
				status:  "error",
			}
			if callback != nil {
				callback(ToolTrace{AgentID: r.traceID(), Name: call.Tool, Status: "error", Args: results[i].args, Output: results[i].output})
			}
			calls[i] = toolCall{}
			continue
		}
		calls[i] = canon
	}
	for i, call := range calls {
		if call.Tool == "" {
			continue
		}
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
		runnable = append(runnable, i)
	}
	run := func(idx int, c toolCall) {
		callArgs := singleLine(string(c.Args))
		if err := ctx.Err(); err != nil {
			// The run was interrupted by an earlier call in this step; the
			// rest still need a result each, but must not start.
			results[idx] = parallelResult{
				agentID: r.traceID(),
				name:    c.Tool,
				args:    callArgs,
				output:  "error: not executed: " + err.Error(),
				status:  "error",
			}
			return
		}
		if callback != nil && isMajorOperationTool(c.Tool) {
			// The note is one line of the transcript: a heredoc's newlines,
			// or the "\n... (truncated)" truncate appends, would otherwise
			// render the start of the script as a paragraph of its own.
			args := strings.Join(strings.Fields(summarizeLoopToolArgs(c.Tool, callArgs)), " ")
			msg := fmt.Sprintf("Starting %s (%s).", c.Tool, args)
			callback(ToolTrace{AgentID: r.traceID(), Name: "comment", Status: "success", Args: fmt.Sprintf(`{"message":%q}`, msg), Output: msg})
		}
		if callback != nil {
			callback(ToolTrace{AgentID: r.traceID(), Name: c.Tool, Args: callArgs, Status: "running"})
		}
		cctx, sink := withImageSink(ctx)
		cctx, changes := withFileChangeSink(cctx)
		output, err := r.executeWithTimeout(cctx, c, allowed)
		status := "success"
		if err != nil {
			status = "error"
			output = toolErrorOutput(output, err)
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
			callback(ToolTrace{AgentID: r.traceID(), Name: c.Tool, Status: status, Args: callArgs, Output: truncate(output, 600), Images: sink.list(), FileChanges: changes.list()})
			if isMajorOperationTool(c.Tool) {
				msg := fmt.Sprintf("Completed %s.", c.Tool)
				if err != nil {
					msg = fmt.Sprintf("Failed %s: %s", c.Tool, truncate(err.Error(), 180))
				}
				callback(ToolTrace{AgentID: r.traceID(), Name: "comment", Status: "success", Args: fmt.Sprintf(`{"message":%q}`, msg), Output: msg})
			}
		}
	}
	for _, batch := range r.planBatches(calls, runnable) {
		if r.shouldStop() {
			// An earlier batch ended the turn (ask-user's reply-in-chat exit,
			// task-stop): later calls were planned on the assumption the turn
			// goes on, so they get a result each but must not start.
			for _, idx := range batch {
				results[idx] = parallelResult{
					agentID: r.traceID(),
					name:    calls[idx].Tool,
					args:    singleLine(string(calls[idx].Args)),
					output:  "error: not executed: the turn was ended by an earlier call in this step",
					status:  "error",
				}
			}
			continue
		}
		if len(batch) == 1 {
			run(batch[0], calls[batch[0]])
			continue
		}
		var wg sync.WaitGroup
		for _, idx := range batch {
			wg.Add(1)
			go func(idx int) {
				defer wg.Done()
				run(idx, calls[idx])
			}(idx)
		}
		wg.Wait()
	}
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
		case "grep", "glob":
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
	// The PostToolUse hooks in finishToolCall need the canonical call too.
	if canon, err := r.canonicalCall(call); err == nil {
		call = canon
	}
	ctx = withCalledAs(ctx, r.hookAlias(call))
	// How the call is bounded depends on the built-in that carries it out,
	// not on its name. A call no built-in carries out fails in execute
	// before doing anything, so the default deadline is all it needs.
	run, runErr := r.builtinCall(call)
	if runErr != nil {
		run = toolCall{}
	}
	if blocksOnUserInput(run.Tool) {
		// The tool is waiting on a person, who may take as long as they take.
		// A deadline here would cancel the question out from under them and
		// hand the model a timeout error as if nobody was there — the run must
		// block until the user actually answers (or declines). The manifest's
		// timeout_sec bounds tool execution, not human attention.
		out, err := r.execute(ctx, call, allowed)
		return r.finishToolCall(ctx, call, out, err), err
	}
	if run.Tool == "agent" {
		// The agent case bounds the sub-agent run itself (agentTimeout), so
		// that a sub-agent running out of time is reported with its partial
		// work instead of the whole call being cut off.
		out, err := r.execute(ctx, call, allowed)
		return r.finishToolCall(ctx, call, out, err), err
	}
	if r.isForegroundShellCall(run) {
		// runShellTool owns both deadlines of a foreground command: the
		// approval prompt gets the tool's default window, and the command's
		// own timeout (honouring a per-call timeout argument) starts only once
		// it is approved. An outer deadline here would start before approval,
		// so a slow approval — or a short per-call timeout — would eat into
		// the other. The command's wait is itself bounded (process-group kill
		// plus WaitDelay), and hooks carry their own timeouts.
		out, err := r.execute(ctx, call, allowed)
		return r.finishToolCall(ctx, call, out, err), err
	}
	timeout := time.Duration(r.defaultToolTimeoutSec(call.Tool)) * time.Second
	tctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	out, err := r.execute(tctx, call, allowed)
	return r.finishToolCall(tctx, call, out, err), err
}

// defaultToolTimeoutSec is a tool's execution limit in seconds when the call
// does not ask for its own: the manifest's timeout_sec, else 45s, with longer
// floors for swarms/workflows and for shell tools in goal mode. tool is the
// call's identity, whose manifest entry sets the limit; the floors follow the
// built-in that carries the call out (toolRuntime.builtinFor), so an
// unfolded shell-exec gets the shell's and a tool of the operator's own
// called bash does not.
func (r *toolRuntime) defaultToolTimeoutSec(tool string) int {
	timeoutSec := 45
	if spec, ok := r.toolPolicies[tool]; ok && spec.TimeoutSec > 0 {
		timeoutSec = spec.TimeoutSec
	}
	builtin := r.builtinFor(tool)
	if builtin == "ultra" || builtin == "workflow" {
		// A swarm — or a workflow script, which may run several rounds of them
		// — is many full sub-agent turns; the per-tool default (and any
		// manifest value tuned for single tools) would kill it mid-flight.
		timeoutSec = 7200
	}
	if r.goalMode {
		switch builtin {
		case "bash":
			if r.shellTimeoutSec > 0 {
				timeoutSec = r.shellTimeoutSec
			} else if timeoutSec < 600 {
				timeoutSec = 600
			}
		}
	}
	return timeoutSec
}

// blocksOnUserInput reports whether a tool's execution is a wait on the human,
// not work the agent is doing. Those tools run without a deadline; everything
// else is bounded by executeWithTimeout.
func blocksOnUserInput(tool string) bool {
	return tool == "ask-user"
}

func (r *toolRuntime) execute(ctx context.Context, call toolCall, allowed map[string]struct{}) (string, error) {
	// parallelExec already canonicalized the call; this covers direct callers,
	// so a retired name never reaches the dispatch below.
	call, err := r.canonicalCall(call)
	if err != nil {
		return "", err
	}
	ctx = withCalledAs(ctx, r.hookAlias(call))
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
		if err := r.lspOpDenied(call, spec); err != nil {
			return "", err
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
	// Everything above judged the call by its identity. From here on it is
	// carried out: call becomes the built-in's view of it (tool_names.go),
	// and id keeps the identity, for the per-tool policy a built-in looks up
	// while it runs (approvals, command and path rules) and for labels.
	id := call.Tool
	call, err = r.builtinCall(call)
	if err != nil {
		return "", err
	}
	if call.Tool != "file-read" && call.Tool != "glob" && call.Tool != "grep" {
		if next, ok := r.nextRequiredRead(); ok {
			return "", fmt.Errorf("must read %q with file-read first", next)
		}
	}
	if r.checkpoint != nil && needsCheckpoint(call) {
		r.checkpointStep(id)
	}
	switch call.Tool {
	case "file-read":
		return r.runFileRead(ctx, call.Args)
	case "file-write":
		defer r.lockFileForMutation(call.Args)()
		args, err := decodeFileWriteArgs(call.Args)
		if err != nil {
			return "", err
		}
		abs, rel, err := r.resolvePath(args.Path)
		if err != nil {
			return "", err
		}
		if strings.TrimSpace(args.Path) == "" {
			return "", fmt.Errorf("file-write path is required")
		}
		defer r.lockFile(abs)()
		_, statErr := os.Stat(abs)
		exists := statErr == nil
		var oldRaw []byte
		if exists {
			raw, err := os.ReadFile(abs)
			if err != nil {
				return "", err
			}
			oldRaw = raw
		}
		oldContent := string(oldRaw)
		// Overwriting needs a full read (a stamp); a grep hit
		// only showed the model a line or two of the file. Appending replaces
		// nothing, so it needs no read — and does not count as one.
		stampedBefore := exists && r.stampMatches(rel, oldRaw)
		if exists && !args.Append {
			if !r.hasFileStamp(rel) {
				r.mu.Lock()
				_, searched := r.readSet[rel]
				r.mu.Unlock()
				if searched {
					return "", fmt.Errorf("refusing write: file-read %q first (a search hit is not a full read)", rel)
				}
				return "", fmt.Errorf("refusing write: read %q first", rel)
			}
			if err := r.checkFileStamp("file-write", rel, oldRaw); err != nil {
				return "", err
			}
		}
		newContent := args.Content
		if args.Append {
			newContent = oldContent + args.Content
		}
		if err := r.authorizeFileChange(ctx, "file-write", abs, rel, oldContent, newContent, !exists); err != nil {
			return "", err
		}
		if err := r.recheckBeforeWrite("file-write", rel, abs, exists, oldRaw); err != nil {
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
		if !args.Append || !exists || stampedBefore {
			// An append to a file the agent had not seen in full leaves it
			// unstamped: the model still has not read what was there.
			r.recordFileStamp(rel, []byte(newContent))
		}
		r.invalidateSymbolIndex(rel)
		recordFileChange(ctx, abs, oldContent, newContent, !exists)
		if exists {
			return r.withLSPDiagnostics(ctx, abs, fmt.Sprintf("updated %s", rel)), nil
		}
		return r.withLSPDiagnostics(ctx, abs, fmt.Sprintf("created %s", rel)), nil
	case "glob":
		var args struct {
			Pattern string `json:"pattern"`
			Path    string `json:"path"` // optional subdirectory
		}
		if err := decodeJSONStrict(call.Args, &args); err != nil {
			return "", fmt.Errorf("glob args: %w", err)
		}
		if strings.TrimSpace(args.Pattern) == "" {
			return r.runListDir(args.Path)
		}
		out, err := r.runGlob(ctx, args.Pattern, args.Path)
		if err != nil {
			return "", err
		}
		return r.spoolResult("glob", out), nil
	case "grep":
		var gargs grepArgs
		if err := decodeJSONStrict(call.Args, &gargs); err != nil {
			return "", fmt.Errorf("grep args: %w", err)
		}
		if gargs.Symbol != nil {
			return r.runSymbolSearch(ctx, gargs)
		}
		out, err := r.runGrep(ctx, gargs)
		if err != nil {
			return "", err
		}
		return r.spoolResult("grep", out), nil
	case "web-fetch":
		return r.runWebFetch(ctx, call.Args)
	case "download":
		return r.runDownload(ctx, call.Args)
	case "web-search":
		return r.runWebSearch(ctx, call.Args)
	case "view-image":
		return r.runViewImage(ctx, call.Args)
	case "ask-user":
		return r.runAskUser(ctx, call.Args)
	case "enter-plan-mode":
		return r.runPlanModeToggle(call.Args, true)
	case "exit-plan-mode":
		return r.runPlanModeToggle(call.Args, false)
	case "task-stop":
		return r.runTaskStop(call.Args)
	case "goal-complete":
		return r.runGoalComplete(call.Args)
	case "tool-search":
		return r.runToolSearch(allowed, call.Args)
	case "skill":
		return r.runSkill(call.Args)
	case "config":
		return r.runConfigTool(call.Args)
	case "lsp":
		return r.runLSP(ctx, call.Args, call.CalledAs)
	case "rename-symbol":
		return r.runLSPRename(ctx, call.Args)
	case "mcp-list-resources":
		return r.runMCPListResources(ctx, call.Args)
	case "mcp-read-resource":
		return r.runMCPReadResource(ctx, call.Args)
	case "mcp-auth":
		return r.runMCPAuth(ctx, call.Args)
	case "save-memory":
		return r.runSaveMemory(call.Args)
	case "todo-write":
		return r.runTodoWrite(call.Args)
	case "file-edit":
		defer r.lockFileForMutation(call.Args)()
		return r.runFileEdit(ctx, id, call.Args)
	case "enter-worktree":
		return r.runEnterWorktree(ctx, call.Args)
	case "exit-worktree":
		return r.runExitWorktree(ctx, call.Args)
	case "send-message":
		return r.runSendMessage(call.Args)
	case "bash":
		// Models frequently treat bash (or its bash-output alias) as the polling
		// tool for background jobs (job_id + offset); honor that reading
		// whenever a job_id is supplied so both conventions work.
		var probe struct {
			JobID string `json:"job_id"`
		}
		if json.Unmarshal(call.Args, &probe) == nil && strings.TrimSpace(probe.JobID) != "" {
			return r.runJobOutput(call.Args)
		}
		return r.runShellTool(ctx, id, call.Args, "bash")
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
			Isolation      string `json:"isolation"`
		}
		if err := decodeJSONStrict(call.Args, &args); err != nil {
			return "", fmt.Errorf("agent args: %w", err)
		}
		isolation := strings.TrimSpace(args.Isolation)
		if isolation != "" && isolation != "worktree" {
			return "", fmt.Errorf("agent: unsupported isolation %q (only \"worktree\")", isolation)
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
		subCWD := r.cwd
		var workspace *agentWorkspace
		if isolation == "worktree" {
			ws, err := r.newSubagentWorkspace(ctx, target)
			if err != nil {
				return "", fmt.Errorf("agent: %w", err)
			}
			workspace = ws
			subCWD = ws.subCWD
		}
		subAgent := LLMAgent{
			Spec:            subSpec,
			PermissionFn:    r.permissionFn,
			ProviderManager: r.providerMgr,
			ProviderName:    r.providerName,
			ModelName:       r.modelName,
			CWD:             subCWD,
			MaxTokens:       r.maxTokens,
			MaxOutputTokens: r.maxOutputTokens,
			Thinking:        r.subAgentThinking(),
			Compact:         r.compactCfg,
			parentSnapshot:  r.sessionCtx,
			parentCWD:       r.cwd,
			ToolCallback:    r.toolCallback,
			Checkpoint:      r.subagentCheckpoint(subCWD),
			ShellApproval:   r.shellApproval,
			AskUser:         r.askUser,
			Manifest:        r.manifest,
			SandboxState:    r.sandboxState,
			SessionDir:      r.sessionDir,
			DelegationDepth: r.delegationDepth + 1,
			ParentAgentID:   parentID,
			// Carries only the wrap-up notice (subagent_timeout.go).
			Steering: NewSteeringQueue(),
		}
		// The sub-agent's deadline is set here rather than by
		// executeWithTimeout, so that when it passes the parent is still
		// running and can report the partial work.
		limit := r.agentTimeout(*spec)
		runCtx, cancelRun := context.WithTimeout(ctx, limit)
		stopWrapUp := scheduleWrapUp(subAgent.Steering, limit)
		result, err := subAgent.Run(runCtx, subTask)
		stopWrapUp()
		timedOut := err != nil && subagentTimedOut(ctx, runCtx)
		cancelRun()
		if err != nil {
			// The workspace outlives the (possibly cancelled) run context so
			// throwaway worktrees still get cleaned up.
			var kept *workspaceMerge
			var files []string
			if workspace != nil {
				files = workspace.changedFiles(context.WithoutCancel(ctx))
				kept = workspace.abandon(context.WithoutCancel(ctx))
			} else {
				files = modifiedFilesFromTraces(result.Tools)
			}
			if ctx.Err() != nil {
				// The parent itself was cancelled: nobody is waiting on a
				// report.
				if kept != nil {
					return "", fmt.Errorf("agent %s: %w (work preserved on branch %s at %s)", target, err, kept.Branch, kept.Path)
				}
				return "", fmt.Errorf("agent %s: %w", target, err)
			}
			status, reason := "failed", err.Error()
			if timedOut {
				status, reason = "timed_out", fmt.Sprintf("time limit of %s reached", limit)
			}
			return marshalSubagentPartial(target, status, reason, result, files, kept),
				&toolOutputError{msg: fmt.Sprintf("agent %s %s: %s", target, strings.ReplaceAll(status, "_", " "), reason)}
		}
		var merge *workspaceMerge
		if workspace != nil {
			// The merge writes into the main checkout, which the sub-agent's
			// own snapshots (taken in its worktree) never covered.
			r.checkpointStep("agent")
			m := workspace.finalize(context.WithoutCancel(ctx))
			merge = &m
		}
		return marshalSubagentResult(target, result, merge), nil
	case "ultra":
		return r.runUltra(ctx, call.Args)
	case "workflow":
		return r.runWorkflow(ctx, call.Args)
	default:
		return "", fmt.Errorf("unsupported tool %q", id)
	}
}

// isMutatingTool reports whether a tool can modify the working tree and thus
// warrants a pre-execution checkpoint. Shell tools count as mutating;
// needsCheckpoint (checkpoint_policy.go) exempts the narrow set of shell
// command lines that provably only read.
func isMutatingTool(tool string) bool {
	switch tool {
	case "file-write", "file-edit", "rename-symbol", "bash", "pty-start", "pty-write":
		return true
	}
	return false
}

// fileMutationLocks holds one mutex per file (keyed by resolved absolute path)
// serializing the in-process read-modify-write tools on it. It is process-wide
// rather than per runtime so sibling sub-agents editing the same checkout are
// covered too. parallelExec already runs mutating calls of one step serially;
// this is the backstop for everything that step ordering cannot see.
var fileMutationLocks sync.Map // string -> *sync.Mutex

// lockFileForMutation locks the file named by a tool call's path argument (or
// its file_path alias) and returns the unlock function. Arguments without a
// resolvable path lock nothing: the tool reports that error itself.
func (r *toolRuntime) lockFileForMutation(rawArgs []byte) (unlock func()) {
	var probe struct {
		Path     string `json:"path"`
		FilePath string `json:"file_path"`
	}
	_ = json.Unmarshal(rawArgs, &probe)
	p := firstNonEmpty(probe.Path, probe.FilePath)
	if p == "" {
		return func() {}
	}
	abs, _, err := r.resolvePath(p)
	if err != nil {
		return func() {}
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		abs = real
	}
	v, _ := fileMutationLocks.LoadOrStore(abs, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// readableRequiredReads turns the caller's required reads into the
// workspace-relative, slash-separated paths runFileRead records, dropping
// any the model could never satisfy: a path outside the workspace, or one
// that is not an existing regular file. Every non-read tool call is refused
// and every final answer is sent back while a required read is pending, so
// an unsatisfiable entry would otherwise loop the turn until its budget ran
// out. The TUI already sends relative, existing paths; ACP sends the
// absolute paths of the files the client attached.
func (r *toolRuntime) readableRequiredReads(paths []string) []string {
	out := make([]string, 0, len(paths))
	seen := map[string]bool{}
	for _, p := range paths {
		if strings.TrimSpace(p) == "" {
			continue
		}
		abs, rel, err := r.resolvePath(p)
		if err != nil {
			continue
		}
		if info, err := os.Stat(abs); err != nil || !info.Mode().IsRegular() {
			continue
		}
		if !seen[rel] {
			seen[rel] = true
			out = append(out, rel)
		}
	}
	return out
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

// searchLineNumberRE matches ":<digits>" segments in symbol-search output, used
// by markReadFromSearch to detect ripgrep-style "path:lineno:..." rows.
var searchLineNumberRE = regexp.MustCompile(`^\d+$`)

// invalidateSymbolIndex drops rel from the repo symbol index after one of the
// agent's own write tools touched it, so the next symbol search re-parses it
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
