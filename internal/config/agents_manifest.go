package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"

	"spettro/internal/sandbox"
)

const AgentManifestFilename = "spettro.agents.toml"

type SandboxMode string

const (
	SandboxWorkspaceWrite SandboxMode = "workspace-write"
	SandboxReadOnly       SandboxMode = "read-only"
	SandboxFullAccess     SandboxMode = "full-access"
	// SandboxOff is an accepted input alias of SandboxFullAccess. The
	// canonical persisted disabled value stays "full-access" so manifests keep
	// loading under pre-v3 binaries, whose validation rejects "off".
	SandboxOff SandboxMode = "off"
)

type AgentRole string

const (
	AgentRolePrimary      AgentRole = "primary"
	AgentRoleSubagent     AgentRole = "subagent"
	AgentRoleOrchestrator AgentRole = "orchestrator"
	AgentRoleWorker       AgentRole = "worker"
)

type RuleAction string

const (
	RuleAllow RuleAction = "allow"
	RuleAsk   RuleAction = "ask"
	RuleDeny  RuleAction = "deny"
)

type PermissionRule struct {
	Permission string     `toml:"permission"`
	Pattern    string     `toml:"pattern"`
	Action     RuleAction `toml:"action"`
}

type DelegationPolicy struct {
	MaxParallelWorkers  int `toml:"max_parallel_workers"`
	MaxDepth            int `toml:"max_depth"`
	MaxToolCallsPerStep int `toml:"max_tool_calls_per_step"`
}

type AgentManifest struct {
	Version      int           `toml:"version"`
	DefaultAgent string        `toml:"default_agent"`
	Metadata     AgentMetadata `toml:"metadata"`
	Runtime      RuntimePolicy `toml:"runtime"`
	Tools        []ToolSpec    `toml:"tools"`
	Agents       []AgentSpec   `toml:"agents"`
}

type AgentMetadata struct {
	Name        string `toml:"name"`
	Description string `toml:"description"`
}

type RuntimePolicy struct {
	DefaultPermission PermissionLevel `toml:"default_permission"`
	DefaultTimeoutSec int             `toml:"default_timeout_sec"`
	LogToolCalls      bool            `toml:"log_tool_calls"`
	SandboxMode       SandboxMode     `toml:"sandbox_mode"`
	// SandboxNet, SandboxAllowDirs and SandboxAllowReadDirs extend the OS
	// sandbox policy: network scope ("all", "localhost", "none",
	// "ports:443,8080"), extra writable roots, and extra readable-only roots
	// (e.g. a toolchain cache outside the workspace when reads are confined).
	// omitempty keeps migrated manifests loadable by older binaries, whose
	// strict decoder rejects unknown fields.
	SandboxNet           string           `toml:"sandbox_net,omitempty"`
	SandboxAllowDirs     []string         `toml:"sandbox_allow_dirs,omitempty"`
	SandboxAllowReadDirs []string         `toml:"sandbox_allow_read_dirs,omitempty"`
	Delegation           DelegationPolicy `toml:"delegation"`
	PermissionRules      []PermissionRule `toml:"permission_rules"`
	AllowNetworkTools    bool             `toml:"allow_network_tools"` // legacy field; ignored
	// Limits overrides the default character caps for tool output retained in
	// model context. Zero values fall back to the built-in defaults.
	Limits ContextLimits `toml:"limits,omitempty"`
	// Fallback configures model availability routing: what to do when the
	// selected model fails with a quota/server/timeout error.
	Fallback FallbackPolicy `toml:"fallback,omitempty"`
	// LoopDetection configures detection of the agent repeating itself
	// (identical tool calls or identical text output) so a run is nudged and
	// then stopped instead of burning tokens.
	LoopDetection LoopDetectionPolicy `toml:"loop_detection,omitempty"`
}

// LoopDetectionPolicy configures agent repetition detection. Zero values for
// the thresholds fall back to built-in defaults; Disabled turns the feature
// off entirely.
type LoopDetectionPolicy struct {
	// Disabled turns loop detection off.
	Disabled bool `toml:"disabled,omitempty"`
	// ConsecutiveThreshold trips after N identical consecutive tool calls
	// (same tool, same normalized args). Default 3.
	ConsecutiveThreshold int `toml:"consecutive_threshold,omitempty"`
	// WindowSize is the rolling window of recent tool calls inspected for
	// non-consecutive repetition. Default 20.
	WindowSize int `toml:"window_size,omitempty"`
	// WindowRepeatThreshold trips when the same call/args pair occurs M times
	// within the window. Default 5.
	WindowRepeatThreshold int `toml:"window_repeat_threshold,omitempty"`
	// TextRepeatThreshold trips after N identical consecutive assistant text
	// outputs. Default 3.
	TextRepeatThreshold int `toml:"text_repeat_threshold,omitempty"`
}

// FallbackMode controls how a fallback model is adopted after the primary
// model fails with a transient availability error.
type FallbackMode string

const (
	// FallbackPrompt asks the user before switching the main conversation to
	// a fallback model (a swap invalidates the provider prompt cache).
	FallbackPrompt FallbackMode = "prompt"
	// FallbackSilent switches without asking. Only honoured on the main
	// conversation when no interactive prompt is available (headless runs);
	// internal utility calls always fall back silently.
	FallbackSilent FallbackMode = "silent"
	// FallbackOff disables fallback routing entirely.
	FallbackOff FallbackMode = "off"
)

// FallbackPolicy configures the fallback chain for model availability
// failures. Chain entries and InternalModel are "provider/model" refs.
type FallbackPolicy struct {
	// Mode: "prompt" (default), "silent", or "off".
	Mode FallbackMode `toml:"mode,omitempty"`
	// Chain is the ordered list of fallback models tried after the primary.
	Chain []string `toml:"chain,omitempty"`
	// InternalModel, when set, routes internal utility calls (compaction,
	// titling, classification) to a designated small/cheap model with silent
	// fallback, independent of the UI-selected model.
	InternalModel string `toml:"internal_model,omitempty"`
}

