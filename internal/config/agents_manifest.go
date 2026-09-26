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

	"spettro/internal/safeio"
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
	// SearchChars is the max chars returned by grep/glob and kept in history.
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

// ptyTools are shared between the default manifest and the v8 migration that
// retrofits them into existing manifests. pty-start carries the same approval
// and risk surface as shell-exec; one start approval covers subsequent
// pty-write input into that session, so pty-write itself needs no approval.
var ptyTools = []ToolSpec{
	{ID: "pty-start", Name: "PTY Start", Description: "Start an interactive terminal session under a pseudo-terminal.", Kind: "builtin", Enabled: true, TimeoutSec: 60, RequiresApproval: true, PermittedActions: []string{"execute", "git"}, RiskLevel: "high"},
	{ID: "pty-write", Name: "PTY Write", Description: "Send input to a pty session and read new output.", Kind: "builtin", Enabled: true, TimeoutSec: 60, RequiresApproval: false, PermittedActions: []string{"execute"}, RiskLevel: "high"},
	{ID: "pty-kill", Name: "PTY Kill", Description: "Terminate a pty session.", Kind: "builtin", Enabled: true, TimeoutSec: 15, RequiresApproval: false, PermittedActions: []string{"execute"}, RiskLevel: "low"},
}

// toolOutputSpec is shared between the default manifest and the v9 migration
// that retrofits it into existing manifests. It reads back tool outputs
// offloaded to the session spool (execution-time truncation and compaction
// reference stubs point at it), a read-only surface granted alongside
// file-read.
var toolOutputSpec = ToolSpec{ID: "tool-output", Name: "Tool Output", Description: "Re-read the full output of an earlier tool call that was offloaded to the session spool.", Kind: "builtin", Enabled: true, TimeoutSec: 10, RequiresApproval: false, PermittedActions: []string{"read"}, RiskLevel: "low"}

var visionToolViewImage = ToolSpec{ID: "view-image", Name: "View Image", Description: "Attach an image file from the workspace so the model can see it (vision models only).", Kind: "builtin", Enabled: true, TimeoutSec: 15, RequiresApproval: false, PermittedActions: []string{"read"}, RiskLevel: "low"}

// askUserToolSpec is shared between the default manifest and the v10 migration
// that retrofits the grant into existing manifests. It is the only way an
// agent can put a question to the human driving it, so it is granted to the
// agents a person talks to directly (primary/orchestrator roles) and withheld
// from workers and subagents, whose runs inherit the parent's callback and
// would interrupt the user mid-orchestration with no context about who asked.
var askUserToolSpec = ToolSpec{ID: "ask-user", Name: "Ask User", Description: "Prompt the user for a decision.", Kind: "builtin", Enabled: true, TimeoutSec: 10, RequiresApproval: false, PermittedActions: []string{"ask"}, RiskLevel: "low"}

// generalPurposeAgentSpec is shared between the default manifest and the v11
// migration that retrofits it into existing manifests. It is the fallback
// delegation target: the specialists each cover one slice (explore reads,
// test runs verification, docs summarizes), so an open-ended subtask that
// spans several of them had nowhere to go and had to be split by hand. Its
// tool list is spelled out in full rather than relying on the v5-v10
// retrofits, which run before this one and would skip an agent that does not
// exist yet. It names the canonical tools (v12's, and v13's lsp); a manifest
// older than those gets the retired name it holds instead
// (ensureGeneralPurposeAgent), and v12 or v13 then folds it.
//
// Role is subagent so both orchestrators and workers can hand off to it, and
// the agent tool (primary_only) stays out of reach: it does the work itself
// instead of fanning out further.
var generalPurposeAgentSpec = AgentSpec{
	ID:          "general-purpose",
	Name:        "General Purpose",
	Description: "General-purpose worker for open-ended research, multi-step search, and tasks that no specialist covers",
	Skill:       "general",
	Mode:        "worker",
	Role:        AgentRoleSubagent,
	Color:       "magenta",
	AllowedTools: []string{
		"glob", "grep", "file-read", "file-write", "file-edit",
		"lsp", "rename-symbol",
		"bash", "job-output", "job-kill",
		"pty-start", "pty-write", "pty-kill",
		"web-search", "web-fetch", "download",
		"todo-write", "comment", "skill-read", "skill-list", "view-image", "tool-output",
	},
	PermittedActions: []string{"read", "search", "write", "execute", "git", "network"},
	Permission:       PermissionRestricted,
	Temperature:      0.1,
	MaxTokens:        8192,
	Enabled:          true,
	// No handoffs: the agent tool is primary_only, so a subagent cannot
	// delegate anyway — it finishes the task itself and reports back.
	Handoffs:   nil,
	PromptFile: "agents/general-purpose.md",
}

