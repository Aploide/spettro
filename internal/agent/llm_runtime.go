package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"spettro/internal/budget"
	"spettro/internal/config"
	"spettro/internal/hooks"
	"spettro/internal/provider"
	"spettro/internal/session"
	"spettro/internal/skills"
)

const (
	toolCallPrefix = "TOOL_CALL"
	finalPrefix    = "FINAL"

	// maxHistoryBytes caps the size of the rolling tool-loop history that
	// gets embedded into every prompt. Without this, a long run with many
	// tool calls (or a single misbehaving step that emits hundreds of
	// calls) would balloon the prompt indefinitely. The cap is generous —
	// the most recent ~32 KB of conversation are kept, older entries are
	// dropped FIFO with a "(history truncated)" marker.
	maxHistoryBytes = 32 * 1024
)

const codingSystemPromptFallback = `You are a coding agent that can use tools.
Implement the task using minimal safe edits and verify your changes.
Never include chain-of-thought or <think> blocks in output.`

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
	out, traces, tokens, contextTokens, err := runToolLoop(ctx, toolLoopConfig{
		SystemPrompt:    systemPrompt,
		UserTask:        plan,
		CWD:             c.CWD,
		MaxSteps:        24,
		RequireToolCall: true,
		AllowedTools:    []string{"repo-search", "file-read", "file-write", "shell-exec", "glob", "grep"},
		LogToolCalls:    true,
		ProviderManager: c.ProviderManager,
		ProviderName:    c.ProviderName,
		ModelName:       c.ModelName,
		MaxTokens:       c.MaxTokens,
		RequiredReads:   c.RequiredReads,
		ToolCallback:    c.ToolCallback,
		Permission:      level,
		ShellApproval:   c.ShellApproval,
		AskUser:         c.AskUser,
	})
	if err != nil {
		return RunResult{}, err
	}
	main, _ := stripThinkTags(out)
	return RunResult{
		Content:       strings.TrimSpace(main),
		Tools:         traces,
		TokensUsed:    tokens,
		ContextTokens: contextTokens,
	}, nil
}

type toolLoopConfig struct {
	SystemPrompt string
	UserTask     string
	// History is an optional bounded transcript of prior conversation turns
	// (user/assistant), rendered into the prompt as a "Conversation so far"
	// section before the current Task. Empty means a fresh, first-turn run.
	History         string
	CWD             string
	AgentID         string
	MaxSteps        int
	RequireToolCall bool
	AllowedTools    []string
	ToolPolicies    map[string]config.ToolSpec
	LogToolCalls    bool
	ProviderManager *provider.Manager
	ProviderName    func() string
	ModelName       func() string
	MaxTokens       int                    // max tokens per request; 0 = unlimited
	Thinking        provider.ThinkingLevel // forwarded to provider.Request.Thinking
	RequiredReads   []string
	Images          []string        // only used on first LLM call (chat use case)
	ToolCallback    func(ToolTrace) // optional: called with status="running" before and final status after each tool
	Permission      config.PermissionLevel
	ShellApproval   ShellApprovalCallback
	AskUser         AskUserCallback
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
	sessionDir    string
	agentID       string
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
}

// parallelResult holds the outcome of a single tool execution in a parallel batch.
type parallelResult struct {
	agentID string
	name    string
	args    string
	output  string
	status  string
}