// ContextLimits caps how many characters of tool output are retained in model
// context. Zero for any field means use the built-in default.
type ContextLimits struct {
	// FileReadChars is the max chars returned by file-read and kept in history.
	FileReadChars int `toml:"file_read_chars"`
	// SearchChars is the max chars returned by repo-search/grep/glob/ls and kept in history.
	SearchChars int `toml:"search_chars"`
	// ToolOutputChars is the max chars for other tools (shell, agent, etc.).
	ToolOutputChars int `toml:"tool_output_chars"`
}

type ToolSpec struct {
	ID               string           `toml:"id"`
	Name             string           `toml:"name"`
	Description      string           `toml:"description"`
	Kind             string           `toml:"kind"`
	Enabled          bool             `toml:"enabled"`
	EntryPoint       string           `toml:"entry_point"`
	TimeoutSec       int              `toml:"timeout_sec"`
	RequiresApproval bool             `toml:"requires_approval"`
	PermittedActions []string         `toml:"permitted_actions"`
	Aliases          []string         `toml:"aliases"`
	InputSchema      map[string]any   `toml:"input_schema"`
	RiskLevel        string           `toml:"risk_level"`
	PrimaryOnly      bool             `toml:"primary_only"`
	PermissionRules  []PermissionRule `toml:"permission_rules"`
}

type AgentSpec struct {
	ID               string           `toml:"id"`
	Name             string           `toml:"name"`
	Description      string           `toml:"description"`
	Skill            string           `toml:"skill"`
	Mode             string           `toml:"mode"`
	Role             AgentRole        `toml:"role"`
	Color            string           `toml:"color"`
	ModelProvider    string           `toml:"model_provider"`
	Model            string           `toml:"model"`
	SystemPrompt     string           `toml:"system_prompt"`
	PromptFile       string           `toml:"prompt_file"`
	AllowedTools     []string         `toml:"allowed_tools"`
	PermittedActions []string         `toml:"permitted_actions"`
	Permission       PermissionLevel  `toml:"permission"`
	PermissionRules  []PermissionRule `toml:"permission_rules"`
	Temperature      float64          `toml:"temperature"`
	MaxTokens        int              `toml:"max_tokens"`
	// Deprecated: ignored. The tool loop is unbounded; retained only so existing
	// manifests with `max_steps` keep parsing under DisallowUnknownFields.
	MaxSteps int      `toml:"max_steps"`
	Handoffs []string `toml:"handoffs"`
	Enabled  bool     `toml:"enabled"`
}

// visionToolViewImage is shared between the default manifest and the v5
// migration that retrofits it into existing manifests. It is the generic
// "let the model see an image" tool: the agent produces images however it
// likes (its own headless-browser screenshot via shell, a generated chart,
// an existing asset) and views the file.
// lspDeepTools are shared between the default manifest and the v6 migration
// that retrofits them into existing manifests (hover for type info, and
// rename-symbol for LSP-powered cross-file renames).
var lspDeepTools = []ToolSpec{
	{ID: "hover", Name: "LSP Hover", Description: "Type signature and documentation for a symbol via the language server.", Kind: "builtin", Enabled: true, TimeoutSec: 30, RequiresApproval: false, PermittedActions: []string{"read", "search"}, RiskLevel: "low"},
	{ID: "rename-symbol", Name: "LSP Rename", Description: "Rename a symbol across the workspace via the language server.", Kind: "builtin", Enabled: true, TimeoutSec: 60, RequiresApproval: true, PermittedActions: []string{"write"}, RiskLevel: "high"},
}

var visionToolViewImage = ToolSpec{ID: "view-image", Name: "View Image", Description: "Attach an image file from the workspace so the model can see it (vision models only).", Kind: "builtin", Enabled: true, TimeoutSec: 15, RequiresApproval: false, PermittedActions: []string{"read"}, RiskLevel: "low"}

