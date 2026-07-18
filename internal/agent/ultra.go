package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"spettro/internal/config"
	"spettro/internal/provider"
)

// Ultra: swarm-style sub-agent fan-out. When the user enables Ultra, the
// top-level agent gains the "ultra" tool, which expands a prompt template over
// a list of items and runs one sub-agent per item in parallel. Sub-agents
// inherit the parent's model/provider/thinking/permissions, so Ultra works
// with any model.

const (
	ultraToolID = "ultra"
	// ultraMaxSubagents caps a single ultra call. ;
	// Spettro starts conservative — raise this constant if needed.
	ultraMaxSubagents = 32
	// Launch ramp: start up to ultraInitialLaunch sub-agents immediately, then
	// one more every ultraLaunchInterval to avoid hammering the provider.
	ultraInitialLaunch  = 5
	ultraLaunchInterval = 700 * time.Millisecond
	// Transient provider failures (rate limits, availability) are retried per
	// sub-agent with exponential backoff: 3s, 6s, 12s.
	ultraRetryBase    = 3 * time.Second
	ultraMaxAttempts  = 3
	ultraItemTemplate = "{{item}}"
	// ultraConcurrencyEnv optionally hard-caps how many sub-agents run at once.
	ultraConcurrencyEnv = "SPETTRO_ULTRA_MAX_CONCURRENCY"
)

// ultraPromptSection is appended to the top-level system prompt when Ultra is
// enabled. It must stay byte-stable for the whole run (prompt-cache contract),
// which holds because LLMAgent.Ultra is read once at run construction.
const ultraPromptSection = `

ULTRA MODE (agent swarm) is active. The user enabled Ultra: they want the main work of hard or decomposable tasks fanned out across many parallel sub-agents via the ultra tool.
- First do light exploration yourself (read/search just enough to decompose correctly). Then do not handle the main work yourself: call the ultra tool with a prompt_template containing {{item}} and an items list, one item per independent unit of work.
- Give each sub-agent a distinct, non-overlapping scope. Sub-agents cannot see your context or each other's work, so each filled prompt must be self-contained: include file paths, constraints, and the expected output.
- Do not try to conserve the number of agents: decompose work as finely as independence allows (per file, per package, per test suite).
- Sub-agents run concurrently on disjoint scopes; never assign two agents work that touches the same file.
- After the ultra call returns, review the aggregated results, fix or re-dispatch failures, and integrate/verify the final outcome yourself.
- For trivial single-step tasks, just do them directly — ultra is for parallelizable or hard work.`

type ultraArgs struct {
	Description    string   `json:"description"`
	SubagentType   string   `json:"subagent_type"`
	PromptTemplate string   `json:"prompt_template"`
	Items          []string `json:"items"`
}

type ultraResult struct {
	item    string
	content string
	err     error
}

// resolveUltraTarget picks the sub-agent spec an ultra call runs: the explicit
// subagent_type when given, otherwise the "code" worker, otherwise the first
// enabled worker/subagent in the manifest.
func resolveUltraTarget(manifest *config.AgentManifest, subagentType string) (config.AgentSpec, error) {
	target := strings.TrimSpace(subagentType)
	if target == "" {
		target = "code"
		if _, ok := manifest.AgentByID(target); !ok {
			target = ""
			for _, a := range manifest.Agents {
				if a.Enabled && (a.Role == config.AgentRoleWorker || a.Role == config.AgentRoleSubagent) {
					target = a.ID
					break
				}
			}
		}
	}
	if target == "" {
		return config.AgentSpec{}, fmt.Errorf("ultra: no worker agent available in manifest")
	}
	spec, ok := manifest.AgentByID(target)
	if !ok {
		return config.AgentSpec{}, fmt.Errorf("ultra: unknown subagent_type %q", target)
	}
	if !spec.Enabled {
		return config.AgentSpec{}, fmt.Errorf("ultra: subagent_type %q is disabled", target)
	}
	if spec.Mode == "orchestrator" || (spec.Role != config.AgentRoleWorker && spec.Role != config.AgentRoleSubagent) {
		return config.AgentSpec{}, fmt.Errorf("ultra: subagent_type %q must be a worker/subagent, got role %q", target, spec.Role)
	}
	return spec, nil
}

// ultraMaxConcurrency reads the optional hard cap from the environment.
// 0 means uncapped (the launch ramp is the only throttle).
func ultraMaxConcurrency() int {
	raw := strings.TrimSpace(os.Getenv(ultraConcurrencyEnv))
	if raw == "" {
		return 0
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return 0
	}
	return n
}

func expandUltraPrompts(template string, items []string) ([]string, error) {
	template = strings.TrimSpace(template)
	if template == "" {
		return nil, fmt.Errorf("ultra: prompt_template is required")
	}
	if !strings.Contains(template, ultraItemTemplate) {
		return nil, fmt.Errorf("ultra: prompt_template must contain the %s placeholder", ultraItemTemplate)
	}
	if len(items) < 2 {
		return nil, fmt.Errorf("ultra: at least 2 items are required (use the agent tool for a single delegation)")
	}
	if len(items) > ultraMaxSubagents {
		return nil, fmt.Errorf("ultra: too many items (%d); the limit is %d", len(items), ultraMaxSubagents)
	}
	prompts := make([]string, 0, len(items))
	seen := map[string]struct{}{}
	for _, item := range items {
		item = strings.TrimSpace(item)
		if item == "" {
			return nil, fmt.Errorf("ultra: items must be non-empty")
		}
		prompt := strings.ReplaceAll(template, ultraItemTemplate, item)
		if _, dup := seen[prompt]; dup {
			return nil, fmt.Errorf("ultra: duplicate item %q — every filled prompt must be distinct", item)
		}
		seen[prompt] = struct{}{}
		prompts = append(prompts, prompt)
	}
	return prompts, nil
}