// runToolLoop returns the final output, the tool traces, the cumulative token
// count (sum of every step's prompt+completion — the session COST), the
// approximate context occupancy (the largest single-step prompt+completion,
// which best estimates how full the window is — see EFF-3), and an error.
//
// Cost and occupancy are deliberately distinct: each step's prompt re-embeds
// the rolling history, so summing every step double-counts the same context.
// The gauge must use occupancy, not the cumulative sum.
func runToolLoop(ctx context.Context, cfg toolLoopConfig) (string, []ToolTrace, int, int, error) {
	if cfg.ProviderManager == nil {
		return "", nil, 0, 0, fmt.Errorf("missing provider manager")
	}
	if cfg.ProviderName == nil || cfg.ModelName == nil {
		return "", nil, 0, 0, fmt.Errorf("missing provider/model selectors")
	}
	if strings.TrimSpace(cfg.UserTask) == "" {
		return "", nil, 0, 0, fmt.Errorf("empty task")
	}
	if cfg.MaxSteps <= 0 {
		cfg.MaxSteps = 8
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
		readSet:         map[string]struct{}{},
		requiredReads:   map[string]struct{}{},
		permission:      cfg.Permission,
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
		sessionDir:      cfg.SessionDir,
		agentID:         cfg.AgentID,
		parentID:        cfg.ParentAgentID,
		delegationDepth: cfg.DelegationDepth,
		skillsCatalog:   cfg.SkillsCatalog,
	}
	if !cfg.LogToolCalls {
		runtime.logToolCalls = false
	}
	if cfg.Manifest != nil {
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
		cfg.MaxWorkers = 4
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
	allowedShell, err := loadAllowedCommandSet(cfg.CWD)
	if err != nil {
		return "", nil, 0, 0, err
	}
	runtime.allowedShell = allowedShell
	hooksCfg, err := hooks.LoadEffective(cfg.CWD)
	if err != nil {
		return "", nil, 0, 0, err
	}
	runtime.hooksConfig = hooksCfg
	if err := runtime.runSessionStartHooks(ctx); err != nil {
		return "", nil, 0, 0, err
	}
	for _, p := range cfg.RequiredReads {
		p = filepath.ToSlash(strings.TrimSpace(p))
		if p != "" {
			runtime.requiredReads[p] = struct{}{}
		}
	}
	usedTool := false
	var traces []ToolTrace
	var lastContent string // last non-empty response, used as fallback if max steps hit

	var history strings.Builder
	for step := 1; step <= cfg.MaxSteps; step++ {
		prompt := buildLoopPrompt(cfg, tailTrimHistory(history.String(), maxHistoryBytes), step)
		if err := budget.Validate(cfg.MaxTokens, prompt); err != nil {
			return "", traces, totalTokens, contextTokens, err
		}
		req := provider.Request{
			Prompt:    prompt,
			MaxTokens: cfg.MaxTokens,
			Thinking:  cfg.Thinking,
		}
		if step == 1 && len(cfg.Images) > 0 {
			req.Images = cfg.Images
		}
		resp, err := cfg.ProviderManager.Send(ctx, cfg.ProviderName(), cfg.ModelName(), req)
		if err != nil {
			return "", traces, totalTokens, contextTokens, fmt.Errorf("agent call failed: %w", err)
		}
		totalTokens += resp.EstimatedTokens
		// Occupancy ~= the largest single request (prompt+completion). The
		// last step usually has the most accumulated history, but using the
		// max is robust even if the final completion is short.
		if resp.EstimatedTokens > contextTokens {
			contextTokens = resp.EstimatedTokens
		}

		content := strings.TrimSpace(resp.Content)
		main, _ := stripThinkTags(content)
		main = strings.TrimSpace(main)
		if main == "" {
			continue
		}
		lastContent = main

		calls, parseErrs := parseAllToolCalls(main)
		if len(calls) > 0 || len(parseErrs) > 0 {
			if len(calls) > 0 {
				usedTool = true
			}
			results := runtime.parallelExec(ctx, calls, allowed, cfg.ToolCallback)
			history.WriteString(fmt.Sprintf("assistant(%d): %s\n", step, singleLine(main)))
			for _, res := range results {
				trace := ToolTrace{AgentID: res.agentID, Name: res.name, Status: res.status, Args: res.args, Output: truncate(res.output, 600)}
				traces = append(traces, trace)
				// The LLM must always receive tool outcomes in the next step, even when
				// human-facing tool logging is disabled in the manifest.
				history.WriteString(fmt.Sprintf("tool(%d)[%s]: %s\n", step, res.name, summarizeLoopToolResult(res.name, res.args, res.status, res.output)))
			}
			if runtime.shouldStop() {
				return runtime.stopMessage(), traces, totalTokens, contextTokens, nil
			}
			for _, perr := range parseErrs {
				history.WriteString(fmt.Sprintf("tool(%d): parse error: %s — fix the JSON and retry\n", step, perr))
			}
			history.WriteString("\n")
			continue
		}

		if final, ok := parseFinal(main); ok {
			if next, ok := runtime.nextRequiredRead(); ok {
				history.WriteString(fmt.Sprintf("assistant(%d): %s\n", step, singleLine(main)))
				history.WriteString(fmt.Sprintf("system: you must read %q with file-read before FINAL.\n\n", next))
				continue
			}
			if cfg.RequireToolCall && !usedTool {
				// LLM tried to finalize without using any tools: nudge it and retry.
				history.WriteString(fmt.Sprintf("assistant(%d): %s\n", step, singleLine(main)))
				history.WriteString(fmt.Sprintf("system: you must use at least one tool before writing FINAL.\n\n"))
				continue
			}
			return strings.TrimSpace(final), traces, totalTokens, contextTokens, nil
		}

		// Plain text without FINAL prefix and without a tool call.
		if cfg.RequireToolCall {
			history.WriteString(fmt.Sprintf("assistant(%d): %s\n", step, singleLine(main)))
			if !usedTool {
				history.WriteString("system: use TOOL_CALL before providing the final answer.\n\n")
			} else {
				history.WriteString("system: output FINAL on its own line followed by your final answer. Do not write TOOL_CALL as text.\n\n")
			}
			continue
		}
		return main, traces, totalTokens, contextTokens, nil
	}

	// Max steps exhausted: return whatever content we accumulated rather than discarding it.
	if lastContent != "" {
		return lastContent, traces, totalTokens, contextTokens, nil
	}
	return "", traces, totalTokens, contextTokens, fmt.Errorf("max tool steps reached without final answer")
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
				agentID: r.agentID,
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
					agentID: r.agentID,
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
				callback(ToolTrace{AgentID: r.agentID, Name: "comment", Status: "success", Args: fmt.Sprintf(`{"message":%q}`, msg), Output: msg})
			}
			if callback != nil {
				callback(ToolTrace{AgentID: r.agentID, Name: c.Tool, Args: callArgs, Status: "running"})
			}
			output, err := r.executeWithTimeout(ctx, c, allowed)
			status := "success"
			if err != nil {
				status = "error"
				output = "error: " + err.Error()
			}
			results[idx] = parallelResult{
				agentID: r.agentID,
				name:    c.Tool,
				args:    callArgs,
				output:  output,
				status:  status,
			}
			if callback != nil {
				callback(ToolTrace{AgentID: r.agentID, Name: c.Tool, Status: status, Args: callArgs, Output: truncate(output, 600)})
				if isMajorOperationTool(c.Tool) {
					msg := fmt.Sprintf("Completed %s.", c.Tool)
					if err != nil {
						msg = fmt.Sprintf("Failed %s: %s", c.Tool, truncate(err.Error(), 180))
					}
					callback(ToolTrace{AgentID: r.agentID, Name: "comment", Status: "success", Args: fmt.Sprintf(`{"message":%q}`, msg), Output: msg})
				}
			}
		}(i, call)
	}
	wg.Wait()
	return results
}