func DefaultAgentManifest() AgentManifest {
	m := AgentManifest{
		// Version starts below the latest migration on purpose: the
		// normalizeFromVersion call below runs the same upgrades a loaded
		// manifest gets (v5 fills agent allow-lists with the vision tools),
		// keeping fresh defaults and migrated manifests identical.
		Version:      3,
		DefaultAgent: "plan",
		Metadata: AgentMetadata{
			Name:        "Spettro default agents",
			Description: "Built-in fallback manifest when no spettro.agents.toml is present.",
		},
		Runtime: RuntimePolicy{
			DefaultPermission: PermissionAskFirst,
			DefaultTimeoutSec: 120,
			LogToolCalls:      true,
			// The OS sandbox is opt-in: full-access here means "not configured";
			// activation comes from the --sandbox flag or an explicit manifest
			// setting.
			SandboxMode: SandboxFullAccess,
			Delegation:  DelegationPolicy{MaxParallelWorkers: 2, MaxDepth: 2},
		},
		Tools: []ToolSpec{
			{ID: "glob", Name: "Glob", Description: "Find files by name pattern.", Kind: "builtin", Enabled: true, TimeoutSec: 30, RequiresApproval: false, PermittedActions: []string{"read", "search"}, RiskLevel: "low"},
			{ID: "grep", Name: "Grep", Description: "Search file contents with regex.", Kind: "builtin", Enabled: true, TimeoutSec: 30, RequiresApproval: false, PermittedActions: []string{"read", "search"}, RiskLevel: "low"},
			{ID: "file-read", Name: "File Reader", Description: "Reads file contents in the workspace.", Kind: "builtin", Enabled: true, TimeoutSec: 30, RequiresApproval: false, PermittedActions: []string{"read"}, RiskLevel: "low"},
			{ID: "file-write", Name: "File Writer", Description: "Creates and edits files in the workspace.", Kind: "builtin", Enabled: true, TimeoutSec: 60, RequiresApproval: true, PermittedActions: []string{"write"}, RiskLevel: "high"},
			{ID: "file-edit", Name: "File Edit", Description: "Apply targeted edits to existing files.", Kind: "builtin", Enabled: true, TimeoutSec: 60, RequiresApproval: true, PermittedActions: []string{"write"}, RiskLevel: "high"},
			{ID: "multi-edit", Name: "Multi Edit", Description: "Apply several find/replace edits to one file in a single atomic call.", Kind: "builtin", Enabled: true, TimeoutSec: 60, RequiresApproval: true, PermittedActions: []string{"write"}, RiskLevel: "high"},
			{ID: "diagnostics", Name: "LSP Diagnostics", Description: "Fetch language-server diagnostics for a file or the workspace.", Kind: "builtin", Enabled: true, TimeoutSec: 30, RequiresApproval: false, PermittedActions: []string{"read", "search"}, RiskLevel: "low"},
			{ID: "references", Name: "LSP References", Description: "Find references or the definition of a symbol via the language server.", Kind: "builtin", Enabled: true, TimeoutSec: 30, RequiresApproval: false, PermittedActions: []string{"read", "search"}, RiskLevel: "low"},
			{ID: "lsp-restart", Name: "LSP Restart", Description: "Restart a wedged language server.", Kind: "builtin", Enabled: true, TimeoutSec: 30, RequiresApproval: false, PermittedActions: []string{"read"}, RiskLevel: "low"},
			{ID: "shell-exec", Name: "Shell Executor", Description: "Runs shell commands in the project directory.", Kind: "builtin", Enabled: true, TimeoutSec: 120, RequiresApproval: true, PermittedActions: []string{"execute", "git"}, RiskLevel: "high"},
			{ID: "repo-search", Name: "Repository Search", Description: "Searches file names and content inside the project.", Kind: "builtin", Enabled: true, TimeoutSec: 30, RequiresApproval: false, PermittedActions: []string{"read", "search"}, RiskLevel: "low"},
			{ID: "tool-search", Name: "Tool Search", Description: "Search available tools for current agent.", Kind: "builtin", Enabled: true, TimeoutSec: 20, RequiresApproval: false, PermittedActions: []string{"read", "search"}, RiskLevel: "low"},
			{ID: "ls", Name: "List Directory", Description: "List directory contents.", Kind: "builtin", Enabled: true, TimeoutSec: 10, RequiresApproval: false, PermittedActions: []string{"read", "search"}, RiskLevel: "low"},
			{ID: "save-memory", Name: "Save Memory", Description: "Save one short durable fact or preference to persistent cross-session memory.", Kind: "builtin", Enabled: true, TimeoutSec: 10, RequiresApproval: false, PermittedActions: []string{"write", "ask"}, RiskLevel: "low"},
			{ID: "todo-write", Name: "Todo Write", Description: "Write a list of todos to track task progress.", Kind: "builtin", Enabled: true, TimeoutSec: 10, RequiresApproval: false, PermittedActions: []string{"write"}, RiskLevel: "medium"},
			{ID: "task-create", Name: "Task Create", Description: "Create a structured task in session state.", Kind: "builtin", Enabled: true, TimeoutSec: 10, RequiresApproval: false, PermittedActions: []string{"write"}, RiskLevel: "low"},
			{ID: "task-get", Name: "Task Get", Description: "Fetch one task by id.", Kind: "builtin", Enabled: true, TimeoutSec: 10, RequiresApproval: false, PermittedActions: []string{"read"}, RiskLevel: "low"},
			{ID: "task-update", Name: "Task Update", Description: "Update a task by id.", Kind: "builtin", Enabled: true, TimeoutSec: 10, RequiresApproval: false, PermittedActions: []string{"write"}, RiskLevel: "low"},
			{ID: "task-list", Name: "Task List", Description: "List tasks in current session.", Kind: "builtin", Enabled: true, TimeoutSec: 10, RequiresApproval: false, PermittedActions: []string{"read"}, RiskLevel: "low"},
			{ID: "task-stop", Name: "Task Stop", Description: "Request stopping the current agent run.", Kind: "builtin", Enabled: true, TimeoutSec: 5, RequiresApproval: false, PermittedActions: []string{"plan", "ask"}, RiskLevel: "low"},
			{ID: "goal-complete", Name: "Goal Complete", Description: "Declare the goal fully achieved and verified; ends the run. Only call after you have confirmed the objective is met (tests pass / build green / change applied).", Kind: "builtin", Enabled: true, TimeoutSec: 5, RequiresApproval: false, PermittedActions: []string{"plan", "ask"}, RiskLevel: "low"},
			{ID: "config", Name: "Config", Description: "Read or update selected runtime settings.", Kind: "builtin", Enabled: true, TimeoutSec: 10, RequiresApproval: false, PermittedActions: []string{"read", "write", "plan"}, RiskLevel: "medium"},
			{ID: "bash", Name: "Bash", Description: "Execute a bash command and return output.", Kind: "builtin", Enabled: true, TimeoutSec: 120, RequiresApproval: true, PermittedActions: []string{"execute", "git"}, RiskLevel: "high"},
			{ID: "job-output", Name: "Job Output", Description: "Fetch accumulated output of a background shell job.", Kind: "builtin", Enabled: true, TimeoutSec: 10, RequiresApproval: false, PermittedActions: []string{"read"}, RiskLevel: "low"},
			{ID: "job-kill", Name: "Job Kill", Description: "Terminate a background shell job.", Kind: "builtin", Enabled: true, TimeoutSec: 10, RequiresApproval: false, PermittedActions: []string{"execute"}, RiskLevel: "low"},
			{ID: "comment", Name: "Comment", Description: "Emit a progress comment or note.", Kind: "builtin", Enabled: true, TimeoutSec: 5, RequiresApproval: false, PermittedActions: []string{"read"}, RiskLevel: "low"},
			{ID: "ask-user", Name: "Ask User", Description: "Prompt the user for a decision.", Kind: "builtin", Enabled: true, TimeoutSec: 10, RequiresApproval: false, PermittedActions: []string{"ask"}, RiskLevel: "low"},
			{ID: "enter-plan-mode", Name: "Enter Plan Mode", Description: "Switch execution into planning mode.", Kind: "builtin", Enabled: true, TimeoutSec: 5, RequiresApproval: false, PermittedActions: []string{"plan"}, RiskLevel: "low"},
			{ID: "exit-plan-mode", Name: "Exit Plan Mode", Description: "Exit planning mode.", Kind: "builtin", Enabled: true, TimeoutSec: 5, RequiresApproval: false, PermittedActions: []string{"plan"}, RiskLevel: "low"},
			{ID: "web-search", Name: "Web Search", Description: "Search the web and return result links.", Kind: "builtin", Enabled: true, TimeoutSec: 30, RequiresApproval: true, PermittedActions: []string{"search", "network"}, RiskLevel: "medium"},
			{ID: "web-fetch", Name: "Web Fetch", Description: "Fetch a URL and return readable text/markdown content.", Kind: "builtin", Enabled: true, TimeoutSec: 45, RequiresApproval: true, PermittedActions: []string{"read", "network"}, RiskLevel: "medium"},
			{ID: "download", Name: "Download", Description: "Download a URL to a file inside the workspace (size-limited).", Kind: "builtin", Enabled: true, TimeoutSec: 180, RequiresApproval: true, PermittedActions: []string{"write", "network"}, RiskLevel: "high"},
			{ID: "grok-image", Name: "Grok Image", Description: "Generate images via xAI Grok Imagine and save them to the workspace (defaults to assets/ or Next.js public/).", Kind: "builtin", Enabled: true, TimeoutSec: 120, RequiresApproval: false, PermittedActions: []string{"write", "network"}, RiskLevel: "medium"},
			visionToolViewImage,
			{ID: "grok-video", Name: "Grok Video", Description: "Generate videos via xAI Grok Imagine (polled until ready) and save them to the workspace.", Kind: "builtin", Enabled: true, TimeoutSec: 900, RequiresApproval: false, PermittedActions: []string{"write", "network"}, RiskLevel: "medium"},
			{ID: "mcp-list-resources", Name: "MCP List Resources", Description: "List resources exposed by MCP servers.", Kind: "builtin", Enabled: true, TimeoutSec: 30, RequiresApproval: true, PermittedActions: []string{"read", "network"}, RiskLevel: "medium"},
			{ID: "mcp-read-resource", Name: "MCP Read Resource", Description: "Read one MCP resource.", Kind: "builtin", Enabled: true, TimeoutSec: 45, RequiresApproval: true, PermittedActions: []string{"read", "network"}, RiskLevel: "medium"},
			{ID: "mcp-auth", Name: "MCP Auth", Description: "Store MCP server auth credentials.", Kind: "builtin", Enabled: true, TimeoutSec: 20, RequiresApproval: true, PermittedActions: []string{"write", "network"}, RiskLevel: "high"},
			{ID: "enter-worktree", Name: "Enter Worktree", Description: "Create and enter a git worktree.", Kind: "builtin", Enabled: true, TimeoutSec: 120, RequiresApproval: true, PermittedActions: []string{"git", "write"}, RiskLevel: "high"},
			{ID: "exit-worktree", Name: "Exit Worktree", Description: "Remove a git worktree.", Kind: "builtin", Enabled: true, TimeoutSec: 120, RequiresApproval: true, PermittedActions: []string{"git", "write"}, RiskLevel: "high"},
			{ID: "send-message", Name: "Send Message", Description: "Send a structured coordination message.", Kind: "builtin", Enabled: true, TimeoutSec: 10, RequiresApproval: false, PermittedActions: []string{"ask", "plan"}, RiskLevel: "low"},
			{ID: "agent", Name: "Agent", Description: "Spawn a sub-agent to handle a subtask.", Kind: "builtin", Enabled: true, TimeoutSec: 300, RequiresApproval: false, PermittedActions: []string{"read", "write", "execute", "git", "search", "plan", "ask"}, RiskLevel: "medium", PrimaryOnly: true},
			{ID: "skill-read", Name: "Skill Read", Description: "Activate an installed Agent Skill and load its SKILL.md instructions.", Kind: "builtin", Enabled: true, TimeoutSec: 15, RequiresApproval: false, PermittedActions: []string{"read"}, Aliases: []string{"activate-skill", "skill-activate"}, RiskLevel: "low"},
			{ID: "skill-list", Name: "Skill List", Description: "List installed Agent Skills with name + description.", Kind: "builtin", Enabled: true, TimeoutSec: 10, RequiresApproval: false, PermittedActions: []string{"read"}, RiskLevel: "low"},
		},
		Agents: []AgentSpec{
			{ID: "plan", Name: "Plan", Description: "Planning orchestrator (delegates all discovery to explore worker)", Skill: "planning", Mode: "orchestrator", Role: AgentRoleOrchestrator, Color: "blue", AllowedTools: []string{"agent", "tool-search", "task-create", "task-get", "task-update", "task-list", "task-stop", "config", "ask-user", "enter-plan-mode", "exit-plan-mode", "send-message", "todo-write", "comment", "skill-read", "skill-list"}, PermittedActions: []string{"read", "search", "plan", "write"}, Permission: PermissionAskFirst, Enabled: true, Handoffs: []string{"explore", "review", "docs"}, PromptFile: "agents/planning.md"},
			{ID: "coding", Name: "Coding", Description: "Coding orchestrator", Skill: "implementation", Mode: "orchestrator", Role: AgentRolePrimary, Color: "green", AllowedTools: []string{"agent", "glob", "grep", "file-read", "file-write", "file-edit", "multi-edit", "diagnostics", "references", "lsp-restart", "shell-exec", "bash", "job-output", "job-kill", "ls", "tool-search", "task-create", "task-get", "task-update", "task-list", "task-stop", "config", "send-message", "todo-write", "comment", "skill-read", "skill-list", "save-memory", "grok-image", "grok-video", "web-fetch", "download"}, PermittedActions: []string{"read", "search", "plan", "write", "execute", "git", "network"}, Permission: PermissionRestricted, Enabled: true, Handoffs: []string{"code", "git", "test", "review", "docs", "explore"}, PromptFile: "agents/coding.md"},
			{ID: "ask", Name: "Ask", Description: "Read-only orchestrator for Q&A", Skill: "conversation", Mode: "orchestrator", Role: AgentRolePrimary, Color: "cyan", AllowedTools: []string{"agent", "glob", "grep", "file-read", "tool-search", "web-search", "web-fetch", "mcp-list-resources", "mcp-read-resource", "comment", "skill-read", "skill-list", "save-memory"}, PermittedActions: []string{"ask", "read", "search"}, Permission: PermissionAskFirst, Enabled: true, Handoffs: []string{"explore", "docs"}, PromptFile: "agents/chat.md"},
			{ID: "explore", Name: "Explore", Description: "Read-only code exploration worker", Skill: "analysis", Mode: "worker", Role: AgentRoleWorker, Color: "blue", AllowedTools: []string{"glob", "grep", "file-read", "ls", "comment", "skill-read", "skill-list"}, PermittedActions: []string{"read", "search"}, Permission: PermissionAskFirst, Enabled: true, Handoffs: []string{"explore", "review", "docs"}, PromptFile: "agents/explore.md"},
			{ID: "code", Name: "Code", Description: "Implementation worker", Skill: "implementation", Mode: "worker", Role: AgentRoleWorker, Color: "green", AllowedTools: []string{"agent", "glob", "grep", "file-read", "file-write", "file-edit", "multi-edit", "diagnostics", "references", "lsp-restart", "shell-exec", "bash", "job-output", "job-kill", "ls", "task-create", "task-get", "task-update", "task-list", "task-stop", "config", "enter-worktree", "exit-worktree", "comment", "todo-write", "skill-read", "skill-list", "save-memory", "grok-image", "grok-video", "web-fetch", "download"}, PermittedActions: []string{"read", "search", "write", "execute", "git", "network"}, Permission: PermissionRestricted, Enabled: true, Handoffs: []string{"explore", "review", "test", "docs"}, PromptFile: "agents/code.md"},
			{ID: "git", Name: "Git", Description: "Git operations worker", Skill: "git", Mode: "worker", Role: AgentRoleWorker, Color: "yellow", AllowedTools: []string{"glob", "grep", "file-read", "shell-exec", "bash", "job-output", "job-kill", "ls", "comment", "skill-read", "skill-list"}, PermittedActions: []string{"read", "search", "execute", "git"}, Permission: PermissionRestricted, Enabled: true, Handoffs: []string{"review", "docs"}, PromptFile: "agents/git.md"},
			{ID: "test", Name: "Test", Description: "Test execution worker", Skill: "testing", Mode: "worker", Role: AgentRoleWorker, Color: "yellow", AllowedTools: []string{"glob", "grep", "file-read", "shell-exec", "bash", "job-output", "job-kill", "ls", "comment", "skill-read", "skill-list"}, PermittedActions: []string{"read", "search", "execute"}, Permission: PermissionRestricted, Enabled: true, Handoffs: []string{"review", "explore"}, PromptFile: "agents/tester.md"},
			{ID: "review", Name: "Review", Description: "Code review worker", Skill: "review", Mode: "worker", Role: AgentRoleSubagent, Color: "red", AllowedTools: []string{"glob", "grep", "file-read", "shell-exec", "bash", "job-output", "job-kill", "ls", "comment", "skill-read", "skill-list"}, PermittedActions: []string{"read", "search", "execute", "plan"}, Permission: PermissionAskFirst, Enabled: true, Handoffs: []string{"explore", "docs"}, PromptFile: "agents/reviewer.md"},
			{ID: "docs", Name: "Docs", Description: "Read-only documentation worker", Skill: "documentation", Mode: "worker", Role: AgentRoleSubagent, Color: "cyan", AllowedTools: []string{"glob", "grep", "file-read", "comment", "skill-read", "skill-list"}, PermittedActions: []string{"read", "search", "ask"}, Permission: PermissionAskFirst, Enabled: true, Handoffs: []string{"explore"}, PromptFile: "agents/docs-writer.md"},
		},
	}
	_ = m.normalizeFromVersion()
	return m
}