func (r *toolRuntime) runUltra(ctx context.Context, rawArgs json.RawMessage) (string, error) {
	var args ultraArgs
	if err := decodeJSONStrict(rawArgs, &args); err != nil {
		return "", fmt.Errorf("ultra args: %w", err)
	}
	if r.delegationDepth > 0 {
		return "", fmt.Errorf("ultra: only the top-level agent can start a swarm")
	}
	if r.manifest == nil || r.providerMgr == nil {
		return "", fmt.Errorf("ultra: sub-agent execution not configured")
	}
	// Defense in depth: hosts refuse to enable Ultra under ask-first, but the
	// permission level can change mid-run, and a swarm under ask-first would
	// flood the user with approval prompts.
	if r.perm() == config.PermissionAskFirst {
		return "", fmt.Errorf("ultra: requires restricted or yolo permission (current: ask-first)")
	}
	prompts, err := expandUltraPrompts(args.PromptTemplate, args.Items)
	if err != nil {
		return "", err
	}
	spec, err := resolveUltraTarget(r.manifest, args.SubagentType)
	if err != nil {
		return "", err
	}
	// The parent's effective permission cascades to sub-agents, same as the
	// agent tool, so a user-level setting (e.g. yolo) holds across the swarm.
	subSpec := spec
	if p := r.perm(); p != "" {
		subSpec.Permission = p
	}

	results := make([]ultraResult, len(prompts))
	var sem chan struct{}
	if limit := ultraMaxConcurrency(); limit > 0 {
		sem = make(chan struct{}, limit)
	}
	var wg sync.WaitGroup
	for i := range prompts {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			res := ultraResult{item: strings.TrimSpace(args.Items[idx])}
			// Launch ramp: everything past the initial batch is staggered.
			if idx >= ultraInitialLaunch {
				delay := time.Duration(idx-ultraInitialLaunch+1) * ultraLaunchInterval
				if !ultraSleep(ctx, delay) {
					res.err = ctx.Err()
					results[idx] = res
					return
				}
			}
			if sem != nil {
				select {
				case sem <- struct{}{}:
					defer func() { <-sem }()
				case <-ctx.Done():
					res.err = ctx.Err()
					results[idx] = res
					return
				}
			}
			res.content, res.err = r.runUltraSubagent(ctx, subSpec, prompts[idx])
			results[idx] = res
		}(i)
	}
	wg.Wait()
	if ctx.Err() != nil {
		return "", fmt.Errorf("ultra: %w", ctx.Err())
	}
	return renderUltraResults(subSpec.ID, results), nil
}

// runUltraSubagent runs one sub-agent to completion, retrying transient
// provider failures (rate limits, availability) with exponential backoff.
func (r *toolRuntime) runUltraSubagent(ctx context.Context, spec config.AgentSpec, task string) (string, error) {
	sub := LLMAgent{
		Spec:            spec,
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
		ParentAgentID:   r.agentID,
	}
	var lastErr error
	for attempt := 0; attempt < ultraMaxAttempts; attempt++ {
		if attempt > 0 {
			if !ultraSleep(ctx, ultraRetryBase<<(attempt-1)) {
				return "", ctx.Err()
			}
		}
		result, err := sub.Run(ctx, task)
		if err == nil {
			return strings.TrimSpace(result.Content), nil
		}
		lastErr = err
		if ctx.Err() != nil || !provider.Classify(err).Transient() {
			break
		}
	}
	return "", lastErr
}

// ultraSleep waits for d or until ctx is cancelled; false means cancelled.
func ultraSleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// renderUltraResults aggregates every sub-agent outcome, in input order, into
// the single block the main agent sees. Each sub-agent's final message is its
// entire handoff — nothing else crosses the boundary.
func renderUltraResults(subagentType string, results []ultraResult) string {
	completed, failed := 0, 0
	var body strings.Builder
	for i, res := range results {
		outcome := "completed"
		text := truncate(res.content, 4000)
		if res.err != nil {
			outcome = "failed"
			failed++
			text = "error: " + truncate(res.err.Error(), 600)
		} else {
			completed++
		}
		fmt.Fprintf(&body, "<subagent index=%q type=%q item=%q outcome=%q>\n%s\n</subagent>\n",
			strconv.Itoa(i+1), subagentType, truncate(res.item, 200), outcome, text)
	}
	out := fmt.Sprintf("<ultra_result>\n<summary>completed: %d, failed: %d</summary>\n%s</ultra_result>", completed, failed, body.String())
	if failed > 0 {
		out += "\nSome sub-agents failed. Re-dispatch the failed items with a corrected prompt via ultra, or handle them directly."
	}
	return out
}