func (r *toolRuntime) executeWithTimeout(ctx context.Context, call toolCall, allowed map[string]struct{}) (string, error) {
	timeoutSec := 45
	if spec, ok := r.toolPolicies[call.Tool]; ok && spec.TimeoutSec > 0 {
		timeoutSec = spec.TimeoutSec
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
		return truncate(out, 8000), nil
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
			content = sliceLines(content, args.StartLine, args.EndLine)
		}
		return truncate(content, 12000), nil
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
		if exists {
			r.mu.Lock()
			_, alreadyRead := r.readSet[rel]
			r.mu.Unlock()
			if !alreadyRead {
				return "", fmt.Errorf("refusing write: read %q first", rel)
			}
		}
		if err := r.authorizeWriteAccess(ctx, "file-write", rel); err != nil {
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
		if exists {
			return fmt.Sprintf("updated %s", rel), nil
		}
		return fmt.Sprintf("created %s", rel), nil
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
		return r.runGrep(ctx, gargs)
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
		if err := r.authorizeNetworkAccess(ctx, "web-fetch", "web-fetch"); err != nil {
			return "", err
		}
		return r.runWebFetch(ctx, call.Args)
	case "web-search":
		return r.runWebSearch(ctx, call.Args)
	case "grok-image":
		return r.runGrokImage(ctx, call.Args)
	case "grok-video":
		return r.runGrokVideo(ctx, call.Args)
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
	case "task-stop":
		return r.runTaskStop(call.Args)
	case "tool-search":
		return r.runToolSearch(allowed, call.Args)
	case "skill-read", "activate-skill", "skill-activate":
		return r.runSkillRead(call.Args)
	case "skill-list":
		return r.runSkillList(call.Args)
	case "config":
		return r.runConfigTool(call.Args)
	case "mcp-list-resources":
		return r.runMCPListResources(ctx, call.Args)
	case "mcp-read-resource":
		return r.runMCPReadResource(ctx, call.Args)
	case "mcp-auth":
		return r.runMCPAuth(ctx, call.Args)
	case "todo-write":
		var args struct {
			Todos []interface{} `json:"todos"`
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
			m, ok := item.(map[string]interface{})
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
			if rawDeps, ok := m["dependencies"].([]interface{}); ok {
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
		raw, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			return "", fmt.Errorf("todo-write: marshal: %w", err)
		}
		todosPath := filepath.Join(r.sessionDir, "todos.json")
		tasksPath := filepath.Join(r.sessionDir, "tasks.json")
		if err := os.MkdirAll(filepath.Dir(todosPath), 0o700); err != nil {
			return "", fmt.Errorf("todo-write: mkdir: %w", err)
		}
		if err := os.WriteFile(todosPath, raw, 0o644); err != nil {
			return "", fmt.Errorf("todo-write: write: %w", err)
		}
		if err := os.WriteFile(tasksPath, raw, 0o644); err != nil {
			return "", fmt.Errorf("todo-write: write tasks: %w", err)
		}
		return fmt.Sprintf("wrote %d todos", len(out)), nil
	case "file-edit":
		return r.runFileEdit(ctx, call.Args)
	case "enter-worktree":
		return r.runEnterWorktree(ctx, call.Args)
	case "exit-worktree":
		return r.runExitWorktree(ctx, call.Args)
	case "send-message":
		return r.runSendMessage(call.Args)
	case "bash", "bash-output":
		return r.runShellTool(ctx, call.Tool, call.Args, "bash")
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
				allowedHandoff := false
				for _, id := range caller.Handoffs {
					if id == target {
						allowedHandoff = true
						break
					}
				}
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
		if r.permission != "" {
			subSpec.Permission = r.permission
		}
		subAgent := LLMAgent{
			Spec:            subSpec,
			ProviderManager: r.providerMgr,
			ProviderName:    r.providerName,
			ModelName:       r.modelName,
			CWD:             r.cwd,
			MaxTokens:       r.maxTokens,
			Thinking:        r.thinkingLevel,
			ToolCallback:    r.toolCallback,
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
	default:
		return "", fmt.Errorf("unsupported tool %q", call.Tool)
	}
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
			found := false
			for _, e := range exts {
				if ext == e {
					found = true
					break
				}
			}
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
				start := mi - args.Context
				if start < 0 {
					start = 0
				}
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
	rel = filepath.ToSlash(rel)
	return abs, rel, nil
}

// searchLineNumberRE matches ":<digits>" segments in repo-search output, used
// by markReadFromSearch to detect ripgrep-style "path:lineno:..." rows.
var searchLineNumberRE = regexp.MustCompile(`^\d+$`)

func (r *toolRuntime) markReadFromSearch(out string) {
	lines := strings.Split(out, "\n")
	for _, line := range lines {
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