func AgentManifestPath(cwd string) string {
	return filepath.Join(cwd, AgentManifestFilename)
}

func LoadAgentManifestForProject(cwd string) (AgentManifest, error) {
	p := AgentManifestPath(cwd)
	m, originalVersion, changed, err := loadAgentManifestWithMigrationInfo(p)
	if err == nil {
		if changed || originalVersion == 1 {
			if werr := backupAndWriteManifest(p, m); werr != nil {
				return AgentManifest{}, werr
			}
		}
		return m, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return DefaultAgentManifest(), nil
	}
	return AgentManifest{}, err
}

func LoadAgentManifest(path string) (AgentManifest, error) {
	m, _, _, err := loadAgentManifestWithMigrationInfo(path)
	if err != nil {
		return AgentManifest{}, err
	}
	return m, nil
}

func loadAgentManifestWithMigrationInfo(path string) (AgentManifest, int, bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return AgentManifest{}, 0, false, err
	}
	defer f.Close()
	return DecodeAgentManifestWithMigrationInfo(f)
}

func DecodeAgentManifest(r io.Reader) (AgentManifest, error) {
	m, _, _, err := DecodeAgentManifestWithMigrationInfo(r)
	return m, err
}

func DecodeAgentManifestWithMigrationInfo(r io.Reader) (AgentManifest, int, bool, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return AgentManifest{}, 0, false, fmt.Errorf("read agent manifest: %w", err)
	}
	var versionOnly struct {
		Version int `toml:"version"`
	}
	if err := toml.Unmarshal(data, &versionOnly); err != nil {
		return AgentManifest{}, 0, false, fmt.Errorf("decode manifest version: %w", err)
	}
	if versionOnly.Version <= 0 {
		versionOnly.Version = 1
	}

	var manifest AgentManifest
	dec := toml.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&manifest); err != nil {
		return AgentManifest{}, versionOnly.Version, false, fmt.Errorf("decode agent manifest: %w", err)
	}
	originalVersion := manifest.Version
	if originalVersion == 0 {
		originalVersion = versionOnly.Version
	}
	changed := manifest.normalizeFromVersion()
	if err := manifest.Validate(); err != nil {
		return AgentManifest{}, originalVersion, changed, err
	}
	return manifest, originalVersion, changed, nil
}