// commentToolSpec is shared between the default manifest and the v12
// migration, which gives an agent left with no tools just this one.
var commentToolSpec = ToolSpec{ID: "comment", Name: "Comment", Description: "Emit a progress comment or note.", Kind: "builtin", Enabled: true, TimeoutSec: 5, RequiresApproval: false, PermittedActions: []string{"read"}, RiskLevel: "low"}

// defaultToolSpecs is the default manifest's tool registry before the
// version upgrades DefaultAgentManifest runs (which append the v6 and v8
// tools). Each tool that absorbed others in v12 or v13 lists the retired
// names as aliases, exactly as the migration leaves them.
func defaultToolSpecs() []ToolSpec {
	return []ToolSpec{
		{ID: "glob", Name: "Glob", Description: "Find files by name pattern, or list one directory.", Kind: "builtin", Enabled: true, TimeoutSec: 30, RequiresApproval: false, PermittedActions: []string{"read", "search"}, Aliases: retiredToolNames("glob"), RiskLevel: "low"},
		{ID: "grep", Name: "Grep", Description: "Search file contents with regex, or look up a symbol's definitions and usages.", Kind: "builtin", Enabled: true, TimeoutSec: 30, RequiresApproval: false, PermittedActions: []string{"read", "search"}, Aliases: retiredToolNames("grep"), RiskLevel: "low"},
		{ID: "file-read", Name: "File Reader", Description: "Reads file contents in the workspace.", Kind: "builtin", Enabled: true, TimeoutSec: 30, RequiresApproval: false, PermittedActions: []string{"read"}, RiskLevel: "low"},
		{ID: "file-write", Name: "File Writer", Description: "Creates and edits files in the workspace.", Kind: "builtin", Enabled: true, TimeoutSec: 60, RequiresApproval: true, PermittedActions: []string{"write"}, RiskLevel: "high"},
		{ID: "file-edit", Name: "File Edit", Description: "Apply targeted edits to existing files.", Kind: "builtin", Enabled: true, TimeoutSec: 60, RequiresApproval: true, PermittedActions: []string{"write"}, Aliases: retiredToolNames("file-edit"), RiskLevel: "high"},
		{ID: "lsp", Name: "LSP", Description: "Query the language server: diagnostics, references, definition, hover, or restart a server.", Kind: "builtin", Enabled: true, TimeoutSec: 30, RequiresApproval: false, PermittedActions: []string{"read", "search"}, Aliases: retiredToolNames("lsp"), RiskLevel: "low"},
		{ID: "tool-search", Name: "Tool Search", Description: "Search available tools for current agent.", Kind: "builtin", Enabled: true, TimeoutSec: 20, RequiresApproval: false, PermittedActions: []string{"read", "search"}, RiskLevel: "low"},
		{ID: "save-memory", Name: "Save Memory", Description: "Save one short durable fact or preference to persistent cross-session memory.", Kind: "builtin", Enabled: true, TimeoutSec: 10, RequiresApproval: false, PermittedActions: []string{"write", "ask"}, RiskLevel: "low"},
		{ID: "todo-write", Name: "Todo Write", Description: "Read and update the session task list.", Kind: "builtin", Enabled: true, TimeoutSec: 10, RequiresApproval: false, PermittedActions: []string{"write"}, Aliases: retiredToolNames("todo-write"), RiskLevel: "medium"},
		{ID: "task-stop", Name: "Task Stop", Description: "Request stopping the current agent run.", Kind: "builtin", Enabled: true, TimeoutSec: 5, RequiresApproval: false, PermittedActions: []string{"plan", "ask"}, RiskLevel: "low"},
		{ID: "goal-complete", Name: "Goal Complete", Description: "Declare the goal fully achieved and verified; ends the run. Only call after you have confirmed the objective is met (tests pass / build green / change applied).", Kind: "builtin", Enabled: true, TimeoutSec: 5, RequiresApproval: false, PermittedActions: []string{"plan", "ask"}, RiskLevel: "low"},
		{ID: "config", Name: "Config", Description: "Read or update selected runtime settings.", Kind: "builtin", Enabled: true, TimeoutSec: 10, RequiresApproval: false, PermittedActions: []string{"read", "write", "plan"}, RiskLevel: "medium"},
		{ID: "bash", Name: "Bash", Description: "Execute a command with the host shell and return its output.", Kind: "builtin", Enabled: true, TimeoutSec: 120, RequiresApproval: true, PermittedActions: []string{"execute", "git"}, Aliases: retiredToolNames("bash"), RiskLevel: "high"},
		{ID: "job-output", Name: "Job Output", Description: "Fetch accumulated output of a background shell job.", Kind: "builtin", Enabled: true, TimeoutSec: 10, RequiresApproval: false, PermittedActions: []string{"read"}, RiskLevel: "low"},
		{ID: "job-kill", Name: "Job Kill", Description: "Terminate a background shell job.", Kind: "builtin", Enabled: true, TimeoutSec: 10, RequiresApproval: false, PermittedActions: []string{"execute"}, RiskLevel: "low"},
		toolOutputSpec,
		commentToolSpec,
		askUserToolSpec,
		{ID: "enter-plan-mode", Name: "Enter Plan Mode", Description: "Switch execution into planning mode.", Kind: "builtin", Enabled: true, TimeoutSec: 5, RequiresApproval: false, PermittedActions: []string{"plan"}, RiskLevel: "low"},
		{ID: "exit-plan-mode", Name: "Exit Plan Mode", Description: "Exit planning mode.", Kind: "builtin", Enabled: true, TimeoutSec: 5, RequiresApproval: false, PermittedActions: []string{"plan"}, RiskLevel: "low"},
		{ID: "web-search", Name: "Web Search", Description: "Search the web and return result links.", Kind: "builtin", Enabled: true, TimeoutSec: 30, RequiresApproval: true, PermittedActions: []string{"search", "network"}, RiskLevel: "medium"},
		{ID: "web-fetch", Name: "Web Fetch", Description: "Fetch a URL and return readable text/markdown content.", Kind: "builtin", Enabled: true, TimeoutSec: 45, RequiresApproval: true, PermittedActions: []string{"read", "network"}, RiskLevel: "medium"},
		{ID: "download", Name: "Download", Description: "Download a URL to a file inside the workspace (size-limited).", Kind: "builtin", Enabled: true, TimeoutSec: 180, RequiresApproval: true, PermittedActions: []string{"write", "network"}, RiskLevel: "high"},
		visionToolViewImage,
		{ID: "mcp-list-resources", Name: "MCP List Resources", Description: "List resources exposed by MCP servers.", Kind: "builtin", Enabled: true, TimeoutSec: 30, RequiresApproval: true, PermittedActions: []string{"read", "network"}, RiskLevel: "medium"},
		{ID: "mcp-read-resource", Name: "MCP Read Resource", Description: "Read one MCP resource.", Kind: "builtin", Enabled: true, TimeoutSec: 45, RequiresApproval: true, PermittedActions: []string{"read", "network"}, RiskLevel: "medium"},
		{ID: "mcp-auth", Name: "MCP Auth", Description: "Store MCP server auth credentials.", Kind: "builtin", Enabled: true, TimeoutSec: 20, RequiresApproval: true, PermittedActions: []string{"write", "network"}, RiskLevel: "high"},
		{ID: "enter-worktree", Name: "Enter Worktree", Description: "Create and enter a git worktree.", Kind: "builtin", Enabled: true, TimeoutSec: 120, RequiresApproval: true, PermittedActions: []string{"git", "write"}, RiskLevel: "high"},
		{ID: "exit-worktree", Name: "Exit Worktree", Description: "Remove a git worktree.", Kind: "builtin", Enabled: true, TimeoutSec: 120, RequiresApproval: true, PermittedActions: []string{"git", "write"}, RiskLevel: "high"},
		{ID: "send-message", Name: "Send Message", Description: "Send a structured coordination message.", Kind: "builtin", Enabled: true, TimeoutSec: 10, RequiresApproval: false, PermittedActions: []string{"ask", "plan"}, RiskLevel: "low"},
		{ID: "agent", Name: "Agent", Description: "Spawn a sub-agent to handle a subtask.", Kind: "builtin", Enabled: true, TimeoutSec: 300, RequiresApproval: false, PermittedActions: []string{"read", "write", "execute", "git", "search", "plan", "ask"}, RiskLevel: "medium", PrimaryOnly: true},
		{ID: "skill-read", Name: "Skill Read", Description: "Activate an installed Agent Skill and load its SKILL.md instructions.", Kind: "builtin", Enabled: true, TimeoutSec: 15, RequiresApproval: false, PermittedActions: []string{"read"}, Aliases: []string{"activate-skill", "skill-activate"}, RiskLevel: "low"},
		{ID: "skill-list", Name: "Skill List", Description: "List installed Agent Skills with name + description.", Kind: "builtin", Enabled: true, TimeoutSec: 10, RequiresApproval: false, PermittedActions: []string{"read"}, RiskLevel: "low"},
	}
}

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
		Tools: defaultToolSpecs(),
		Agents: []AgentSpec{
			{ID: "plan", Name: "Plan", Description: "Planning orchestrator (delegates all discovery to explore worker)", Skill: "planning", Mode: "orchestrator", Role: AgentRoleOrchestrator, Color: "blue", AllowedTools: []string{"agent", "tool-search", "todo-write", "task-stop", "config", "ask-user", "enter-plan-mode", "exit-plan-mode", "send-message", "comment", "skill-read", "skill-list"}, PermittedActions: []string{"read", "search", "plan", "write", "ask"}, Permission: PermissionAskFirst, Enabled: true, Handoffs: []string{"explore", "review", "docs", "general-purpose"}, PromptFile: "agents/planning.md"},
			{ID: "coding", Name: "Coding", Description: "Coding orchestrator", Skill: "implementation", Mode: "orchestrator", Role: AgentRolePrimary, Color: "green", AllowedTools: []string{"agent", "glob", "grep", "file-read", "file-write", "file-edit", "lsp", "bash", "job-output", "job-kill", "tool-search", "todo-write", "task-stop", "config", "ask-user", "send-message", "comment", "skill-read", "skill-list", "save-memory", "web-fetch", "download"}, PermittedActions: []string{"read", "search", "plan", "write", "execute", "git", "network", "ask"}, Permission: PermissionRestricted, Enabled: true, Handoffs: []string{"code", "git", "test", "review", "docs", "explore", "general-purpose"}, PromptFile: "agents/coding.md"},
			{ID: "ask", Name: "Ask", Description: "Read-only orchestrator for Q&A", Skill: "conversation", Mode: "orchestrator", Role: AgentRolePrimary, Color: "cyan", AllowedTools: []string{"agent", "glob", "grep", "file-read", "tool-search", "web-search", "web-fetch", "mcp-list-resources", "mcp-read-resource", "ask-user", "comment", "skill-read", "skill-list", "save-memory"}, PermittedActions: []string{"ask", "read", "search"}, Permission: PermissionAskFirst, Enabled: true, Handoffs: []string{"explore", "docs", "general-purpose"}, PromptFile: "agents/chat.md"},
			{ID: "explore", Name: "Explore", Description: "Read-only code exploration worker", Skill: "analysis", Mode: "worker", Role: AgentRoleWorker, Color: "blue", AllowedTools: []string{"glob", "grep", "file-read", "comment", "skill-read", "skill-list"}, PermittedActions: []string{"read", "search"}, Permission: PermissionAskFirst, Enabled: true, Handoffs: []string{"explore", "review", "docs"}, PromptFile: "agents/explore.md"},
			{ID: "code", Name: "Code", Description: "Implementation worker", Skill: "implementation", Mode: "worker", Role: AgentRoleWorker, Color: "green", AllowedTools: []string{"agent", "glob", "grep", "file-read", "file-write", "file-edit", "lsp", "bash", "job-output", "job-kill", "todo-write", "task-stop", "config", "enter-worktree", "exit-worktree", "comment", "skill-read", "skill-list", "save-memory", "web-fetch", "download"}, PermittedActions: []string{"read", "search", "write", "execute", "git", "network"}, Permission: PermissionRestricted, Enabled: true, Handoffs: []string{"explore", "review", "test", "docs"}, PromptFile: "agents/code.md"},
			{ID: "git", Name: "Git", Description: "Git operations worker", Skill: "git", Mode: "worker", Role: AgentRoleWorker, Color: "yellow", AllowedTools: []string{"glob", "grep", "file-read", "bash", "job-output", "job-kill", "comment", "skill-read", "skill-list"}, PermittedActions: []string{"read", "search", "execute", "git"}, Permission: PermissionRestricted, Enabled: true, Handoffs: []string{"review", "docs"}, PromptFile: "agents/git.md"},
			{ID: "test", Name: "Test", Description: "Test execution worker", Skill: "testing", Mode: "worker", Role: AgentRoleWorker, Color: "yellow", AllowedTools: []string{"glob", "grep", "file-read", "bash", "job-output", "job-kill", "comment", "skill-read", "skill-list"}, PermittedActions: []string{"read", "search", "execute"}, Permission: PermissionRestricted, Enabled: true, Handoffs: []string{"review", "explore"}, PromptFile: "agents/tester.md"},
			{ID: "review", Name: "Review", Description: "Code review worker", Skill: "review", Mode: "worker", Role: AgentRoleSubagent, Color: "red", AllowedTools: []string{"glob", "grep", "file-read", "bash", "job-output", "job-kill", "comment", "skill-read", "skill-list"}, PermittedActions: []string{"read", "search", "execute", "plan"}, Permission: PermissionAskFirst, Enabled: true, Handoffs: []string{"explore", "docs"}, PromptFile: "agents/reviewer.md"},
			{ID: "docs", Name: "Docs", Description: "Read-only documentation worker", Skill: "documentation", Mode: "worker", Role: AgentRoleSubagent, Color: "cyan", AllowedTools: []string{"glob", "grep", "file-read", "comment", "skill-read", "skill-list"}, PermittedActions: []string{"read", "search", "ask"}, Permission: PermissionAskFirst, Enabled: true, Handoffs: []string{"explore"}, PromptFile: "agents/docs-writer.md"},
			generalPurposeAgentSpec,
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
	if err := safeio.Replace(tmp, path); err != nil {
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
		// rename-symbol. (v13 folds references and hover into lsp; an agent
		// holding lsp already has hover and counts as trusted with
		// references.)
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
	if m.Version < 8 {
		// v8 introduces interactive PTY sessions (pty-start/write/kill).
		// Agents already trusted with shell-exec get them: the surface is the
		// same arbitrary-command trust level, approved through the same path.
		m.ensurePTYTools()
		m.Version = 8
		changed = true
	}
	if m.Version < 9 {
		// v9 introduces the tool-output spool reader used by reference-based
		// compaction. Agents already trusted with file-read get it: same
		// read-only surface (it only reads back outputs their own tools
		// produced).
		m.ensureToolOutputTool()
		m.Version = 9
		changed = true
	}
	if m.Version < 10 {
		// v10 makes ask-user reachable: the tool existed but no shipped agent
		// could actually call it (the allow-list grant was missing, and the
		// "ask" action family was missing from the agents that had it), so a
		// model had no way to put a question to the user at all.
		m.ensureAskUserTool()
		m.Version = 10
		changed = true
	}
	if m.Version < 11 {
		// v11 adds the general-purpose subagent. Every shipped worker is a
		// specialist, so an open-ended subtask ("figure out how X works, then
		// fix it") had no single delegation target and had to be split by
		// hand across explore/code/test.
		m.ensureGeneralPurposeAgent()
		m.Version = 11
		changed = true
	}
	if m.Version < 12 {
		// v12 folds duplicate built-ins into one canonical tool each (bash,
		// file-edit, grep, glob, todo-write; the old names stay callable as
		// hidden aliases) and removes the grok image/video generators.
		m.consolidateBuiltinTools(consolidatedTools, removedTools)
		m.Version = 12
		changed = true
	}
	if m.Version < 13 {
		// v13 folds the read-only language-server tools (diagnostics,
		// references, hover, lsp-restart) into one lsp tool with an op
		// argument; the old names stay callable as hidden aliases.
		// rename-symbol, which writes, stays separate.
		m.consolidateBuiltinTools(lspConsolidatedTools, nil)
		m.Version = 13
		changed = true
	}
	return changed
}

// ensureGeneralPurposeAgent retrofits the general-purpose subagent into a
// manifest that predates v11. The agent is added when absent, and the handoff
// goes to primary/orchestrator agents that already delegate at all (a
// non-empty handoffs list): an operator who stripped an agent's handoffs
// disabled its delegation deliberately, and a manifest whose agent was
// removed by hand does not get it back.
//
// Tools the manifest is missing entirely are dropped from the allow-list,
// since Validate rejects an agent referencing an unknown tool.
func (m *AgentManifest) ensureGeneralPurposeAgent() {
	for _, a := range m.Agents {
		if a.ID == generalPurposeAgentSpec.ID {
			return
		}
	}
	known := map[string]bool{}
	builtin := map[string]bool{}
	for _, t := range m.Tools {
		known[t.ID] = true
		builtin[t.ID] = t.Kind == "builtin"
	}
	spec := generalPurposeAgentSpec
	tools := make([]string, 0, len(spec.AllowedTools))
	for _, id := range spec.AllowedTools {
		if known[id] {
			tools = append(tools, id)
			continue
		}
		// A pre-v12 manifest may hold the tool under a name v12 or v13
		// retires; grant that one and let the migration fold it.
		for _, old := range retiredToolNames(id) {
			if builtin[old] && !readOnlyRetiredTools[old] && !slices.Contains(tools, old) {
				tools = append(tools, old)
				break
			}
		}
	}
	spec.AllowedTools = tools
	m.Agents = append(m.Agents, spec)
	for i := range m.Agents {
		if !m.Agents[i].IsPrimaryRole() || len(m.Agents[i].Handoffs) == 0 {
			continue
		}
		if !slices.Contains(m.Agents[i].Handoffs, spec.ID) {
			m.Agents[i].Handoffs = append(m.Agents[i].Handoffs, spec.ID)
		}
	}
}

// ensureAskUserTool retrofits the ask-user grant into a manifest that predates
// v10. The definition is added when absent, and the grant goes to
// primary/orchestrator agents only — the ones a human converses with directly.
// Workers and subagents are left alone: their runs inherit the parent's
// callback, so a question from a nested worker would interrupt the user
// mid-orchestration with no context about who is asking.
//
// Reachability needs both halves: the tool ID on the agent's allow-list AND
// the "ask" action family on its permitted_actions, since resolveToolPolicies
// drops an allow-listed tool whose actions the agent does not hold.
func (m *AgentManifest) ensureAskUserTool() {
	haveTool := false
	for _, t := range m.Tools {
		if t.ID == askUserToolSpec.ID {
			haveTool = true
			break
		}
	}
	if !haveTool {
		m.Tools = append(m.Tools, askUserToolSpec)
	}
	for i := range m.Agents {
		if !m.Agents[i].IsPrimaryRole() {
			continue
		}
		if !slices.Contains(m.Agents[i].AllowedTools, askUserToolSpec.ID) {
			m.Agents[i].AllowedTools = append(m.Agents[i].AllowedTools, askUserToolSpec.ID)
		}
		// An empty permitted_actions list means "no action filtering at all";
		// appending to it would narrow the agent to the ask family alone.
		if len(m.Agents[i].PermittedActions) > 0 && !slices.Contains(m.Agents[i].PermittedActions, "ask") {
			m.Agents[i].PermittedActions = append(m.Agents[i].PermittedActions, "ask")
		}
	}
}

// ensureToolOutputTool retrofits the tool-output spool reader into a manifest
// that predates v9: the definition is added when absent, and any agent
// already holding file-read gets it allowed (identical read trust level, so
// deliberate restrictions are preserved).
func (m *AgentManifest) ensureToolOutputTool() {
	haveTool := false
	for _, t := range m.Tools {
		if t.ID == toolOutputSpec.ID {
			haveTool = true
			break
		}
	}
	if !haveTool {
		m.Tools = append(m.Tools, toolOutputSpec)
	}
	for i := range m.Agents {
		allowed := map[string]bool{}
		for _, id := range m.Agents[i].AllowedTools {
			allowed[id] = true
		}
		if allowed["file-read"] && !allowed["tool-output"] {
			m.Agents[i].AllowedTools = append(m.Agents[i].AllowedTools, "tool-output")
		}
	}
}

// ensurePTYTools retrofits the pty session tools into a manifest that
// predates v8: definitions are added when absent, and any agent already
// holding a shell (shell-exec, or bash, its v12 name) gets all three
// (identical execute trust level, so deliberate restrictions are preserved).
func (m *AgentManifest) ensurePTYTools() {
	have := map[string]bool{}
	for _, t := range m.Tools {
		have[t.ID] = true
	}
	for _, t := range ptyTools {
		if !have[t.ID] {
			m.Tools = append(m.Tools, t)
		}
	}
	for i := range m.Agents {
		allowed := map[string]bool{}
		for _, id := range m.Agents[i].AllowedTools {
			allowed[id] = true
		}
		if !allowed["shell-exec"] && !allowed["bash"] {
			continue
		}
		for _, t := range ptyTools {
			if !allowed[t.ID] {
				m.Agents[i].AllowedTools = append(m.Agents[i].AllowedTools, t.ID)
			}
		}
	}
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
//
// The default manifest runs this too, and it already has v13's lsp tool,
// which answers to hover (an alias) and does what references did: no hover
// definition is added next to it, and an agent holding lsp counts as holding
// references for rename-symbol.
func (m *AgentManifest) ensureLSPDeepTools() {
	for _, t := range lspDeepTools {
		if _, ok := m.toolNamed(t.ID); !ok {
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
		if (allowed["references"] || allowed["lsp"]) && allowed["file-edit"] && !allowed["rename-symbol"] {
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
		toolIDs[strings.TrimSpace(tool.ID)] = struct{}{}
	}
	aliasOwner := map[string]string{}
	for _, tool := range m.Tools {
		for _, alias := range tool.Aliases {
			alias = strings.TrimSpace(alias)
			if alias == "" {
				continue // reported below with the tool's other checks
			}
			if _, clash := toolIDs[alias]; clash {
				return fmt.Errorf("agent manifest: tool %q alias %q is another tool's id", tool.ID, alias)
			}
			if owner, dup := aliasOwner[alias]; dup && owner != tool.ID {
				return fmt.Errorf("agent manifest: alias %q is claimed by both %q and %q", alias, owner, tool.ID)
			}
			aliasOwner[alias] = tool.ID
		}
	}
	toolIDs = map[string]struct{}{}
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
		if slices.Contains(t.Aliases, id) {
			return t, true
		}
	}
	return ToolSpec{}, false
}

func (a AgentSpec) IsPrimaryRole() bool {
	return a.Role == AgentRolePrimary || a.Role == AgentRoleOrchestrator
}