func backupAndWriteManifest(path string, m AgentManifest) error {
	if _, err := os.Stat(path); err == nil {
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return fmt.Errorf("read manifest before migration backup: %w", rerr)
		}
		backup := fmt.Sprintf("%s.migrated-%s.bak", path, time.Now().UTC().Format("20060102-150405"))
		if werr := os.WriteFile(backup, data, 0o644); werr != nil {
			return fmt.Errorf("write migration backup: %w", werr)
		}
	}
	raw, err := toml.Marshal(m)
	if err != nil {
		return fmt.Errorf("encode migrated manifest: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return fmt.Errorf("write migrated manifest: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("replace migrated manifest: %w", err)
	}
	return nil
}

func (m *AgentManifest) normalizeFromVersion() bool {
	changed := false
	if m.Version <= 0 {
		m.Version = 1
	}
	if m.Runtime.DefaultTimeoutSec <= 0 {
		m.Runtime.DefaultTimeoutSec = 120
		changed = true
	}
	if m.Runtime.SandboxMode == "" {
		m.Runtime.SandboxMode = SandboxFullAccess
		changed = true
	}
	if m.Runtime.Delegation.MaxParallelWorkers <= 0 {
		m.Runtime.Delegation.MaxParallelWorkers = 4
		changed = true
	}
	if m.Runtime.Delegation.MaxDepth <= 0 {
		m.Runtime.Delegation.MaxDepth = 2
		changed = true
	}
	if m.Runtime.Delegation.MaxToolCallsPerStep <= 0 {
		m.Runtime.Delegation.MaxToolCallsPerStep = 32
		changed = true
	}
	for i := range m.Tools {
		if m.Tools[i].RiskLevel == "" {
			m.Tools[i].RiskLevel = "medium"
			changed = true
		}
		if len(m.Tools[i].Aliases) == 0 {
			switch m.Tools[i].ID {
			case "bash":
				m.Tools[i].Aliases = []string{"bash-output"}
				changed = true
			}
		}
	}
	for i := range m.Agents {
		if m.Agents[i].Role == "" {
			switch strings.ToLower(strings.TrimSpace(m.Agents[i].Mode)) {
			case "orchestrator":
				m.Agents[i].Role = AgentRoleOrchestrator
			case "worker":
				m.Agents[i].Role = AgentRoleWorker
			default:
				m.Agents[i].Role = AgentRoleWorker
			}
			changed = true
		}
	}
	if m.Version < 2 {
		m.Version = 2
		changed = true
	}
	if m.Version < 3 {
		// Before v3 the sandbox_mode field was persisted by the tool (never
		// user-chosen) and never enforced. Now that workspace-write activates
		// the OS sandbox, rewrite the inert default so existing projects keep
		// their current (unconfined) behavior; users who want the sandbox
		// re-enable it explicitly.
		if m.Runtime.SandboxMode == SandboxWorkspaceWrite {
			m.Runtime.SandboxMode = SandboxFullAccess
		}
		m.Version = 3
		changed = true
	}
	if m.Version < 5 {
		// v5 introduces the view-image vision tool. Existing manifests get the
		// tool definition, and agents already trusted with workspace reads
		// (file-read) get it allowed.
		m.ensureVisionTools()
		m.Version = 5
		changed = true
	}
	if m.Version < 6 {
		// v6 introduces the deeper LSP tools (hover, rename-symbol). Existing
		// manifests get the definitions; agents already trusted with the
		// references tool get hover, and those also holding file-edit get
		// rename-symbol.
		m.ensureLSPDeepTools()
		m.Version = 6
		changed = true
	}
	if m.Version < 7 {
		// v7 backs repo-search with the symbol index (ranked definitions).
		// Grant it to agents already trusted with grep — same read/search
		// surface — so they can use the cheaper symbol lookup.
		m.ensureRepoSearchTool()
		m.Version = 7
		changed = true
	}
	return changed
}

// ensureRepoSearchTool retrofits the symbol-index-backed repo-search tool
// into a manifest that predates v7: the definition is added when absent, and
// any agent already holding grep gets repo-search allowed (identical
// read/search trust level, so deliberate restrictions are preserved).
func (m *AgentManifest) ensureRepoSearchTool() {
	haveTool := false
	for _, t := range m.Tools {
		if t.ID == "repo-search" {
			haveTool = true
			break
		}
	}
	if !haveTool {
		m.Tools = append(m.Tools, ToolSpec{ID: "repo-search", Name: "Repository Search", Description: "Searches file names and content inside the project.", Kind: "builtin", Enabled: true, TimeoutSec: 30, RequiresApproval: false, PermittedActions: []string{"read", "search"}, RiskLevel: "low"})
	}
	for i := range m.Agents {
		allowed := map[string]bool{}
		for _, id := range m.Agents[i].AllowedTools {
			allowed[id] = true
		}
		if allowed["grep"] && !allowed["repo-search"] {
			m.Agents[i].AllowedTools = append(m.Agents[i].AllowedTools, "repo-search")
		}
	}
}

// ensureLSPDeepTools retrofits hover and rename-symbol into a manifest that
// predates them, mirroring ensureVisionTools: definitions are added when
// absent, and allow-lists grow only for agents whose existing tools show the
// same level of trust (references for hover; references + file-edit for
// rename-symbol) so deliberate restrictions are preserved.
func (m *AgentManifest) ensureLSPDeepTools() {
	have := map[string]bool{}
	for _, t := range m.Tools {
		have[t.ID] = true
	}
	for _, t := range lspDeepTools {
		if !have[t.ID] {
			m.Tools = append(m.Tools, t)
		}
	}
	for i := range m.Agents {
		allowed := map[string]bool{}
		for _, id := range m.Agents[i].AllowedTools {
			allowed[id] = true
		}
		if allowed["references"] && !allowed["hover"] {
			m.Agents[i].AllowedTools = append(m.Agents[i].AllowedTools, "hover")
		}
		if allowed["references"] && allowed["file-edit"] && !allowed["rename-symbol"] {
			m.Agents[i].AllowedTools = append(m.Agents[i].AllowedTools, "rename-symbol")
		}
	}
}

// ensureVisionTools retrofits the view-image tool into a manifest that
// predates it. The definition is added when absent; each agent's allow-list
// grows the tool only when it already holds file-read, so an operator's
// deliberate restrictions are preserved.
func (m *AgentManifest) ensureVisionTools() {
	haveTool := false
	for _, t := range m.Tools {
		if t.ID == visionToolViewImage.ID {
			haveTool = true
			break
		}
	}
	if !haveTool {
		m.Tools = append(m.Tools, visionToolViewImage)
	}
	for i := range m.Agents {
		allowed := map[string]bool{}
		for _, id := range m.Agents[i].AllowedTools {
			allowed[id] = true
		}
		if allowed["file-read"] && !allowed["view-image"] {
			m.Agents[i].AllowedTools = append(m.Agents[i].AllowedTools, "view-image")
		}
	}
}

func (m AgentManifest) Validate() error {
	_ = m.normalizeFromVersion()
	if m.Version <= 0 {
		return fmt.Errorf("agent manifest: version must be > 0")
	}
	if len(m.Tools) == 0 {
		return fmt.Errorf("agent manifest: at least one tool is required")
	}
	if len(m.Agents) == 0 {
		return fmt.Errorf("agent manifest: at least one agent is required")
	}
	if strings.TrimSpace(m.DefaultAgent) == "" {
		return fmt.Errorf("agent manifest: default_agent is required")
	}
	if m.Runtime.DefaultTimeoutSec <= 0 {
		return fmt.Errorf("agent manifest: runtime.default_timeout_sec must be > 0")
	}
	if err := validatePermissionLevel(m.Runtime.DefaultPermission); err != nil {
		return fmt.Errorf("agent manifest: invalid runtime.default_permission: %w", err)
	}
	if err := validateSandboxMode(m.Runtime.SandboxMode); err != nil {
		return fmt.Errorf("agent manifest: invalid runtime.sandbox_mode: %w", err)
	}
	if strings.TrimSpace(m.Runtime.SandboxNet) != "" {
		if _, _, err := sandbox.ParseNetSpec(m.Runtime.SandboxNet); err != nil {
			return fmt.Errorf("agent manifest: invalid runtime.sandbox_net: %w", err)
		}
	}
	if m.Runtime.Delegation.MaxParallelWorkers <= 0 {
		return fmt.Errorf("agent manifest: runtime.delegation.max_parallel_workers must be > 0")
	}
	if m.Runtime.Delegation.MaxDepth <= 0 {
		return fmt.Errorf("agent manifest: runtime.delegation.max_depth must be > 0")
	}
	if m.Runtime.Delegation.MaxToolCallsPerStep <= 0 {
		return fmt.Errorf("agent manifest: runtime.delegation.max_tool_calls_per_step must be > 0")
	}
	if err := validatePermissionRules(m.Runtime.PermissionRules, "runtime.permission_rules"); err != nil {
		return err
	}
	if err := validateFallbackPolicy(m.Runtime.Fallback); err != nil {
		return err
	}

	toolIDs := map[string]struct{}{}
	for _, tool := range m.Tools {
		id := strings.TrimSpace(tool.ID)
		if id == "" {
			return fmt.Errorf("agent manifest: tool id is required")
		}
		if _, exists := toolIDs[id]; exists {
			return fmt.Errorf("agent manifest: duplicate tool id %q", id)
		}
		toolIDs[id] = struct{}{}
		if strings.TrimSpace(tool.Name) == "" {
			return fmt.Errorf("agent manifest: tool %q name is required", id)
		}
		switch tool.Kind {
		case "builtin", "mcp", "script", "http":
		default:
			return fmt.Errorf("agent manifest: tool %q has unsupported kind %q", id, tool.Kind)
		}
		if tool.TimeoutSec <= 0 {
			return fmt.Errorf("agent manifest: tool %q timeout_sec must be > 0", id)
		}
		if len(tool.PermittedActions) == 0 {
			return fmt.Errorf("agent manifest: tool %q must declare permitted_actions", id)
		}
		if (tool.Kind == "script" || tool.Kind == "http" || tool.Kind == "mcp") && strings.TrimSpace(tool.EntryPoint) == "" {
			return fmt.Errorf("agent manifest: tool %q requires entry_point for kind %q", id, tool.Kind)
		}
		for _, alias := range tool.Aliases {
			if strings.TrimSpace(alias) == "" {
				return fmt.Errorf("agent manifest: tool %q aliases cannot be blank", id)
			}
		}
		if err := validateRiskLevel(tool.RiskLevel); err != nil {
			return fmt.Errorf("agent manifest: invalid risk_level for tool %q: %w", id, err)
		}
		if err := validatePermissionRules(tool.PermissionRules, fmt.Sprintf("tool %q permission_rules", id)); err != nil {
			return err
		}
	}

	agentIDs := map[string]struct{}{}
	for _, ag := range m.Agents {
		id := strings.TrimSpace(ag.ID)
		if id == "" {
			return fmt.Errorf("agent manifest: agent id is required")
		}
		if _, exists := agentIDs[id]; exists {
			return fmt.Errorf("agent manifest: duplicate agent id %q", id)
		}
		agentIDs[id] = struct{}{}
		if strings.TrimSpace(ag.Name) == "" {
			return fmt.Errorf("agent manifest: agent %q name is required", id)
		}
		if strings.TrimSpace(ag.Mode) == "" {
			return fmt.Errorf("agent manifest: agent %q mode is required", id)
		}
		if len(ag.AllowedTools) == 0 {
			return fmt.Errorf("agent manifest: agent %q must declare allowed_tools", id)
		}
		if err := validatePermissionLevel(ag.Permission); err != nil {
			return fmt.Errorf("agent manifest: invalid permission for agent %q: %w", id, err)
		}
		if err := validateAgentRole(ag.Role); err != nil {
			return fmt.Errorf("agent manifest: invalid role for agent %q: %w", id, err)
		}
		if strings.TrimSpace(ag.Color) != "" {
			if err := validateAgentColor(ag.Color); err != nil {
				return fmt.Errorf("agent manifest: invalid color for agent %q: %w", id, err)
			}
		}
		for _, toolID := range ag.AllowedTools {
			if _, exists := toolIDs[toolID]; !exists {
				return fmt.Errorf("agent manifest: agent %q references unknown tool %q", id, toolID)
			}
		}
		if err := validatePermissionRules(ag.PermissionRules, fmt.Sprintf("agent %q permission_rules", id)); err != nil {
			return err
		}
	}
	if _, exists := agentIDs[m.DefaultAgent]; !exists {
		return fmt.Errorf("agent manifest: default_agent %q not found in agents", m.DefaultAgent)
	}
	for _, ag := range m.Agents {
		for _, handoff := range ag.Handoffs {
			if _, exists := agentIDs[handoff]; !exists {
				return fmt.Errorf("agent manifest: agent %q handoff references unknown agent %q", ag.ID, handoff)
			}
		}
	}
	return nil
}

func validatePermissionLevel(level PermissionLevel) error {
	switch level {
	case PermissionYOLO, PermissionRestricted, PermissionAskFirst:
		return nil
	default:
		return fmt.Errorf("unsupported permission level %q", level)
	}
}

func validateSandboxMode(mode SandboxMode) error {
	switch mode {
	case SandboxWorkspaceWrite, SandboxReadOnly, SandboxFullAccess, SandboxOff:
		return nil
	default:
		return fmt.Errorf("unsupported sandbox mode %q", mode)
	}
}

func validateRiskLevel(level string) error {
	switch strings.TrimSpace(level) {
	case "", "low", "medium", "high":
		return nil
	default:
		return fmt.Errorf("unsupported risk level %q; must be low, medium, or high", level)
	}
}

func validateAgentRole(role AgentRole) error {
	switch role {
	case AgentRolePrimary, AgentRoleSubagent, AgentRoleOrchestrator, AgentRoleWorker:
		return nil
	default:
		return fmt.Errorf("unsupported role %q; must be primary, subagent, orchestrator, or worker", role)
	}
}

func validateFallbackPolicy(p FallbackPolicy) error {
	switch p.Mode {
	case "", FallbackPrompt, FallbackSilent, FallbackOff:
	default:
		return fmt.Errorf("agent manifest: runtime.fallback.mode must be prompt, silent, or off")
	}
	for i, ref := range p.Chain {
		if err := validateModelRef(ref); err != nil {
			return fmt.Errorf("agent manifest: runtime.fallback.chain[%d]: %w", i, err)
		}
	}
	if strings.TrimSpace(p.InternalModel) != "" {
		if err := validateModelRef(p.InternalModel); err != nil {
			return fmt.Errorf("agent manifest: runtime.fallback.internal_model: %w", err)
		}
	}
	return nil
}

// validateModelRef checks the "provider/model" shape used by fallback config.
func validateModelRef(ref string) error {
	ref = strings.TrimSpace(ref)
	i := strings.Index(ref, "/")
	if i <= 0 || i == len(ref)-1 {
		return fmt.Errorf("model ref %q must be provider/model", ref)
	}
	return nil
}

func validatePermissionRules(rules []PermissionRule, field string) error {
	for i, r := range rules {
		if strings.TrimSpace(r.Permission) == "" {
			return fmt.Errorf("agent manifest: %s[%d].permission is required", field, i)
		}
		if strings.TrimSpace(r.Pattern) == "" {
			return fmt.Errorf("agent manifest: %s[%d].pattern is required", field, i)
		}
		switch r.Action {
		case RuleAllow, RuleAsk, RuleDeny:
		default:
			return fmt.Errorf("agent manifest: %s[%d].action must be allow, ask, or deny", field, i)
		}
	}
	return nil
}

var validAgentColors = map[string]struct{}{
	"blue":    {},
	"green":   {},
	"cyan":    {},
	"yellow":  {},
	"magenta": {},
	"red":     {},
	"white":   {},
	"purple":  {},
}

func validateAgentColor(color string) error {
	if _, ok := validAgentColors[color]; !ok {
		return fmt.Errorf("unsupported color %q; must be one of: blue, green, cyan, yellow, magenta, red, white, purple", color)
	}
	return nil
}

func (m AgentManifest) AgentByID(id string) (AgentSpec, bool) {
	for _, a := range m.Agents {
		if a.ID == id {
			return a, true
		}
	}
	return AgentSpec{}, false
}

func (m AgentManifest) EnabledAgents() []AgentSpec {
	out := make([]AgentSpec, 0, len(m.Agents))
	for _, a := range m.Agents {
		if a.Enabled {
			out = append(out, a)
		}
	}
	return out
}

func (m AgentManifest) EnabledToolsForAgent(agentID string) []ToolSpec {
	agent, ok := m.AgentByID(agentID)
	if !ok {
		return nil
	}
	allowed := map[string]struct{}{}
	for _, id := range agent.AllowedTools {
		allowed[id] = struct{}{}
	}
	out := make([]ToolSpec, 0)
	for _, t := range m.Tools {
		if !t.Enabled {
			continue
		}
		if _, ok := allowed[t.ID]; ok {
			out = append(out, t)
		}
	}
	slices.SortFunc(out, func(a, b ToolSpec) int {
		return strings.Compare(a.ID, b.ID)
	})
	return out
}

func (m AgentManifest) ToolByID(id string) (ToolSpec, bool) {
	for _, t := range m.Tools {
		if t.ID == id {
			return t, true
		}
		for _, alias := range t.Aliases {
			if alias == id {
				return t, true
			}
		}
	}
	return ToolSpec{}, false
}

func (a AgentSpec) IsPrimaryRole() bool {
	return a.Role == AgentRolePrimary || a.Role == AgentRoleOrchestrator
}
